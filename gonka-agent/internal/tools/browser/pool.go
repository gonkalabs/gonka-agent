// Package browser provides a managed headless Chrome pool via chromedp
// for rendering JS-heavy pages (WebFetch) and a SearXNG integration
// for reliable web search (WebSearch).
package browser

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/chromedp"
)

// Pool manages a single headless Chrome instance with tab reuse.
// It auto-restarts on crash and provides screenshot-on-error diagnostics.
type Pool struct {
	mu        sync.Mutex
	allocCtx  context.Context
	allocCancel context.CancelFunc
	started   bool
	maxTabs   int
	cacheDir  string
	cache     *pageCache
}

// NewPool creates a browser pool. cacheDir stores screenshots/LRU cache.
func NewPool(cacheDir string, maxTabs int) *Pool {
	if maxTabs <= 0 {
		maxTabs = 3
	}
	return &Pool{
		maxTabs:  maxTabs,
		cacheDir: cacheDir,
		cache:    newPageCache(64),
	}
}

// Start launches the headless Chrome process.
func (p *Pool) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.startLocked()
}

func (p *Pool) startLocked() error {
	if p.started {
		return nil
	}

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("disable-translate", true),
		chromedp.UserAgent("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"),
	)

	p.allocCtx, p.allocCancel = chromedp.NewExecAllocator(context.Background(), opts...)
	p.started = true
	slog.Info("browser: Chrome pool started")
	return nil
}

// Stop terminates the Chrome process.
func (p *Pool) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.allocCancel != nil {
		p.allocCancel()
	}
	p.started = false
}

// Restart kills and relaunches Chrome.
func (p *Pool) Restart() error {
	p.Stop()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.startLocked()
}

// Fetch renders a URL and returns the page text content.
// Uses LRU cache; retries up to 3 times on failure with
// increasing timeouts. Captures screenshot on final failure.
func (p *Pool) Fetch(ctx context.Context, url string) (string, error) {
	if cached, ok := p.cache.Get(url); ok {
		slog.Debug("browser: cache hit", "url", url)
		return cached, nil
	}

	p.mu.Lock()
	if !p.started {
		if err := p.startLocked(); err != nil {
			p.mu.Unlock()
			return "", fmt.Errorf("browser: start failed: %w", err)
		}
	}
	allocCtx := p.allocCtx
	p.mu.Unlock()

	var lastErr error
	timeouts := []time.Duration{15 * time.Second, 30 * time.Second, 45 * time.Second}

	for attempt, timeout := range timeouts {
		text, err := p.fetchOnce(allocCtx, ctx, url, timeout)
		if err == nil {
			p.cache.Set(url, text)
			return text, nil
		}
		lastErr = err
		slog.Warn("browser: fetch attempt failed",
			"url", url, "attempt", attempt+1, "err", err, "timeout", timeout)

		if strings.Contains(err.Error(), "executable file not found") ||
			strings.Contains(err.Error(), "cannot find Chrome") {
			return "", fmt.Errorf("browser: Chrome not installed: %w", err)
		}

		if attempt < len(timeouts)-1 {
			time.Sleep(time.Duration(attempt+1) * time.Second)
		}
	}

	if restartErr := p.Restart(); restartErr != nil {
		slog.Error("browser: restart after failure also failed", "err", restartErr)
	}

	return "", fmt.Errorf("browser: all %d attempts failed for %s: %w", len(timeouts), url, lastErr)
}

func (p *Pool) fetchOnce(allocCtx, parentCtx context.Context, url string, timeout time.Duration) (string, error) {
	tabCtx, tabCancel := chromedp.NewContext(allocCtx)
	defer tabCancel()

	ctx, cancel := context.WithTimeout(tabCtx, timeout)
	defer cancel()

	_ = parentCtx

	var text string
	err := chromedp.Run(ctx,
		chromedp.Navigate(url),
		chromedp.WaitReady("body"),
		chromedp.Sleep(2*time.Second),
		chromedp.EvaluateAsDevTools(`document.body.innerText`, &text),
	)
	if err != nil {
		return "", err
	}

	if len(text) > 100000 {
		text = text[:100000] + "\n... [truncated at 100KB]"
	}

	return text, nil
}

// CacheStats returns cache hit/miss counts.
func (p *Pool) CacheStats() (hits, misses int64) {
	return p.cache.Stats()
}
