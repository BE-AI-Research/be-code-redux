package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/lipgloss"
)

// One input line per attached terminal. A shared session renders one frame
// for everyone, but a draft belongs to whoever is typing it: each client
// gets its own textarea, keys are routed to the sender's, and the shared
// frame leaves the input rows blank (the host splices each client's own
// rows in). The in-process TUI is simply client 0.

// newInputArea builds one textarea with the prompt, height and key
// bindings every input line shares.
func (m *View) newInputArea() *textarea.Model {
	ta := textarea.New()
	ta.Placeholder = "describe a task…  (Enter sends · Ctrl+J newline · / for commands)"
	ta.SetHeight(m.inputRows())
	setInputPrompt(&ta, m.compact())
	ta.CharLimit = 0
	ta.ShowLineNumbers = false
	ta.Focus()
	ta.KeyMap.InsertNewline.SetKeys("ctrl+j")
	if m.served {
		ta.Cursor.SetMode(cursor.CursorStatic) // blinking would push an overlay every 500 ms per client
	}
	if w := m.inputWidth(); w > 0 {
		ta.SetWidth(w)
	}
	return &ta
}

// setInputPrompt gives a textarea the prompt of the current layout: the
// short one in compact, the full one otherwise. layout() applies it to
// every input line on a resize, and newInputArea applies it once up front —
// a terminal attaching to a session that is already compact must not have
// to wait for the next resize to get the right prompt.
func setInputPrompt(ta *textarea.Model, compact bool) {
	if compact {
		ta.SetPromptFunc(2, func(i int) string {
			if i == 0 {
				return "> "
			}
			return "  "
		})
		return
	}
	ta.SetPromptFunc(5, func(i int) string {
		if i == 0 {
			return "(>): "
		}
		return "     "
	})
}

// inputFor returns the textarea of one client, creating it on first use.
// The in-process TUI is client 0.
func (m *View) inputFor(client int) *textarea.Model {
	if m.inputs == nil {
		m.inputs = map[int]*textarea.Model{}
	}
	ta, ok := m.inputs[client]
	if !ok {
		ta = m.newInputArea()
		m.inputs[client] = ta
	}
	return ta
}

// dropInput forgets a departed client's draft. Its place in the shared input
// history is dropped by Session.SetClients, which owns everything the
// terminals share.
func (m *View) dropInput(client int) {
	delete(m.inputs, client)
}

// inputRows is the height of the input area: three rows, or one in a
// served session small enough for the compact layout.
func (m *View) inputRows() int {
	if m.served && m.compact() {
		return 1
	}
	return 3
}

// wheelWidthNow is the width of the context-wheel column to the right of
// the input row: the full glyph-plus-percentage field, or the unpadded
// short form in compact layout.
func (m *View) wheelWidthNow() int {
	if m.compact() {
		return 5 // glyph + "NN%", no fixed-width padding
	}
	return wheelWidth
}

// inputWidth is the width of one client's input line, leaving room for the
// wheel column.
func (m *View) inputWidth() int {
	w := m.width - m.wheelWidthNow() - 2
	if w < 0 {
		w = 0
	}
	return w
}

// blankInputRows is what the shared frame shows where each client's private
// input rows will be spliced in.
func (m *View) blankInputRows() string {
	row := strings.Repeat(" ", m.inputWidth())
	rows := make([]string, m.inputRows())
	for i := range rows {
		rows[i] = row
	}
	return strings.Join(rows, "\n")
}

// clientLabels joins the attached terminals' labels for the bottom line,
// truncated with an ellipsis to fit room cells. With no usable room it
// returns "": an untruncated list would overrun the row and wrap the
// status line on every attached terminal.
func (m *View) clientLabels(room int) string {
	if room <= 1 {
		return ""
	}
	labels := make([]string, 0, len(m.clients))
	for _, c := range m.clients {
		labels = append(labels, c.Label)
	}
	joined := strings.Join(labels, ", ")
	if r := []rune(joined); len(r) > room {
		joined = string(r[:room-1]) + "…"
	}
	return joined
}

// inputRow is the input area plus the context wheel at its right. In a
// served session the frame is shared by every terminal, so the input rows
// are left blank for the host to splice each client's own line into.
func (m *View) inputRow() string {
	body := m.blankInputRows()
	if !m.served {
		body = m.inputFor(0).View()
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, body, " "+m.wheelView())
}
