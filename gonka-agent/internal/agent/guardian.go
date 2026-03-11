package agent

import (
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Guardian protects the agent from adversarial input (spam, toxic content).
// It tracks a session health score; when score drops below threshold,
// the agent enters a locked state until explicitly reset via --new flag.
type Guardian struct {
	mu            sync.Mutex
	healthScore   float64
	messageCount  int
	spamCount     int
	lastMsgTime   time.Time
	locked        bool
	lockReason    string
	threshold     float64
	cooldownUntil time.Time
}

// NewGuardian creates a guardian with default threshold.
func NewGuardian() *Guardian {
	return &Guardian{
		healthScore: 100.0,
		threshold:   20.0,
	}
}

// Check evaluates incoming user input and returns whether it should be processed.
// Returns (allowed bool, reason string).
func (g *Guardian) Check(input string) (bool, string) {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.locked {
		return false, g.lockReason
	}

	if !g.cooldownUntil.IsZero() && time.Now().Before(g.cooldownUntil) {
		return false, "session cooling down, please wait"
	}

	g.messageCount++
	now := time.Now()
	penalties := 0.0

	// Rapid-fire detection: messages faster than 1/sec
	if !g.lastMsgTime.IsZero() && now.Sub(g.lastMsgTime) < time.Second {
		penalties += 10.0
		g.spamCount++
	}
	g.lastMsgTime = now

	// Empty or near-empty messages
	trimmed := strings.TrimSpace(input)
	if utf8.RuneCountInString(trimmed) < 3 {
		penalties += 5.0
	}

	// Repeated characters (e.g. "aaaaaaa", "!!!!!!!")
	if hasRepeatedChars(trimmed, 8) {
		penalties += 8.0
	}

	// Excessive length without structure
	if utf8.RuneCountInString(trimmed) > 5000 && strings.Count(trimmed, "\n") < 3 {
		penalties += 15.0
	}

	// Consecutive spam messages
	if g.spamCount >= 5 {
		penalties += 20.0
	}

	// Apply penalties
	g.healthScore -= penalties

	// Recovery: slow natural increase
	if penalties == 0 && g.healthScore < 100 {
		g.healthScore += 2.0
		if g.healthScore > 100 {
			g.healthScore = 100
		}
	}

	// Check threshold
	if g.healthScore <= g.threshold {
		g.locked = true
		g.lockReason = "session locked: adversarial input detected. Use `gonka --new` to start fresh."
		return false, g.lockReason
	}

	// Soft cooldown at low health
	if g.healthScore < 40 {
		g.cooldownUntil = now.Add(3 * time.Second)
	}

	return true, ""
}

// Reset unlocks the guardian and restores health (called by --new).
func (g *Guardian) Reset() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.healthScore = 100.0
	g.messageCount = 0
	g.spamCount = 0
	g.locked = false
	g.lockReason = ""
	g.cooldownUntil = time.Time{}
}

// Health returns the current session health score (0-100).
func (g *Guardian) Health() float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.healthScore
}

// IsLocked reports whether the session is in locked state.
func (g *Guardian) IsLocked() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.locked
}

func hasRepeatedChars(s string, threshold int) bool {
	if len(s) < threshold {
		return false
	}
	var prev rune
	count := 0
	for _, r := range s {
		if r == prev {
			count++
			if count >= threshold {
				return true
			}
		} else {
			prev = r
			count = 1
		}
	}
	return false
}
