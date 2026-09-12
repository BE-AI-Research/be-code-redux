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
func (m *Model) newInputArea() *textarea.Model {
	ta := textarea.New()
	ta.Placeholder = "describe a task…  (Enter sends · Ctrl+J newline · / for commands)"
	ta.SetHeight(m.inputRows())
	ta.SetPromptFunc(5, func(lineIdx int) string {
		if lineIdx == 0 {
			return "(>): "
		}
		return "     "
	})
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

// inputFor returns the textarea of one client, creating it on first use.
// The in-process TUI is client 0.
func (m *Model) inputFor(client int) *textarea.Model {
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

// dropInput forgets a departed client's draft.
func (m *Model) dropInput(client int) { delete(m.inputs, client) }

// inputRows is the height of the input area: three rows, or one in a
// served session small enough for the compact layout.
func (m *Model) inputRows() int {
	if m.served && m.compact() {
		return 1
	}
	return 3
}

// wheelWidthNow is the width of the context-wheel column to the right of
// the input row: the full glyph-plus-percentage field, or the unpadded
// short form in compact layout.
func (m *Model) wheelWidthNow() int {
	if m.compact() {
		return 5 // glyph + "NN%", no fixed-width padding
	}
	return wheelWidth
}

// inputWidth is the width of one client's input line, leaving room for the
// wheel column.
func (m *Model) inputWidth() int {
	w := m.width - m.wheelWidthNow() - 2
	if w < 0 {
		w = 0
	}
	return w
}

// blankInputRows is what the shared frame shows where each client's private
// input rows will be spliced in.
func (m *Model) blankInputRows() string {
	row := strings.Repeat(" ", m.inputWidth())
	rows := make([]string, m.inputRows())
	for i := range rows {
		rows[i] = row
	}
	return strings.Join(rows, "\n")
}

// clientLabel names a client for transcript prefixes and the bottom line.
func (m *Model) clientLabel(client int) string {
	for _, c := range m.clients {
		if c.ID == client {
			return c.Label
		}
	}
	return "you"
}

// userPrefix is the transcript prefix for a user line: "you> " with a
// single terminal, "<label>> " for every sender once several are attached.
func (m *Model) userPrefix(client int) string {
	if len(m.clients) > 1 {
		return m.clientLabel(client) + "> "
	}
	return "you> "
}

// clientLabels joins the attached terminals' labels for the bottom line,
// truncated with an ellipsis to fit the room left on that row.
func (m *Model) clientLabels(room int) string {
	labels := make([]string, 0, len(m.clients))
	for _, c := range m.clients {
		labels = append(labels, c.Label)
	}
	joined := strings.Join(labels, ", ")
	if r := []rune(joined); room > 1 && len(r) > room {
		joined = string(r[:room-1]) + "…"
	}
	return joined
}

// inputRow is the input area plus the context wheel at its right. In a
// served session the frame is shared by every terminal, so the input rows
// are left blank for the host to splice each client's own line into, and
// the wheel goes on the last of them — keeping the blank region one
// unbroken rectangle and the wheel directly above the status line.
func (m *Model) inputRow() string {
	if !m.served {
		return lipgloss.JoinHorizontal(lipgloss.Top, m.inputFor(0).View(), " "+m.wheelView())
	}
	rows := strings.Split(m.blankInputRows(), "\n")
	rows[len(rows)-1] += " " + m.wheelView()
	return strings.Join(rows, "\n")
}
