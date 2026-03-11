package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gonkalabs/gonka-agent/internal/inference"
)

// Gonka provider wraps the existing opengnk proxy behavior:
// key pool rotation, X-Inference-Feedback header, Gonka-specific error handling.
type Gonka struct {
	baseURL string
	model   string
	client  *http.Client
	pool    *gonkaKeyPool
	feedbackMu      sync.Mutex
	pendingFeedback string
}

func NewGonka(baseURL, model string, keys []string) *Gonka {
	if baseURL == "" {
		baseURL = "http://localhost:8090/v1"
	}
	return &Gonka{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		client:  &http.Client{Timeout: 240 * time.Second},
		pool:    newGonkaKeyPool(keys),
	}
}

func (g *Gonka) Name() string { return "gonka" }

func (g *Gonka) SetFeedback(outcome string) {
	g.feedbackMu.Lock()
	g.pendingFeedback = `{"outcome":"` + outcome + `"}`
	g.feedbackMu.Unlock()
}

func (g *Gonka) Chat(ctx context.Context, req inference.ChatRequest) (*inference.ChatResponse, error) {
	if req.Model == "" {
		req.Model = g.model
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	apiKey := g.pool.current()
	httpReq, err := http.NewRequestWithContext(ctx, "POST", g.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+apiKey)
	}

	g.feedbackMu.Lock()
	if g.pendingFeedback != "" {
		httpReq.Header.Set("X-Inference-Feedback", g.pendingFeedback)
		g.pendingFeedback = ""
	}
	g.feedbackMu.Unlock()

	resp, err := g.client.Do(httpReq)
	if err != nil {
		g.pool.markCooling(err, 0)
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 400 {
		g.pool.markCooling(nil, resp.StatusCode)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, truncate(string(raw), 200))
	}

	var r inference.ChatResponse
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if len(r.Error) > 0 && string(r.Error) != "null" {
		return nil, fmt.Errorf("API error: %s", string(r.Error))
	}
	if len(r.Choices) == 0 {
		return nil, fmt.Errorf("empty choices")
	}
	return &r, nil
}

func (g *Gonka) Ping(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, "GET", g.baseURL+"/status", nil)
	if err != nil {
		return err
	}
	resp, err := g.client.Do(httpReq)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		httpReq2, _ := http.NewRequestWithContext(ctx, "GET", g.baseURL+"/models", nil)
		resp2, err2 := g.client.Do(httpReq2)
		if err2 != nil {
			return err2
		}
		resp2.Body.Close()
		if resp2.StatusCode >= 400 {
			return fmt.Errorf("ping: HTTP %d", resp2.StatusCode)
		}
	}
	return nil
}

// gonkaKeyPool is a simplified key pool for Gonka's multi-key rotation.
type gonkaKeyPool struct {
	mu          sync.Mutex
	keys        []gonkaKeyState
	idx         int
	cooldownDur time.Duration
}

type gonkaKeyState struct {
	key       string
	coolUntil time.Time
}

func newGonkaKeyPool(keys []string) *gonkaKeyPool {
	ks := make([]gonkaKeyState, len(keys))
	for i, k := range keys {
		ks[i] = gonkaKeyState{key: k}
	}
	return &gonkaKeyPool{keys: ks, cooldownDur: 60 * time.Second}
}

func (p *gonkaKeyPool) current() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.keys) == 0 {
		return ""
	}
	return p.keys[p.idx%len(p.keys)].key
}

func (p *gonkaKeyPool) markCooling(_ error, statusCode int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.keys) == 0 {
		return
	}
	now := time.Now()
	cur := &p.keys[p.idx%len(p.keys)]

	switch {
	case statusCode == 429:
		cur.coolUntil = now.Add(p.cooldownDur)
	case statusCode == 401 || statusCode == 403:
		cur.coolUntil = now.Add(24 * time.Hour)
	default:
		cur.coolUntil = now.Add(10 * time.Second)
	}

	for i := 1; i <= len(p.keys); i++ {
		next := (p.idx + i) % len(p.keys)
		if p.keys[next].coolUntil.IsZero() || now.After(p.keys[next].coolUntil) {
			p.idx = next
			return
		}
	}
}
