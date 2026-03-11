package tui

import "github.com/charmbracelet/lipgloss"

// Gonka GO palette: turquoise-blue-black
var (
	ColorTurquoise = lipgloss.Color("#00D4AA")
	ColorBlue      = lipgloss.Color("#0088FF")
	ColorDarkBlue  = lipgloss.Color("#0044AA")
	ColorBlack     = lipgloss.Color("#0A0E14")
	ColorDarkGray  = lipgloss.Color("#1A1E28")
	ColorMidGray   = lipgloss.Color("#3A3E48")
	ColorLightGray = lipgloss.Color("#8A8E98")
	ColorWhite     = lipgloss.Color("#E0E4EE")
	ColorRed       = lipgloss.Color("#FF4466")
	ColorYellow    = lipgloss.Color("#FFCC00")
	ColorGreen     = lipgloss.Color("#00FF88")

	StyleTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorTurquoise).
			Background(ColorBlack).
			Padding(0, 1)

	StyleSubtitle = lipgloss.NewStyle().
			Foreground(ColorBlue).
			Bold(true)

	StyleNormal = lipgloss.NewStyle().
			Foreground(ColorWhite)

	StyleDim = lipgloss.NewStyle().
			Foreground(ColorLightGray)

	StyleError = lipgloss.NewStyle().
			Foreground(ColorRed).
			Bold(true)

	StyleSuccess = lipgloss.NewStyle().
			Foreground(ColorGreen).
			Bold(true)

	StyleWarning = lipgloss.NewStyle().
			Foreground(ColorYellow)

	StyleBorder = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(ColorTurquoise).
			Padding(0, 1)

	StyleActiveTab = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorBlack).
			Background(ColorTurquoise).
			Padding(0, 2)

	StyleInactiveTab = lipgloss.NewStyle().
			Foreground(ColorLightGray).
			Background(ColorDarkGray).
			Padding(0, 2)

	StyleStatusBar = lipgloss.NewStyle().
			Foreground(ColorTurquoise).
			Background(ColorDarkGray).
			Padding(0, 1)

	StyleInput = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(ColorBlue).
			Padding(0, 1)

	StyleSpinner = lipgloss.NewStyle().
			Foreground(ColorTurquoise)
)

// Logo returns the Gonka GO ASCII art banner.
func Logo() string {
	logo := `
  ██████╗  ██████╗ ███╗   ██╗██╗  ██╗ █████╗ 
 ██╔════╝ ██╔═══██╗████╗  ██║██║ ██╔╝██╔══██╗
 ██║  ███╗██║   ██║██╔██╗ ██║█████╔╝ ███████║
 ██║   ██║██║   ██║██║╚██╗██║██╔═██╗ ██╔══██║
 ╚██████╔╝╚██████╔╝██║ ╚████║██║  ██╗██║  ██║
  ╚═════╝  ╚═════╝ ╚═╝  ╚═══╝╚═╝  ╚═╝╚═╝  ╚═╝`
	return StyleTitle.Render(logo)
}
