package inference

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Router tries providers in priority order, skipping those with open
// circuit breakers. It tracks latency and token usage per provider.
type Router struct {
	providers []Provider
	breakers  map[string]*Breaker
	metrics   map[string]*Metrics
	active    atomic.Value // string: name of last successful provider

	mu sync.RWMutex
}

// NewRouter creates a router from an ordered list of providers.
// First provider has highest priority.
func NewRouter(providers []Provider) *Router {
	r := &Router{
		providers: providers,
		breakers:  make(map[string]*Breaker, len(providers)),
		metrics:   make(map[string]*Metrics, len(providers)),
	}
	for _, p := range providers {
		r.breakers[p.Name()] = newBreaker(3, 60*time.Second)
		r.metrics[p.Name()] = &Metrics{}
	}
	return r
}

// Chat sends a request to the highest-priority healthy provider.
// If it fails, tries the next one. Returns the first success or
// an error if all providers are exhausted.
func (r *Router) Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	var lastErr error
	for _, p := range r.providers {
		brk := r.breakers[p.Name()]
		if !brk.Allow() {
			slog.Debug("inference: skipping provider (breaker open)", "provider", p.Name())
			continue
		}

		t0 := time.Now()
		resp, err := p.Chat(ctx, req)
		elapsed := time.Since(t0)

		m := r.metrics[p.Name()]
		m.Calls++
		m.TotalMs += elapsed.Milliseconds()
		m.LastLatency = elapsed

		if err != nil {
			m.Failures++
			brk.RecordFailure()
			lastErr = fmt.Errorf("%s: %w", p.Name(), err)
			slog.Warn("inference: provider failed, trying next",
				"provider", p.Name(), "err", err, "elapsed", elapsed)
			continue
		}

		brk.RecordSuccess()
		r.active.Store(p.Name())
		resp.Provider = p.Name()
		return resp, nil
	}
	if lastErr != nil {
		return nil, fmt.Errorf("all providers failed, last: %w", lastErr)
	}
	return nil, fmt.Errorf("no inference providers configured")
}

// Active returns the name of the last provider that answered successfully.
func (r *Router) Active() string {
	v := r.active.Load()
	if v == nil {
		return ""
	}
	return v.(string)
}

// ProbeAll pings every provider concurrently and opens breakers for
// unreachable ones. Call at startup for fast first request.
func (r *Router) ProbeAll(ctx context.Context) map[string]error {
	results := make(map[string]error, len(r.providers))
	var mu sync.Mutex
	var wg sync.WaitGroup

	for _, p := range r.providers {
		wg.Add(1)
		go func(p Provider) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			err := p.Ping(pctx)
			mu.Lock()
			results[p.Name()] = err
			mu.Unlock()
			if err != nil {
				r.breakers[p.Name()].RecordFailure()
				r.breakers[p.Name()].RecordFailure()
				r.breakers[p.Name()].RecordFailure()
			}
		}(p)
	}
	wg.Wait()
	return results
}

// ProviderStatus returns health info for each provider.
func (r *Router) ProviderStatus() []ProviderHealth {
	out := make([]ProviderHealth, 0, len(r.providers))
	for _, p := range r.providers {
		brk := r.breakers[p.Name()]
		m := r.metrics[p.Name()]
		out = append(out, ProviderHealth{
			Name:        p.Name(),
			BreakerState: brk.State(),
			Calls:       m.Calls,
			Failures:    m.Failures,
			AvgLatencyMs: func() int64 {
				if m.Calls == 0 {
					return 0
				}
				return m.TotalMs / m.Calls
			}(),
		})
	}
	return out
}

type ProviderHealth struct {
	Name         string
	BreakerState string
	Calls        int64
	Failures     int64
	AvgLatencyMs int64
}

// TokenSummary returns combined token usage across all providers.
func (r *Router) TokenSummary() (calls, promptTokens, completionTokens int64) {
	for _, m := range r.metrics {
		calls += m.Calls
	}
	return
}
