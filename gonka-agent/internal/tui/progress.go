package tui

import (
	"fmt"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// ProgressBus is a concurrency-safe event bus that connects the agent's
// execution engine to both TUI and CLI output. Components push events;
// the TUI subscribes to receive them as tea.Msg.
type ProgressBus struct {
	mu        sync.RWMutex
	events    []ProgressEvent
	tuiProg   *tea.Program
	cliMode   bool
	startTime time.Time
}

// NewProgressBus creates a bus. If cliMode=true, events print to stdout.
func NewProgressBus(cliMode bool) *ProgressBus {
	return &ProgressBus{
		cliMode:   cliMode,
		startTime: time.Now(),
	}
}

// SetProgram links the bus to a running bubbletea program for live updates.
func (b *ProgressBus) SetProgram(p *tea.Program) {
	b.mu.Lock()
	b.tuiProg = p
	b.mu.Unlock()
}

// Emit pushes a progress event to TUI and/or CLI.
func (b *ProgressBus) Emit(phase, step, tool string, tokens int) {
	ev := ProgressEvent{
		Phase:   phase,
		Step:    step,
		Tool:    tool,
		Tokens:  tokens,
		Elapsed: time.Since(b.startTime),
	}

	b.mu.Lock()
	b.events = append(b.events, ev)
	prog := b.tuiProg
	cli := b.cliMode
	b.mu.Unlock()

	if prog != nil {
		prog.Send(ev)
	}
	if cli {
		b.printCLI(ev)
	}
}

func (b *ProgressBus) printCLI(ev ProgressEvent) {
	elapsed := ev.Elapsed.Round(time.Millisecond)
	bar := renderMiniBar(ev.Phase)
	fmt.Printf("%s %s │ %s │ %s │ %dtkn │ %s\n",
		bar, colorize(ev.Phase, "turquoise"), ev.Step,
		colorize(ev.Tool, "dim"), ev.Tokens, elapsed)
}

func renderMiniBar(phase string) string {
	phases := []string{"PLAN", "EXECUTE", "VERIFY"}
	var parts []string
	for _, p := range phases {
		if strings.EqualFold(p, phase) {
			parts = append(parts, colorize("●", "turquoise"))
		} else {
			parts = append(parts, colorize("○", "dim"))
		}
	}
	return strings.Join(parts, "")
}

func colorize(s, color string) string {
	switch color {
	case "turquoise":
		return "\033[38;2;0;212;170m" + s + "\033[0m"
	case "blue":
		return "\033[38;2;0;136;255m" + s + "\033[0m"
	case "dim":
		return "\033[38;2;138;142;152m" + s + "\033[0m"
	case "red":
		return "\033[38;2;255;68;102m" + s + "\033[0m"
	case "green":
		return "\033[38;2;0;255;136m" + s + "\033[0m"
	default:
		return s
	}
}

// Summary returns a formatted summary of all events for reporting.
func (b *ProgressBus) Summary() string {
	b.mu.RLock()
	defer b.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString("## Progress Summary\n\n")
	sb.WriteString("| Phase | Step | Tool | Tokens | Elapsed |\n")
	sb.WriteString("|-------|------|------|--------|---------|\n")
	totalTokens := 0
	for _, ev := range b.events {
		sb.WriteString(fmt.Sprintf("| %s | %s | %s | %d | %s |\n",
			ev.Phase, ev.Step, ev.Tool, ev.Tokens,
			ev.Elapsed.Round(time.Millisecond)))
		totalTokens += ev.Tokens
	}
	sb.WriteString(fmt.Sprintf("\n**Total**: %d events, %d tokens, %s\n",
		len(b.events), totalTokens, time.Since(b.startTime).Round(time.Second)))
	return sb.String()
}
