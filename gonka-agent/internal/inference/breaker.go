package inference

import (
	"sync"
	"time"
)

// Breaker implements a simple circuit breaker per provider.
// States: closed (normal) -> open (skip) -> half-open (probe).
type Breaker struct {
	mu         sync.Mutex
	failures   int
	threshold  int
	resetAfter time.Duration
	lastFail   time.Time
	state      breakerState
}

type breakerState int

const (
	breakerClosed   breakerState = iota // healthy, allow requests
	breakerOpen                         // too many failures, skip
	breakerHalfOpen                     // probing: allow one request
)

func newBreaker(threshold int, resetAfter time.Duration) *Breaker {
	return &Breaker{
		threshold:  threshold,
		resetAfter: resetAfter,
		state:      breakerClosed,
	}
}

// Allow returns true if a request should be attempted.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case breakerClosed:
		return true
	case breakerOpen:
		if time.Since(b.lastFail) >= b.resetAfter {
			b.state = breakerHalfOpen
			return true
		}
		return false
	case breakerHalfOpen:
		return true
	}
	return true
}

func (b *Breaker) RecordSuccess() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.state = breakerClosed
}

func (b *Breaker) RecordFailure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures++
	b.lastFail = time.Now()
	if b.failures >= b.threshold {
		b.state = breakerOpen
	}
}

func (b *Breaker) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case breakerClosed:
		return "closed"
	case breakerOpen:
		return "open"
	case breakerHalfOpen:
		return "half-open"
	}
	return "unknown"
}
