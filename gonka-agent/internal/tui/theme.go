package tui

import "github.com/charmbracelet/lipgloss"

// Gonka GO palette: bright-blue / turquoise / metallic — NO black, NO dark gray
var (
	ColorTurquoise  = lipgloss.Color("#00D4AA")
	ColorBrightBlue = lipgloss.Color("#50B4FF")
	ColorBlue       = lipgloss.Color("#0088FF")
	ColorMetallic   = lipgloss.Color("#A0C8DC")
	ColorBG         = lipgloss.Color("#0C1824")
	ColorPanel      = lipgloss.Color("#142838")
	ColorWhite      = lipgloss.Color("#E0F0FF")
	ColorRed        = lipgloss.Color("#FF5252")
	ColorYellow     = lipgloss.Color("#FFD600")
	ColorGreen      = lipgloss.Color("#00E676")
	ColorMidGray    = lipgloss.Color("#50B4FF") // borders use blue, not gray

	StyleTitle = lipgloss.NewStyle().
			Bold(true).
			Foreground(ColorTurquoise).
			Padding(0, 1)

	StyleSubtitle = lipgloss.NewStyle().
			Foreground(ColorBrightBlue).
			Bold(true)

	StyleNormal = lipgloss.NewStyle().
			Foreground(ColorWhite)

	StyleDim = lipgloss.NewStyle().
			Foreground(ColorMetallic)

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
			Foreground(ColorBG).
			Background(ColorTurquoise).
			Padding(0, 2)

	StyleInactiveTab = lipgloss.NewStyle().
			Foreground(ColorMetallic).
			Background(ColorPanel).
			Padding(0, 2)

	StyleStatusBar = lipgloss.NewStyle().
			Foreground(ColorTurquoise).
			Background(ColorPanel).
			Padding(0, 1)

	StyleInput = lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(ColorBrightBlue).
			Padding(0, 1)

	StyleSpinner = lipgloss.NewStyle().
			Foreground(ColorTurquoise)
)

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
