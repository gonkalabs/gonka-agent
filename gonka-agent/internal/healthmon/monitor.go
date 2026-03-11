// Package healthmon provides a global error bus and structured event
// logging. Components report errors to the bus; the monitor aggregates
// them, writes to a JSONL log, and triggers alerts / restarts.
package healthmon

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Severity int

const (
	SevInfo    Severity = iota
	SevWarn
	SevError
	SevFatal
)

func (s Severity) String() string {
	switch s {
	case SevInfo:
		return "info"
	case SevWarn:
		return "warn"
	case SevError:
		return "error"
	case SevFatal:
		return "fatal"
	}
	return "unknown"
}

// Event represents a single health event from any component.
type Event struct {
	Time      time.Time `json:"time"`
	Component string    `json:"component"`
	Severity  string    `json:"severity"`
	Message   string    `json:"message"`
	Detail    string    `json:"detail,omitempty"`
}

// RestartFunc is called when a component's error count exceeds the threshold.
type RestartFunc func(component string) error

// Monitor is the global health monitor. One instance per agent process.
type Monitor struct {
	events  chan Event
	logPath string
	alertFn func(Event)

	mu        sync.RWMutex
	counts    map[string]int
	restarts  map[string]RestartFunc
	threshold int

	done chan struct{}
	once sync.Once
}

// New creates a monitor. cacheDir is typically ~/.gonka-cache.
func New(cacheDir string) *Monitor {
	logPath := filepath.Join(cacheDir, "errors.jsonl")
	os.MkdirAll(filepath.Dir(logPath), 0755)

	return &Monitor{
		events:    make(chan Event, 256),
		logPath:   logPath,
		counts:    make(map[string]int),
		restarts:  make(map[string]RestartFunc),
		threshold: 5,
		done:      make(chan struct{}),
	}
}

// SetAlertFunc registers a callback for error+ events (e.g. TUI notification).
func (m *Monitor) SetAlertFunc(fn func(Event)) {
	m.mu.Lock()
	m.alertFn = fn
	m.mu.Unlock()
}

// RegisterRestart registers a function to restart a component when it
// exceeds the error threshold. The monitor calls it automatically.
func (m *Monitor) RegisterRestart(component string, fn RestartFunc) {
	m.mu.Lock()
	m.restarts[component] = fn
	m.mu.Unlock()
}

// Report sends an event to the bus. Non-blocking; drops if buffer full.
func (m *Monitor) Report(component string, sev Severity, msg string, detail string) {
	ev := Event{
		Time:      time.Now(),
		Component: component,
		Severity:  sev.String(),
		Message:   msg,
		Detail:    detail,
	}
	select {
	case m.events <- ev:
	default:
		slog.Warn("healthmon: event buffer full, dropping", "component", component, "msg", msg)
	}
}

// Reportf is a convenience wrapper with fmt.Sprintf for the message.
func (m *Monitor) Reportf(component string, sev Severity, format string, args ...any) {
	m.Report(component, sev, fmt.Sprintf(format, args...), "")
}

// Start begins the background event processor. Call once.
func (m *Monitor) Start() {
	go m.watch()
}

// Stop gracefully shuts down the monitor.
func (m *Monitor) Stop() {
	m.once.Do(func() { close(m.done) })
}

func (m *Monitor) watch() {
	f, err := os.OpenFile(m.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		slog.Error("healthmon: cannot open log", "path", m.logPath, "err", err)
		return
	}
	defer f.Close()

	enc := json.NewEncoder(f)

	for {
		select {
		case ev := <-m.events:
			enc.Encode(ev)

			switch ev.Severity {
			case "error", "fatal":
				slog.Error("healthmon", "component", ev.Component, "msg", ev.Message)
				m.mu.Lock()
				m.counts[ev.Component]++
				count := m.counts[ev.Component]
				restartFn := m.restarts[ev.Component]
				alertFn := m.alertFn
				m.mu.Unlock()

				if alertFn != nil {
					alertFn(ev)
				}

				if count >= m.threshold && restartFn != nil {
					slog.Warn("healthmon: triggering restart", "component", ev.Component, "errors", count)
					if err := restartFn(ev.Component); err != nil {
						slog.Error("healthmon: restart failed", "component", ev.Component, "err", err)
					}
					m.mu.Lock()
					m.counts[ev.Component] = 0
					m.mu.Unlock()
				}
			case "warn":
				slog.Warn("healthmon", "component", ev.Component, "msg", ev.Message)
			default:
				slog.Info("healthmon", "component", ev.Component, "msg", ev.Message)
			}

		case <-m.done:
			return
		}
	}
}

// RecentErrors returns the last N errors from the JSONL log.
func (m *Monitor) RecentErrors(n int) ([]Event, error) {
	data, err := os.ReadFile(m.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var all []Event
	dec := json.NewDecoder(
		&byteReader{data: data},
	)
	for dec.More() {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			continue
		}
		if ev.Severity == "error" || ev.Severity == "fatal" {
			all = append(all, ev)
		}
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

type byteReader struct {
	data []byte
	pos  int
}

func (r *byteReader) Read(p []byte) (int, error) {
	if r.pos >= len(r.data) {
		return 0, fmt.Errorf("EOF")
	}
	n := copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
