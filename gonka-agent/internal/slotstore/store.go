// Package slotstore implements the binary singularity PatternSlot store.
//
// A PatternSlot is a compact record of a proven solution pattern:
// an embedding vector (384 dims, all-MiniLM-L6-v2), the task summary,
// a solution sketch, and a quality score.
//
// The store loads/saves slots from disk, performs CPU cosine similarity
// search to find relevant prior solutions, and distills new slots from
// successful task completions.
//
// Integration flow (in cmd/gonka/main.go):
//   1. Open(cfg) — loads slots from disk, ingests BS_RAW_INPUT if set
//   2. Search(task) — before running, check if a matching slot exists
//   3. Distill(task, solution) — after success, create a new slot
//   4. Close() — persist to disk
package slotstore

import (
	"bytes"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Slot struct {
	ID        string    `json:"id"`
	Task      string    `json:"task"`
	Solution  string    `json:"solution"`
	Vec       []float32 `json:"vec"`
	Quality   float32   `json:"quality"`
	UseCount  int       `json:"use_count"`
	CreatedAt time.Time `json:"created_at"`
}

type SearchResult struct {
	Slot       Slot
	Similarity float32
}

type Config struct {
	SlotDir      string
	EmbedURL     string
	ChunkLines   int
	MinSimBps    int
	RawInputPath string
}

type Store struct {
	cfg   Config
	slots []Slot
	dirty bool
}

func Open(cfg Config) (*Store, error) {
	if cfg.SlotDir == "" {
		home, _ := os.UserHomeDir()
		cfg.SlotDir = filepath.Join(home, ".gonka-cache", "slots")
	}
	if cfg.ChunkLines <= 0 {
		cfg.ChunkLines = 50
	}
	if cfg.MinSimBps <= 0 {
		cfg.MinSimBps = 7500
	}

	_ = os.MkdirAll(cfg.SlotDir, 0755)
	s := &Store{cfg: cfg}

	if err := s.load(); err != nil {
		return s, nil
	}

	if cfg.RawInputPath != "" {
		if err := s.IngestFile(cfg.RawInputPath); err != nil {
			return s, fmt.Errorf("slotstore: ingest %s: %w", cfg.RawInputPath, err)
		}
	}

	return s, nil
}

// Search finds slots with cosine similarity above MinSimBps.
func (s *Store) Search(task string) []SearchResult {
	if len(s.slots) == 0 || s.cfg.EmbedURL == "" {
		return nil
	}

	qv, err := s.embed(task)
	if err != nil || len(qv) == 0 {
		return nil
	}

	threshold := float32(s.cfg.MinSimBps) / 10000.0
	var results []SearchResult

	for i := range s.slots {
		sim := cosine(s.slots[i].Vec, qv)
		if sim >= threshold {
			results = append(results, SearchResult{
				Slot:       s.slots[i],
				Similarity: sim,
			})
		}
	}

	// Sort descending.
	for i := 0; i < len(results); i++ {
		for j := i + 1; j < len(results); j++ {
			if results[j].Similarity > results[i].Similarity {
				results[i], results[j] = results[j], results[i]
			}
		}
	}

	if len(results) > 5 {
		results = results[:5]
	}

	for i := range results {
		for j := range s.slots {
			if s.slots[j].ID == results[i].Slot.ID {
				s.slots[j].UseCount++
				s.dirty = true
				break
			}
		}
	}

	return results
}

// Distill creates a new slot from a successful task completion.
func (s *Store) Distill(task, solution string, quality float32) error {
	if s.cfg.EmbedURL == "" {
		return nil
	}

	vec, err := s.embed(task)
	if err != nil {
		return err
	}

	slot := Slot{
		ID:        fmt.Sprintf("slot-%d", time.Now().UnixNano()),
		Task:      truncate(task, 500),
		Solution:  truncate(solution, 2000),
		Vec:       vec,
		Quality:   quality,
		CreatedAt: time.Now(),
	}

	s.slots = append(s.slots, slot)
	s.dirty = true
	return nil
}

// Count returns the number of stored slots.
func (s *Store) Count() int {
	return len(s.slots)
}

// Close persists slots to disk if changed.
func (s *Store) Close() error {
	if !s.dirty {
		return nil
	}
	return s.save()
}

// IngestFile reads a file, splits into chunks, embeds each, and creates slots.
func (s *Store) IngestFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	lines := strings.Split(string(data), "\n")
	chunkSize := s.cfg.ChunkLines

	for i := 0; i < len(lines); i += chunkSize {
		end := i + chunkSize
		if end > len(lines) {
			end = len(lines)
		}
		chunk := strings.Join(lines[i:end], "\n")
		chunk = strings.TrimSpace(chunk)
		if chunk == "" {
			continue
		}

		vec, err := s.embed(chunk)
		if err != nil {
			continue
		}

		slot := Slot{
			ID:        fmt.Sprintf("raw-%d-%d", time.Now().UnixNano(), i),
			Task:      truncate(chunk, 200),
			Solution:  chunk,
			Vec:       vec,
			Quality:   0.5,
			CreatedAt: time.Now(),
		}
		s.slots = append(s.slots, slot)
		s.dirty = true
	}

	return nil
}

// FormatContext builds an injection string from search results for the system prompt.
func FormatContext(results []SearchResult) string {
	if len(results) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n[Binary Singularity — matching PatternSlots]\n")
	for i, r := range results {
		sb.WriteString(fmt.Sprintf("\n--- slot %d (sim=%.2f, quality=%.2f, used=%d) ---\n",
			i+1, r.Similarity, r.Slot.Quality, r.Slot.UseCount))
		sb.WriteString("Task: " + r.Slot.Task + "\n")
		sol := r.Slot.Solution
		if len(sol) > 800 {
			sol = sol[:800] + "..."
		}
		sb.WriteString("Solution: " + sol + "\n")
	}
	return sb.String()
}

func (s *Store) load() error {
	path := filepath.Join(s.cfg.SlotDir, "pattern_slots.gob")
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return gob.NewDecoder(f).Decode(&s.slots)
}

func (s *Store) save() error {
	path := filepath.Join(s.cfg.SlotDir, "pattern_slots.gob")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return gob.NewEncoder(f).Encode(s.slots)
}

type embedReq struct {
	Input string `json:"input"`
	Model string `json:"model"`
}

type embedResp struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
}

func (s *Store) embed(text string) ([]float32, error) {
	body, _ := json.Marshal(embedReq{
		Input: text,
		Model: "all-MiniLM-L6-v2",
	})

	url := strings.TrimRight(s.cfg.EmbedURL, "/")
	if !strings.HasSuffix(url, "/v1/embeddings") && !strings.HasSuffix(url, "/embeddings") {
		url += "/v1/embeddings"
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("embed: HTTP %d: %s", resp.StatusCode, string(raw))
	}

	var r embedResp
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, err
	}
	if len(r.Data) == 0 || len(r.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("embed: empty response")
	}
	return r.Data[0].Embedding, nil
}

func cosine(a, b []float32) float32 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	denom := math.Sqrt(normA) * math.Sqrt(normB)
	if denom == 0 {
		return 0
	}
	sim := dot / denom
	if sim < 0 {
		return 0
	}
	return float32(sim)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
