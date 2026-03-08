// Package semcache provides a local semantic task cache for gonka-agent.
//
// Each completed task is embedded via Gonka /v1/embeddings and stored on disk.
// On subsequent tasks the cache is queried first:
//
//	score ≥ 0.95  → FULL HIT:    inject as pre-solved context; agent verifies.
//	score 0.75–0.94 → PARTIAL HIT: inject previous steps as context; agent continues.
//	score < 0.75  → MISS:        solve from scratch, store on success.
//
// Storage: ~/.gonka-cache/semcache.json (JSON, plain text — no external DB).
// Max entries: configurable via SEMCACHE_MAX_ENTRIES (default 1000).
// Quality scoring: each entry tracks a quality score (0–1) updated from
// X-Inference-Feedback outcomes. Entries below 0.3 are excluded from lookup.
package semcache

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

const (
	FullHitThreshold    = 0.95
	PartialHitThreshold = 0.75
	MinQuality          = 0.30
	DefaultMaxEntries   = 1000
	DefaultTTLDays      = 30
)

// HitKind describes the type of cache lookup result.
type HitKind int

const (
	Miss    HitKind = iota
	Partial         // inject steps as context
	Full            // return directly (agent verifies)
)

// Step mirrors agent.Step for serialization without an import cycle.
type Step struct {
	ToolName  string `json:"tool_name"`
	ArgsJSON  string `json:"args_json"`
	Result    string `json:"result"`
	IsError   bool   `json:"is_error"`
	ElapsedMs int64  `json:"elapsed_ms"`
}

// Entry is one cached task solution.
type Entry struct {
	Task      string    `json:"task"`
	Vec       []float64 `json:"vec"`
	ModelID   string    `json:"model_id"` // embedding model; mismatch → skip
	Steps     []Step    `json:"steps"`
	Answer    string    `json:"answer"`
	Quality   float64   `json:"quality"`   // 0–1; updated from feedback
	CreatedAt time.Time `json:"created_at"`
}

// LookupResult is returned by Lookup.
type LookupResult struct {
	Kind    HitKind
	Score   float64
	Context string  // human-readable context for injection into system prompt
	Entry   *Entry
}

// Cache is the semantic task cache.
type Cache struct {
	mu         sync.RWMutex
	entries    []*Entry
	path       string
	apiURL     string
	apiKey     string
	modelID    string
	maxEntries int
	httpClient *http.Client
}

// Config holds cache configuration.
type Config struct {
	// CacheDir is the directory for semcache.json (default ~/.gonka-cache).
	CacheDir string
	// APIBaseURL is the Gonka inference base URL for /v1/embeddings.
	APIBaseURL string
	// APIKey is the Gonka API key.
	APIKey string
	// EmbedModel is the embedding model name.
	EmbedModel string
	// MaxEntries is the maximum number of cached entries (default 1000).
	MaxEntries int
}

// New creates and loads a Cache from disk.
func New(cfg Config) (*Cache, error) {
	dir := cfg.CacheDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("semcache: home dir: %w", err)
		}
		dir = filepath.Join(home, ".gonka-cache")
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("semcache: mkdir: %w", err)
	}
	max := cfg.MaxEntries
	if max <= 0 {
		if v := os.Getenv("SEMCACHE_MAX_ENTRIES"); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				max = n
			}
		}
		if max <= 0 {
			max = DefaultMaxEntries
		}
	}
	model := cfg.EmbedModel
	if model == "" {
		model = "Qwen/Qwen3-235B-A22B-Instruct-2507-FP8"
	}
	c := &Cache{
		path:       filepath.Join(dir, "semcache.json"),
		apiURL:     cfg.APIBaseURL,
		apiKey:     cfg.APIKey,
		modelID:    model,
		maxEntries: max,
		httpClient: &http.Client{Timeout: 60 * time.Second},
	}
	_ = c.load() // non-fatal: start with empty cache if file missing
	return c, nil
}

// Lookup embeds the task and searches the cache.
// Returns Miss if embeddings are unavailable (graceful degradation).
func (c *Cache) Lookup(task string) LookupResult {
	vec, err := c.embed(task)
	if err != nil {
		return LookupResult{Kind: Miss}
	}

	c.mu.RLock()
	defer c.mu.RUnlock()

	best := LookupResult{Kind: Miss}
	for _, e := range c.entries {
		if e.Quality < MinQuality {
			continue
		}
		if e.ModelID != c.modelID {
			// Embedding space mismatch — skip rather than compare incompatible vectors.
			continue
		}
		if time.Since(e.CreatedAt).Hours() > float64(DefaultTTLDays*24) {
			continue
		}
		score := cosine(vec, e.Vec)
		if score > best.Score {
			best.Score = score
			best.Entry = e
		}
	}

	if best.Entry == nil || best.Score < PartialHitThreshold {
		return LookupResult{Kind: Miss}
	}
	if best.Score >= FullHitThreshold {
		best.Kind = Full
		best.Context = buildContext(best.Entry, best.Score, Full)
	} else {
		best.Kind = Partial
		best.Context = buildContext(best.Entry, best.Score, Partial)
	}
	return best
}

// Store embeds the task and saves the entry to disk.
// Called after a successful agent run.
func (c *Cache) Store(task string, steps []Step, answer string) {
	vec, err := c.embed(task)
	if err != nil {
		return // silently skip — cache failure must not block agent
	}
	entry := &Entry{
		Task:      task,
		Vec:       vec,
		ModelID:   c.modelID,
		Steps:     steps,
		Answer:    answer,
		Quality:   0.7, // initial quality; adjusted by feedback
		CreatedAt: time.Now(),
	}
	c.mu.Lock()
	c.entries = append(c.entries, entry)
	c.evict()
	entries := c.entries
	c.mu.Unlock()
	_ = c.save(entries)
}

// UpdateQuality adjusts the quality score of the most recently stored entry
// based on feedback outcome ("resolved" → +0.1, "unresolved" → −0.2).
func (c *Cache) UpdateQuality(outcome string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) == 0 {
		return
	}
	e := c.entries[len(c.entries)-1]
	switch outcome {
	case "resolved":
		e.Quality = math.Min(1.0, e.Quality+0.1)
	case "unresolved":
		e.Quality = math.Max(0.0, e.Quality-0.2)
	}
	_ = c.save(c.entries)
}

// ─── internal ─────────────────────────────────────────────────────────────────

func (c *Cache) embed(text string) ([]float64, error) {
	if c.apiURL == "" || c.apiKey == "" {
		return nil, fmt.Errorf("semcache: embed API not configured")
	}
	body, _ := json.Marshal(map[string]any{"model": c.modelID, "input": text})
	req, err := http.NewRequest("POST", c.apiURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("semcache: embed %d: %s", resp.StatusCode, string(raw))
	}
	var res struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return nil, err
	}
	if len(res.Data) == 0 || len(res.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("semcache: empty embedding")
	}
	return res.Data[0].Embedding, nil
}

func cosine(a, b []float64) float64 {
	if len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += a[i] * b[i]
		na += a[i] * a[i]
		nb += b[i] * b[i]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

func buildContext(e *Entry, score float64, kind HitKind) string {
	var b bytes.Buffer
	if kind == Full {
		fmt.Fprintf(&b, "CACHE: Similar task solved before (similarity %.2f).\n", score)
		fmt.Fprintf(&b, "Previous task: %s\n\n", e.Task)
		fmt.Fprintf(&b, "Previous answer:\n%s\n\n", e.Answer)
		fmt.Fprintf(&b, "Verify this answer applies to the current task. Adapt if needed.")
	} else {
		fmt.Fprintf(&b, "CONTEXT: A similar task was solved before (similarity %.2f).\n", score)
		fmt.Fprintf(&b, "Previous task: %s\n\n", e.Task)
		fmt.Fprintf(&b, "Approach used (adapt for current task):\n")
		for i, s := range e.Steps {
			if i >= 8 {
				fmt.Fprintf(&b, "  ... (%d more steps)\n", len(e.Steps)-8)
				break
			}
			if !s.IsError {
				fmt.Fprintf(&b, "  %d. %s(%s)\n", i+1, s.ToolName, truncate(s.ArgsJSON, 80))
			}
		}
		fmt.Fprintf(&b, "\nDo not repeat these steps if they already achieve the goal.")
	}
	return b.String()
}

// evict removes low-quality and expired entries to stay within maxEntries.
// Must be called with c.mu held (write lock).
func (c *Cache) evict() {
	if len(c.entries) <= c.maxEntries {
		return
	}
	cutoff := time.Now().Add(-time.Duration(DefaultTTLDays) * 24 * time.Hour)
	var live []*Entry
	for _, e := range c.entries {
		if e.CreatedAt.After(cutoff) && e.Quality >= MinQuality {
			live = append(live, e)
		}
	}
	// If still over limit, sort by quality*recency and trim.
	if len(live) > c.maxEntries {
		sort.Slice(live, func(i, j int) bool {
			ri := live[i].Quality * recency(live[i].CreatedAt)
			rj := live[j].Quality * recency(live[j].CreatedAt)
			return ri > rj
		})
		live = live[:c.maxEntries]
	}
	c.entries = live
}

func recency(t time.Time) float64 {
	days := time.Since(t).Hours() / 24
	return math.Exp(-days / 30)
}

func (c *Cache) load() error {
	data, err := os.ReadFile(c.path)
	if err != nil {
		return err
	}
	var entries []*Entry
	if err := json.Unmarshal(data, &entries); err != nil {
		return err
	}
	c.mu.Lock()
	c.entries = entries
	c.mu.Unlock()
	return nil
}

func (c *Cache) save(entries []*Entry) error {
	data, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, c.path) // atomic on Linux
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
