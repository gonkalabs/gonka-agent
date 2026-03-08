// Package loopdetect detects when an agent is stuck in a repetitive tool-call loop.
//
// Three detectors (ported from openclaw/src/agents/tool-loop-detection.ts):
//
//	generic_repeat      — same tool+args called ≥ WARNING times
//	ping_pong           — alternating between two patterns with identical results
//	poll_no_progress    — polling tool returns identical result ≥ WARNING times
//
// Thresholds: warning=10, critical=20, circuit_breaker=30.
// At warning: a message is injected into the conversation.
// At critical: RunResult.Err is set and the loop terminates.
package loopdetect

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
)

const (
	WarningThreshold        = 10
	CriticalThreshold       = 20
	CircuitBreakerThreshold = 30
	HistorySize             = 30
)

// Kind identifies which detector fired.
type Kind string

const (
	KindGenericRepeat   Kind = "generic_repeat"
	KindPingPong        Kind = "ping_pong"
	KindPollNoProgress  Kind = "poll_no_progress"
	KindCircuitBreaker  Kind = "circuit_breaker"
)

// Level is the severity of a detected loop.
type Level string

const (
	Warning  Level = "warning"
	Critical Level = "critical"
)

// Result is returned by Detect.
type Result struct {
	Stuck   bool
	Level   Level
	Kind    Kind
	Count   int
	Message string
}

// record is one tool call in the history window.
type record struct {
	toolName   string
	argsHash   string
	resultHash string // "" if not yet recorded
}

// Detector tracks tool call history for one agent session.
type Detector struct {
	history []record
}

// New creates an empty Detector.
func New() *Detector {
	return &Detector{}
}

// Record adds a tool call to the history before execution.
func (d *Detector) Record(toolName, argsJSON string) {
	h := hashArgs(toolName, argsJSON)
	d.history = append(d.history, record{toolName: toolName, argsHash: h})
	if len(d.history) > HistorySize {
		d.history = d.history[len(d.history)-HistorySize:]
	}
}

// RecordOutcome adds the result hash to the most recent matching record.
func (d *Detector) RecordOutcome(toolName, argsJSON, result string, isError bool) {
	argsHash := hashArgs(toolName, argsJSON)
	rh := hashResult(result, isError)
	for i := len(d.history) - 1; i >= 0; i-- {
		if d.history[i].toolName == toolName && d.history[i].argsHash == argsHash && d.history[i].resultHash == "" {
			d.history[i].resultHash = rh
			return
		}
	}
}

// Detect checks for loops before the next tool call.
// Must be called after Record but before the tool executes.
func (d *Detector) Detect(toolName, argsJSON string) Result {
	if len(d.history) == 0 {
		return Result{}
	}
	argsHash := hashArgs(toolName, argsJSON)

	// ── circuit breaker: any pattern repeated > 30 times ─────────────────────
	streak := noProgressStreak(d.history, toolName, argsHash)
	if streak >= CircuitBreakerThreshold {
		return Result{
			Stuck:   true,
			Level:   Critical,
			Kind:    KindCircuitBreaker,
			Count:   streak,
			Message: fmt.Sprintf("CRITICAL: %s repeated %d times with identical outcomes. Session blocked by circuit breaker.", toolName, streak),
		}
	}

	// ── poll_no_progress ──────────────────────────────────────────────────────
	if isPollTool(toolName) {
		if streak >= CriticalThreshold {
			return Result{
				Stuck:   true,
				Level:   Critical,
				Kind:    KindPollNoProgress,
				Count:   streak,
				Message: fmt.Sprintf("CRITICAL: %s called %d times with no progress. Stop polling — report the task as failed.", toolName, streak),
			}
		}
		if streak >= WarningThreshold {
			return Result{
				Stuck:   true,
				Level:   Warning,
				Kind:    KindPollNoProgress,
				Count:   streak,
				Message: fmt.Sprintf("WARNING: %s called %d times with no change. Consider increasing wait time or aborting.", toolName, streak),
			}
		}
	}

	// ── ping_pong ─────────────────────────────────────────────────────────────
	if pp := pingPongStreak(d.history, argsHash); pp >= CriticalThreshold {
		return Result{
			Stuck:   true,
			Level:   Critical,
			Kind:    KindPingPong,
			Count:   pp,
			Message: fmt.Sprintf("CRITICAL: Alternating tool-call pattern detected (%d calls). You are stuck in a ping-pong loop. Stop and report failure.", pp),
		}
	} else if pp >= WarningThreshold {
		return Result{
			Stuck:   true,
			Level:   Warning,
			Kind:    KindPingPong,
			Count:   pp,
			Message: fmt.Sprintf("WARNING: Alternating tool-call pattern (%d calls). This looks like a ping-pong loop; try a different approach.", pp),
		}
	}

	// ── generic_repeat ────────────────────────────────────────────────────────
	if !isPollTool(toolName) {
		count := 0
		for _, r := range d.history {
			if r.toolName == toolName && r.argsHash == argsHash {
				count++
			}
		}
		if count >= WarningThreshold {
			return Result{
				Stuck:   true,
				Level:   Warning,
				Kind:    KindGenericRepeat,
				Count:   count,
				Message: fmt.Sprintf("WARNING: %s called %d times with identical arguments. If not making progress, stop and report failure.", toolName, count),
			}
		}
	}

	return Result{}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

func hashArgs(toolName, argsJSON string) string {
	h := sha256.Sum256([]byte(toolName + ":" + stableArgs(argsJSON)))
	return fmt.Sprintf("%x", h[:8])
}

func hashResult(result string, isError bool) string {
	key := fmt.Sprintf("%v:%s", isError, result)
	h := sha256.Sum256([]byte(key))
	return fmt.Sprintf("%x", h[:8])
}

// stableArgs produces a deterministic JSON string from argsJSON for hashing.
// Handles both object and non-object inputs gracefully.
func stableArgs(argsJSON string) string {
	var v any
	if err := json.Unmarshal([]byte(argsJSON), &v); err != nil {
		return argsJSON
	}
	return stableStringify(v)
}

func stableStringify(v any) string {
	if v == nil {
		return "null"
	}
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b := []byte("{")
		for i, k := range keys {
			if i > 0 {
				b = append(b, ',')
			}
			kb, _ := json.Marshal(k)
			b = append(b, kb...)
			b = append(b, ':')
			b = append(b, []byte(stableStringify(val[k]))...)
		}
		return string(append(b, '}'))
	case []any:
		b := []byte("[")
		for i, elem := range val {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, []byte(stableStringify(elem))...)
		}
		return string(append(b, ']'))
	default:
		out, _ := json.Marshal(v)
		return string(out)
	}
}

// noProgressStreak counts the trailing streak of identical (argsHash, resultHash) pairs.
func noProgressStreak(history []record, toolName, argsHash string) int {
	var streak int
	var latestResultHash string
	for i := len(history) - 1; i >= 0; i-- {
		r := history[i]
		if r.toolName != toolName || r.argsHash != argsHash {
			continue
		}
		if r.resultHash == "" {
			continue
		}
		if latestResultHash == "" {
			latestResultHash = r.resultHash
			streak = 1
			continue
		}
		if r.resultHash != latestResultHash {
			break
		}
		streak++
	}
	return streak
}

// pingPongStreak counts how many consecutive calls alternate between two arg hashes.
func pingPongStreak(history []record, currentArgHash string) int {
	if len(history) < 2 {
		return 0
	}
	last := history[len(history)-1]
	// Find the other hash in the alternating pair.
	var otherHash string
	for i := len(history) - 2; i >= 0; i-- {
		if history[i].argsHash != last.argsHash {
			otherHash = history[i].argsHash
			break
		}
	}
	if otherHash == "" {
		return 0
	}
	// Count the alternating tail.
	count := 0
	for i := len(history) - 1; i >= 0; i-- {
		expected := last.argsHash
		if count%2 == 1 {
			expected = otherHash
		}
		if history[i].argsHash != expected {
			break
		}
		count++
	}
	// Confirm current call continues the pattern.
	if count%2 == 0 && currentArgHash != otherHash {
		return 0
	}
	return count + 1
}

func isPollTool(name string) bool {
	switch name {
	case "run_command", "get_diagnostics", "run_tests":
		return true
	}
	return false
}
