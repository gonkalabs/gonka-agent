package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"
)

// SearchResult represents one search hit from SearXNG.
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
	Engine  string `json:"engine"`
}

// Searcher wraps SearXNG's JSON API for reliable web search.
type Searcher struct {
	baseURL string
	client  *http.Client
}

// NewSearcher creates a Searcher pointing at a SearXNG instance.
func NewSearcher(baseURL string) *Searcher {
	if baseURL == "" {
		baseURL = "http://localhost:8888"
	}
	return &Searcher{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Search queries SearXNG and returns up to `limit` results.
// Retries once on failure.
func (s *Searcher) Search(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 10
	}

	results, err := s.searchOnce(ctx, query, limit)
	if err != nil {
		slog.Warn("searxng: first attempt failed, retrying", "err", err)
		time.Sleep(2 * time.Second)
		results, err = s.searchOnce(ctx, query, limit)
	}
	return results, err
}

func (s *Searcher) searchOnce(ctx context.Context, query string, limit int) ([]SearchResult, error) {
	u := fmt.Sprintf("%s/search?q=%s&format=json&engines=google,duckduckgo,brave,bing",
		s.baseURL, url.QueryEscape(query))

	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return nil, err
	}

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("searxng: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("searxng: HTTP %d: %s", resp.StatusCode, truncateStr(string(body), 200))
	}

	var parsed struct {
		Results []SearchResult `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("searxng: decode: %w", err)
	}

	if len(parsed.Results) > limit {
		parsed.Results = parsed.Results[:limit]
	}
	return parsed.Results, nil
}

// Ping checks if SearXNG is reachable.
func (s *Searcher) Ping(ctx context.Context) error {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx2, "GET", s.baseURL+"/search?q=ping&format=json", nil)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("searxng: HTTP %d", resp.StatusCode)
	}
	return nil
}

// EnsureRunning checks if SearXNG Docker container is up; starts it if not.
func (s *Searcher) EnsureRunning(ctx context.Context) error {
	if err := s.Ping(ctx); err == nil {
		return nil
	}

	slog.Info("searxng: starting Docker container")
	cmd := exec.CommandContext(ctx,
		"docker", "run", "-d",
		"--name", "gonka-searxng",
		"-p", "8888:8080",
		"-e", "SEARXNG_SECRET=gonka-secret",
		"--restart", "unless-stopped",
		"searxng/searxng:latest",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "already in use") {
			// Container exists but may be stopped
			exec.CommandContext(ctx, "docker", "start", "gonka-searxng").Run()
			time.Sleep(3 * time.Second)
			return s.Ping(ctx)
		}
		return fmt.Errorf("searxng: docker start failed: %s: %w", string(out), err)
	}

	// Wait for it to be ready
	for i := 0; i < 10; i++ {
		time.Sleep(2 * time.Second)
		if err := s.Ping(ctx); err == nil {
			return nil
		}
	}
	return fmt.Errorf("searxng: started but not responsive after 20s")
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
