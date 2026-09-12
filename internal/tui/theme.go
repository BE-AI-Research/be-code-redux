package tui

import (
	"sort"

	"github.com/charmbracelet/lipgloss"
)

// Palette is one colour theme. Hex values render as true colour where the
// terminal supports it and degrade to the nearest 256-colour entry
// otherwise. Mono is the exception: no colour at all.
type Palette struct {
	Name, Desc                             string
	Accent, Dim, Tool, Err, OK, Warn, User string
	StatusBG, StatusFG, ModalTitle, Border string
	Light                                  bool // for readers; dark is the default
	Mono                                   bool
	SquareBorder                           bool
}

var themes = map[string]Palette{
	"dark":            {Name: "dark", Desc: "BE-Code default (256-colour)", Accent: "39", Dim: "241", Tool: "44", Err: "203", OK: "42", Warn: "214", User: "213", StatusBG: "236", StatusFG: "250", ModalTitle: "214", Border: "240"},
	"light":           {Name: "light", Desc: "readable on light backgrounds (256-colour)", Light: true, Accent: "25", Dim: "244", Tool: "30", Err: "124", OK: "28", Warn: "130", User: "90", StatusBG: "252", StatusFG: "236", ModalTitle: "130", Border: "248"},
	"mono":            {Name: "mono", Desc: "no colour, bold and reverse only", Mono: true, SquareBorder: true},
	"dracula":         {Name: "dracula", Desc: "purple and pink on charcoal", Accent: "#bd93f9", Dim: "#6272a4", Tool: "#8be9fd", Err: "#ff5555", OK: "#50fa7b", Warn: "#f1fa8c", User: "#ff79c6", StatusBG: "#44475a", StatusFG: "#f8f8f2", ModalTitle: "#ffb86c", Border: "#6272a4", BG: "#282a36"},
	"nord":            {Name: "nord", Desc: "cool arctic blues", Accent: "#88c0d0", Dim: "#4c566a", Tool: "#81a1c1", Err: "#bf616a", OK: "#a3be8c", Warn: "#ebcb8b", User: "#b48ead", StatusBG: "#3b4252", StatusFG: "#e5e9f0", ModalTitle: "#ebcb8b", Border: "#4c566a", BG: "#2e3440"},
	"gruvbox":         {Name: "gruvbox", Desc: "warm retro earth tones", Accent: "#83a598", Dim: "#928374", Tool: "#8ec07c", Err: "#fb4934", OK: "#b8bb26", Warn: "#fabd2f", User: "#d3869b", StatusBG: "#3c3836", StatusFG: "#ebdbb2", ModalTitle: "#fe8019", Border: "#665c54", BG: "#282828"},
	"monokai":         {Name: "monokai", Desc: "classic editor greens and pinks", Accent: "#66d9ef", Dim: "#75715e", Tool: "#a6e22e", Err: "#f92672", OK: "#a6e22e", Warn: "#e6db74", User: "#ae81ff", StatusBG: "#3e3d32", StatusFG: "#f8f8f2", ModalTitle: "#fd971f", Border: "#75715e", BG: "#272822"},
	"one-dark":        {Name: "one-dark", Desc: "Atom One Dark", Accent: "#61afef", Dim: "#5c6370", Tool: "#56b6c2", Err: "#e06c75", OK: "#98c379", Warn: "#e5c07b", User: "#c678dd", StatusBG: "#3e4451", StatusFG: "#abb2bf", ModalTitle: "#d19a66", Border: "#5c6370", BG: "#282c34"},
	"solarized-dark":  {Name: "solarized-dark", Desc: "Solarized on base03", Accent: "#268bd2", Dim: "#586e75", Tool: "#2aa198", Err: "#dc322f", OK: "#859900", Warn: "#b58900", User: "#d33682", StatusBG: "#073642", StatusFG: "#93a1a1", ModalTitle: "#cb4b16", Border: "#586e75", BG: "#002b36"},
	"solarized-light": {Name: "solarized-light", Desc: "Solarized on base3", Light: true, Accent: "#268bd2", Dim: "#93a1a1", Tool: "#2aa198", Err: "#dc322f", OK: "#859900", Warn: "#b58900", User: "#d33682", StatusBG: "#eee8d5", StatusFG: "#586e75", ModalTitle: "#cb4b16", Border: "#93a1a1", BG: "#fdf6e3", FG: "#657b83"},
	"tokyo-night":     {Name: "tokyo-night", Desc: "deep blue night with neon accents", Accent: "#7aa2f7", Dim: "#565f89", Tool: "#7dcfff", Err: "#f7768e", OK: "#9ece6a", Warn: "#e0af68", User: "#bb9af7", StatusBG: "#292e42", StatusFG: "#c0caf5", ModalTitle: "#ff9e64", Border: "#565f89", BG: "#1a1b26"},
	"catppuccin":      {Name: "catppuccin", Desc: "Catppuccin Mocha pastels", Accent: "#89b4fa", Dim: "#6c7086", Tool: "#94e2d5", Err: "#f38ba8", OK: "#a6e3a1", Warn: "#f9e2af", User: "#cba6f7", StatusBG: "#313244", StatusFG: "#cdd6f4", ModalTitle: "#fab387", Border: "#585b70", BG: "#1e1e2e"},
	"github-light":    {Name: "github-light", Desc: "GitHub's light palette", Light: true, Accent: "#0969da", Dim: "#6e7781", Tool: "#0550ae", Err: "#cf222e", OK: "#1a7f37", Warn: "#9a6700", User: "#8250df", StatusBG: "#eaeef2", StatusFG: "#24292f", ModalTitle: "#bc4c00", Border: "#d0d7de", BG: "#ffffff", FG: "#24292f"},
}

// ThemeNames lists every theme, default first, then alphabetical.
func ThemeNames() []string {
	names := make([]string, 0, len(themes))
	for n := range themes {
		if n != "dark" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return append([]string{"dark"}, names...)
}

func lookupTheme(name string) (Palette, bool) {
	p, ok := themes[name]
	return p, ok
}

// SetTheme applies a theme to the live palette and returns the name that
// was actually applied ("dark" when name is unknown).
func SetTheme(name string) string {
	p, ok := themes[name]
	if !ok {
		p = themes["dark"]
	}
	if p.Mono {
		plain := lipgloss.NewStyle()
		stAccent, stDim, stTool, stErr, stOK, stWarn = plain, plain, plain, plain, plain, plain
		stUser = lipgloss.NewStyle().Bold(true)
		stStatus = lipgloss.NewStyle().Reverse(true).Padding(0, 1)
		stModalTi = lipgloss.NewStyle().Bold(true)
		stBorder = lipgloss.NewStyle().Border(lipgloss.NormalBorder()).Padding(0, 1)
		return p.Name
	}
	fg := func(c string) lipgloss.Style { return lipgloss.NewStyle().Foreground(lipgloss.Color(c)) }
	stAccent, stDim, stTool = fg(p.Accent), fg(p.Dim), fg(p.Tool)
	stErr, stOK, stWarn = fg(p.Err), fg(p.OK), fg(p.Warn)
	stUser = fg(p.User).Bold(true)
	stStatus = lipgloss.NewStyle().Background(lipgloss.Color(p.StatusBG)).Foreground(lipgloss.Color(p.StatusFG)).Padding(0, 1)
	stModalTi = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(p.ModalTitle))
	border := lipgloss.RoundedBorder()
	if p.SquareBorder {
		border = lipgloss.NormalBorder()
	}
	stBorder = lipgloss.NewStyle().Border(border).BorderForeground(lipgloss.Color(p.Border)).Padding(0, 1)
	return p.Name
}
