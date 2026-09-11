package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// The context wheel is a single glyph at the right of the input row. Idle,
// it shows how full the context is; while the model works it rotates, one
// full turn every 3 seconds, so activity is visible even when no tokens
// are streaming (prompt processing, model loading).

var (
	wheelFill  = []string{"○", "◔", "◑", "◕", "●"}
	wheelSpin  = []string{"◴", "◵", "◶", "◷"}
	wheelEvery = 750 * time.Millisecond // 4 frames per 3-second turn
)

type wheelTickMsg struct{}

// wheelGlyph picks the glyph for a fill percentage, or the rotation frame
// when busy.
func wheelGlyph(pct int, busy bool, frame int) string {
	if busy {
		return wheelSpin[((frame%4)+4)%4]
	}
	switch {
	case pct < 13:
		return wheelFill[0]
	case pct < 38:
		return wheelFill[1]
	case pct < 63:
		return wheelFill[2]
	case pct < 88:
		return wheelFill[3]
	default:
		return wheelFill[4]
	}
}

func (m *Model) wheelTick() tea.Cmd {
	return tea.Tick(wheelEvery, func(time.Time) tea.Msg { return wheelTickMsg{} })
}

// ctxPercent is context usage relative to the compaction limit.
func (m *Model) ctxPercent() int {
	budget := m.usage.budget
	if budget <= 0 {
		budget = 1
	}
	pct := m.usage.ctxTokens * 100 / budget
	if pct < 0 {
		pct = 0
	}
	if pct > 999 {
		pct = 999
	}
	return pct
}

func (m *Model) wheelView() string {
	pct := m.ctxPercent()
	g := wheelGlyph(pct, m.running, m.wheelFrame)
	return stAccent.Render(g) + fmt.Sprintf(" %3d%%", pct)
}

// wheelWidth is the cells the wheel column takes (glyph, space, "100%").
const wheelWidth = 6
