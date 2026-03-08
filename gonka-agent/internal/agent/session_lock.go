package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// sessionLock acquires an exclusive write lock for the session file.
// Pattern ported from openclaw/src/agents/session-write-lock.ts.
//
// Lock file: <sessionPath>.lock
// Contents:  {"pid": N, "created_at": "RFC3339"}
//
// Stale detection:
//   - PID not alive (kill -0 returns ESRCH) → stale
//   - Lock file older than 5 minutes → stale
//
// Returns a release function; caller must defer it.
func sessionLock(sessionPath string) (release func(), err error) {
	lockPath := sessionPath + ".lock"
	deadline := time.Now().Add(10 * time.Second)

	for {
		f, openErr := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if openErr == nil {
			// Lock acquired.
			payload := map[string]any{
				"pid":        os.Getpid(),
				"created_at": time.Now().Format(time.RFC3339),
			}
			data, _ := json.Marshal(payload)
			_, _ = f.Write(data)
			_ = f.Close()
			return func() {
				_ = os.Remove(lockPath)
			}, nil
		}

		if !os.IsExist(openErr) {
			return nil, fmt.Errorf("session lock: %w", openErr)
		}

		// Lock file exists — check if stale.
		if isStale(lockPath) {
			_ = os.Remove(lockPath)
			continue // retry immediately
		}

		if time.Now().After(deadline) {
			return nil, fmt.Errorf("session lock: timeout waiting for %s", lockPath)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// isStale returns true if the lock file belongs to a dead process or is too old.
func isStale(lockPath string) bool {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return true // unreadable → treat as stale
	}
	var payload struct {
		PID       int    `json:"pid"`
		CreatedAt string `json:"created_at"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return true
	}
	// Check age: stale if older than 5 minutes.
	if t, err := time.Parse(time.RFC3339, payload.CreatedAt); err == nil {
		if time.Since(t) > 5*time.Minute {
			return true
		}
	}
	// Check PID liveness via kill(pid, 0).
	if payload.PID > 0 && payload.PID != os.Getpid() {
		if err := syscall.Kill(payload.PID, 0); err != nil {
			// ESRCH = no such process → stale.
			return true
		}
	}
	return false
}

// pidFromLockFile returns the PID stored in a lock file (for diagnostics).
func pidFromLockFile(lockPath string) string {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return "unknown"
	}
	// Fast path: find "pid": N without full JSON parse.
	s := string(data)
	i := strings.Index(s, `"pid"`)
	if i < 0 {
		return "unknown"
	}
	s = s[i+5:]
	i = strings.IndexAny(s, "0123456789")
	if i < 0 {
		return "unknown"
	}
	s = s[i:]
	end := strings.IndexAny(s, ",}")
	if end < 0 {
		end = len(s)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(s[:end]))
	if err != nil {
		return "unknown"
	}
	return strconv.Itoa(pid)
}
