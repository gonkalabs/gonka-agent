package agent

import (
	"strings"
	"sync"
	"time"
)

// FailoverReason classifies why a request to the inference endpoint failed.
// Typed classification (vs string matching) lets each reason carry its own
// retry strategy — e.g. 429 triggers key rotation, 401 stops retrying entirely.
type FailoverReason int

const (
	FailoverNone       FailoverReason = iota
	FailoverRateLimit                 // HTTP 429 — key exhausted, rotate
	FailoverOverloaded                // HTTP 502/503 — node busy, retry different
	FailoverTimeout                   // HTTP 504 / network timeout
	FailoverBadGateway                // HTTP 502
	FailoverEOF                       // EOF / connection reset
	FailoverAuth                      // HTTP 401/403 — key invalid, skip key
)

func (r FailoverReason) String() string {
	switch r {
	case FailoverRateLimit:
		return "rate_limit"
	case FailoverOverloaded:
		return "overloaded"
	case FailoverTimeout:
		return "timeout"
	case FailoverBadGateway:
		return "bad_gateway"
	case FailoverEOF:
		return "eof"
	case FailoverAuth:
		return "auth"
	}
	return "none"
}

// classifyError inspects the HTTP status code and error message to determine
// which FailoverReason applies.  statusCode=0 means the request never reached
// the server (network error).
func classifyError(err error, statusCode int) FailoverReason {
	switch statusCode {
	case 429:
		return FailoverRateLimit
	case 401, 403:
		return FailoverAuth
	case 502:
		return FailoverBadGateway
	case 503:
		return FailoverOverloaded
	case 504:
		return FailoverTimeout
	}
	if statusCode >= 500 {
		return FailoverOverloaded
	}
	if err == nil {
		return FailoverNone
	}
	msg := err.Error()
	if strings.Contains(msg, "timeout") || strings.Contains(msg, "deadline") || strings.Contains(msg, "HTTP 504") {
		return FailoverTimeout
	}
	if strings.Contains(msg, "EOF") || strings.Contains(msg, "connection reset") || strings.Contains(msg, "broken pipe") {
		return FailoverEOF
	}
	if strings.Contains(msg, "HTTP 429") || strings.Contains(strings.ToLower(msg), "rate limit") {
		return FailoverRateLimit
	}
	if strings.Contains(msg, "HTTP 5") {
		return FailoverOverloaded
	}
	return FailoverNone
}

// isTransient returns true when the failure is temporary and the request
// should be retried (possibly with a different key).
func (r FailoverReason) isTransient() bool {
	switch r {
	case FailoverRateLimit, FailoverOverloaded, FailoverTimeout, FailoverBadGateway, FailoverEOF:
		return true
	}
	return false
}

// ─── key pool ─────────────────────────────────────────────────────────────────

// keyState tracks one API key and its cooldown state.
type keyState struct {
	key       string
	coolUntil time.Time // zero = available
}

// keyPool manages a set of API keys with per-key cooldowns.
// When a key receives a 429 it is cooled for cooldownDur; the pool
// automatically moves to the next available key.
type keyPool struct {
	mu          sync.Mutex
	keys        []keyState
	idx         int
	cooldownDur time.Duration
}

const defaultCooldown = 60 * time.Second

func newKeyPool(keys []string) *keyPool {
	if len(keys) == 0 {
		return &keyPool{cooldownDur: defaultCooldown}
	}
	ks := make([]keyState, len(keys))
	for i, k := range keys {
		ks[i] = keyState{key: k}
	}
	return &keyPool{keys: ks, cooldownDur: defaultCooldown}
}

// current returns the active API key.
func (p *keyPool) current() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.keys) == 0 {
		return ""
	}
	return p.keys[p.idx%len(p.keys)].key
}

// markCooling marks the current key as rate-limited and advances to the
// next available key.  If all keys are cooling it waits for the earliest
// one to recover (bounded wait, never infinite).
func (p *keyPool) markCooling(reason FailoverReason) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.keys) == 0 {
		return
	}
	now := time.Now()
	cur := &p.keys[p.idx%len(p.keys)]

	switch reason {
	case FailoverRateLimit:
		cur.coolUntil = now.Add(p.cooldownDur)
	case FailoverAuth:
		// Auth failures cool much longer — key is likely invalid.
		cur.coolUntil = now.Add(24 * time.Hour)
	default:
		cur.coolUntil = now.Add(10 * time.Second)
	}

	// Advance to next available key.
	for i := 1; i <= len(p.keys); i++ {
		next := (p.idx + i) % len(p.keys)
		if p.keys[next].coolUntil.IsZero() || now.After(p.keys[next].coolUntil) {
			p.idx = next
			return
		}
	}
	// All keys cooling: wait until the earliest one recovers, then use it.
	earliest := p.keys[0].coolUntil
	earliestIdx := 0
	for i, k := range p.keys {
		if k.coolUntil.Before(earliest) {
			earliest = k.coolUntil
			earliestIdx = i
		}
	}
	wait := time.Until(earliest)
	if wait > 0 {
		p.mu.Unlock()
		time.Sleep(wait)
		p.mu.Lock()
	}
	p.idx = earliestIdx
	p.keys[earliestIdx].coolUntil = time.Time{}
}
