package tui

import "github.com/charmbracelet/lipgloss"

// SetTheme adjusts the TUI palette. "dark" is the default; "light" picks
// colors readable on light backgrounds; "mono" strips color entirely.
func SetTheme(name string) {
	switch name {
	case "light":
		stAccent = lipgloss.NewStyle().Foreground(lipgloss.Color("25"))
		stDim = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))
		stTool = lipgloss.NewStyle().Foreground(lipgloss.Color("30"))
		stErr = lipgloss.NewStyle().Foreground(lipgloss.Color("124"))
		stOK = lipgloss.NewStyle().Foreground(lipgloss.Color("28"))
		stWarn = lipgloss.NewStyle().Foreground(lipgloss.Color("130"))
		stUser = lipgloss.NewStyle().Foreground(lipgloss.Color("90")).Bold(true)
		stStatus = lipgloss.NewStyle().Background(lipgloss.Color("252")).Foreground(lipgloss.Color("236")).Padding(0, 1)
		stModalTi = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("130"))
		stBorder = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("248")).Padding(0, 1)
	case "mono":
		plain := lipgloss.NewStyle()
		stAccent, stDim, stTool, stErr, stOK, stWarn = plain, plain, plain, plain, plain, plain
		stUser = lipgloss.NewStyle().Bold(true)
		stStatus = lipgloss.NewStyle().Reverse(true).Padding(0, 1)
		stModalTi = lipgloss.NewStyle().Bold(true)
		stBorder = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).Padding(0, 1)
	}
}
