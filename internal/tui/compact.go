package tui

import (
	"fmt"
	"strings"
)

// Compact layout: each terminal is judged by its own size (a phone over SSH,
// a narrow IDE panel), so one attached terminal can be compact while another
// keeps the full layout. Below this size the header, long hints and popup
// descriptions cost more than they are worth, so that terminal switches to a
// reduced layout; m.ascii (set from that client's own capabilities, see
// served.go) separately swaps glyphs for ASCII fallbacks regardless of size.

const (
	compactCols = 70
	compactRows = 20
)

// compact reports whether the reduced layout is in force: forced by
// config, or automatic when the (shared) size is small.
func (m *View) compact() bool {
	switch strings.ToLower(m.cfg.Layout) {
	case "compact":
		return true
	case "full":
		return false
	}
	return m.width < compactCols || m.height < compactRows
}

// ---- ASCII glyph fallbacks --------------------------------------------------

var (
	asciiWheelFill = []string{"o", ".", "o", "O", "*"}
	asciiWheelSpin = []string{"|", "/", "-", "\\"}
)

// wheelGlyphASCII mirrors wheelGlyph but with ASCII-only glyphs, for clients
// that cannot render the Unicode wheel.
func wheelGlyphASCII(pct int, busy bool, frame int) string {
	if busy {
		return asciiWheelSpin[((frame%4)+4)%4]
	}
	switch {
	case pct < 13:
		return asciiWheelFill[0]
	case pct < 38:
		return asciiWheelFill[1]
	case pct < 63:
		return asciiWheelFill[2]
	case pct < 88:
		return asciiWheelFill[3]
	default:
		return asciiWheelFill[4]
	}
}

// ideMarker is the bottom-line editor-bridge marker: "⌘ ide" in full layout,
// just "⌘" in compact, "IDE" for an ASCII client either way.
func (m *View) ideMarker() string {
	if m.ascii {
		return "IDE"
	}
	if m.compact() {
		return "⌘"
	}
	return "⌘ ide"
}

// clientsGlyph is the multi-terminal marker glyph, ASCII-safe when needed.
func (m *View) clientsGlyph() string {
	if m.ascii {
		return "#"
	}
	return "⧉"
}

// ---- popups ------------------------------------------------------------------

// popupRows caps a popup's row count to 6 in compact layout.
func (m *View) popupRows(want int) int {
	if m.compact() && want > 6 {
		return 6
	}
	return want
}

// omitPopupDesc reports whether a popup should drop item descriptions to
// save width. Gated on compact() first so "layout: full" overrides it even
// in a narrow terminal, matching the documented override.
func (m *View) omitPopupDesc() bool {
	return m.compact() && m.width < 60
}

// ---- bottom line -------------------------------------------------------------

// compactModelCap is how much of the model name the compact line keeps. It
// leaves room for "/menu", the state word and the clients marker at 40
// columns, the narrowest terminal the layout is written for.
const compactModelCap = 16

// compactBottomLine is the single-row status when the layout is reduced:
// "/menu · <model> · <state>", the queue count as "qN" (⧉ stays reserved
// for the clients marker), the IDE marker, and the clients marker — no
// hint text.
func (m *View) compactBottomLine() string {
	state := m.st.OK.Render("ready")
	if m.running {
		word := ""
		if fields := strings.Fields(m.statusNote); len(fields) > 0 {
			word = fields[0]
		}
		state = m.spin.View() + " " + word
		if n := m.ag.Pending(); n > 0 {
			state += m.st.Accent.Render(fmt.Sprintf(" · q%d", n))
		}
	}
	line := " " + m.st.Accent.Render("/menu") + m.st.Dim.Render(" · "+shortModelTo(m.ag.Model, compactModelCap)+" · ") + state
	if m.ag.IDEName != "" {
		line += m.st.Accent.Render(" " + m.ideMarker())
	}
	if m.runningSubs != nil {
		if subs := m.runningSubs(); len(subs) == 1 {
			line += m.st.Accent.Render(" ⚙ " + subs[0].Name + " " + subs[0].At)
		} else if len(subs) > 1 {
			line += m.st.Accent.Render(fmt.Sprintf(" ⚙ %d lanes", len(subs)))
		}
	}
	if len(m.clients) > 1 {
		line += m.st.Accent.Render(fmt.Sprintf(" %s %d", m.clientsGlyph(), len(m.clients)))
	}
	// The room's own cue, in the short form: a terminal small enough for this
	// layout is the one most likely to be the second screen somebody is
	// chatting from, and without it nothing on the frame says the room moved.
	if m.chatUnseen > 0 && m.mode != modeChat {
		line += m.st.Accent.Render(fmt.Sprintf(" · chat %d", m.chatUnseen))
	}
	return line
}

// ---- menu status -------------------------------------------------------------

// compactMenuStatus is the two-line status block shown above the menu
// entries in compact layout, in place of menuStatus's full row list.
func (m *View) compactMenuStatus() string {
	line1 := fmt.Sprintf("%s · %s", m.ag.Model, m.ag.Profile.Family)
	line2 := fmt.Sprintf("context %d%% · session %dk tokens", m.ctxPercent(), m.usage.total/1000)
	return m.st.Dim.Render(line1) + "\n" + m.st.Dim.Render(line2)
}
