// Package tui provides the bubbletea-based terminal UI for gonka-agent.
// Three panels: chat (left), progress (top-right), status (bottom-right).
// Keybinds: Tab to switch panels, Ctrl+C to quit, / to focus input.
package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

type panel int

const (
	panelChat panel = iota
	panelProgress
	panelStatus
)

// ProgressEvent is sent to the TUI to update the progress panel.
type ProgressEvent struct {
	Phase   string
	Step    string
	Tool    string
	Tokens  int
	Elapsed time.Duration
}

// ChatMsg is sent to the TUI to append a chat message.
type ChatMsg struct {
	Role    string // "user", "assistant", "system", "tool"
	Content string
}

// StatusUpdate refreshes the status panel.
type StatusUpdate struct {
	Provider     string
	Role         string
	SlotCount    int
	CacheHits    int64
	CacheMisses  int64
	Uptime       time.Duration
}

// Model is the main bubbletea model.
type Model struct {
	width, height int
	activePanel   panel

	chatHistory []ChatMsg
	chatView    viewport.Model
	input       textarea.Model

	progressLog []ProgressEvent
	progressView viewport.Model

	statusInfo StatusUpdate
	spinner    spinner.Model

	ready bool
}

// New creates a new TUI model.
func New() Model {
	ta := textarea.New()
	ta.Placeholder = "Type your task here..."
	ta.ShowLineNumbers = false
	ta.SetHeight(3)
	ta.Focus()

	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = StyleSpinner

	return Model{
		input:       ta,
		spinner:     sp,
		activePanel: panelChat,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		m.spinner.Tick,
	)
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "tab":
			m.activePanel = (m.activePanel + 1) % 3
			return m, nil
		case "enter":
			if m.activePanel == panelChat && m.input.Value() != "" {
				text := strings.TrimSpace(m.input.Value())
				m.chatHistory = append(m.chatHistory, ChatMsg{Role: "user", Content: text})
				m.input.Reset()
				m.updateChatView()
				return m, nil
			}
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		m.initViewports()
		return m, nil

	case ProgressEvent:
		m.progressLog = append(m.progressLog, msg)
		m.updateProgressView()

	case ChatMsg:
		m.chatHistory = append(m.chatHistory, msg)
		m.updateChatView()

	case StatusUpdate:
		m.statusInfo = msg

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(msg)
		cmds = append(cmds, cmd)
	}

	if m.activePanel == panelChat {
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(msg)
		cmds = append(cmds, cmd)
	}

	return m, tea.Batch(cmds...)
}

func (m *Model) initViewports() {
	chatW := m.width * 60 / 100
	rightW := m.width - chatW - 3

	m.chatView = viewport.New(chatW, m.height-6)
	m.progressView = viewport.New(rightW, m.height/2-2)
}

func (m *Model) updateChatView() {
	var sb strings.Builder
	for _, msg := range m.chatHistory {
		switch msg.Role {
		case "user":
			sb.WriteString(StyleSubtitle.Render("You") + "\n")
			sb.WriteString(StyleNormal.Render(msg.Content) + "\n\n")
		case "assistant":
			sb.WriteString(StyleTitle.Render("Gonka") + "\n")
			sb.WriteString(StyleNormal.Render(msg.Content) + "\n\n")
		case "system":
			sb.WriteString(StyleDim.Render("[system] "+msg.Content) + "\n")
		case "tool":
			sb.WriteString(StyleWarning.Render("[tool] "+msg.Content) + "\n")
		}
	}
	m.chatView.SetContent(sb.String())
	m.chatView.GotoBottom()
}

func (m *Model) updateProgressView() {
	var sb strings.Builder
	for _, ev := range m.progressLog {
		icon := StyleSuccess.Render("●")
		sb.WriteString(fmt.Sprintf("%s %s → %s  %s  %dtkn  %s\n",
			icon,
			StyleSubtitle.Render(ev.Phase),
			ev.Step,
			StyleDim.Render(ev.Tool),
			ev.Tokens,
			StyleDim.Render(ev.Elapsed.Round(time.Millisecond).String()),
		))
	}
	m.progressView.SetContent(sb.String())
	m.progressView.GotoBottom()
}

func (m Model) View() string {
	if !m.ready {
		return StyleTitle.Render("  Initializing Gonka GO...  ")
	}

	chatW := m.width * 60 / 100
	rightW := m.width - chatW - 3

	// Chat panel
	chatBorder := lipgloss.RoundedBorder()
	chatStyle := lipgloss.NewStyle().
		Border(chatBorder).
		BorderForeground(func() lipgloss.TerminalColor {
			if m.activePanel == panelChat {
				return ColorTurquoise
			}
			return ColorMidGray
		}()).
		Width(chatW).
		Height(m.height - 6)
	chatPanel := chatStyle.Render(m.chatView.View())

	// Input
	inputStyle := lipgloss.NewStyle().Width(chatW + 2)
	inputPanel := inputStyle.Render(m.input.View())

	left := lipgloss.JoinVertical(lipgloss.Left, chatPanel, inputPanel)

	// Progress panel
	progStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(func() lipgloss.TerminalColor {
			if m.activePanel == panelProgress {
				return ColorTurquoise
			}
			return ColorMidGray
		}()).
		Width(rightW).
		Height(m.height/2 - 2)
	progHeader := StyleSubtitle.Render("  Progress")
	progPanel := progStyle.Render(progHeader + "\n" + m.progressView.View())

	// Status panel
	statusStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(func() lipgloss.TerminalColor {
			if m.activePanel == panelStatus {
				return ColorTurquoise
			}
			return ColorMidGray
		}()).
		Width(rightW).
		Height(m.height/2 - 4)

	si := m.statusInfo
	statusContent := fmt.Sprintf(
		"%s %s\n%s %s\n%s %d\n%s %d/%d\n%s %s",
		StyleDim.Render("Provider:"), StyleNormal.Render(si.Provider),
		StyleDim.Render("Role:"), StyleNormal.Render(si.Role),
		StyleDim.Render("Slots:"), si.SlotCount,
		StyleDim.Render("Cache H/M:"), si.CacheHits, si.CacheMisses,
		StyleDim.Render("Uptime:"), si.Uptime.Round(time.Second),
	)
	statusPanel := statusStyle.Render(StyleSubtitle.Render("  Status") + "\n" + statusContent)

	right := lipgloss.JoinVertical(lipgloss.Left, progPanel, statusPanel)

	main := lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right)

	// Status bar
	bar := StyleStatusBar.Width(m.width).Render(
		fmt.Sprintf(" %s GONKA GO  │  Tab: switch panel  │  Ctrl+C: quit  │  Enter: send",
			m.spinner.View()))

	return lipgloss.JoinVertical(lipgloss.Left, main, bar)
}
