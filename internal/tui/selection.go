package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// Mouse selection over the transcript. Coordinates are lines and columns of
// the wrapped transcript (what the viewport shows), so a selection stays
// put while new output streams in below it. Rendering highlights the
// selected span in reverse video on plain (uncoloured) lines.

type selection struct {
	anchorLine, anchorCol int
	headLine, headCol     int
	dragging, moved       bool
}

// ordered returns the selection bounds with the start before the end;
// the end column is inclusive.
func (s *selection) ordered() (l1, c1, l2, c2 int) {
	l1, c1, l2, c2 = s.anchorLine, s.anchorCol, s.headLine, s.headCol
	if l2 < l1 || (l2 == l1 && c2 < c1) {
		l1, c1, l2, c2 = l2, c2, l1, c1
	}
	return
}

// plainLines is the wrapped transcript with styling removed.
func (m *View) plainLines() []string {
	return strings.Split(ansi.Strip(m.wrapped), "\n")
}

// transcriptCoords maps a mouse position to (line, col) in the wrapped
// transcript, or ok=false when the pointer is outside the transcript.
func (m *View) transcriptCoords(x, y int) (line, col int, ok bool) {
	row := y - m.headerHeight()
	if row < 0 || row >= m.vp.Height || x < 0 {
		return 0, 0, false
	}
	return m.vp.YOffset + row, x, true
}

// handleMouse routes one mouse event. Like a keystroke it is always this
// terminal's own: the selection it makes, and the popup it opens, belong to
// this view alone.
func (m *View) handleMouse(msg tea.MouseMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.Button == tea.MouseButtonRight && msg.Action == tea.MouseActionPress:
		if m.mode == modeInput || m.mode == modeBusy {
			return m.openContextMenu()
		}
		return m, nil
	case msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress:
		if line, col, ok := m.transcriptCoords(msg.X, msg.Y); ok {
			m.sel = &selection{anchorLine: line, anchorCol: col, headLine: line, headCol: col, dragging: true}
			m.refreshTranscript()
		}
		return m, nil
	case msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionMotion:
		if m.sel != nil && m.sel.dragging {
			if line, col, ok := m.transcriptCoords(msg.X, msg.Y); ok {
				m.sel.headLine, m.sel.headCol, m.sel.moved = line, col, true
				m.refreshTranscript()
			}
		}
		return m, nil
	case msg.Action == tea.MouseActionRelease:
		if m.sel != nil && m.sel.dragging {
			m.sel.dragging = false
			if line, col, ok := m.transcriptCoords(msg.X, msg.Y); ok && (line != m.sel.anchorLine || col != m.sel.anchorCol) {
				m.sel.headLine, m.sel.headCol, m.sel.moved = line, col, true
			}
			if !m.sel.moved {
				m.sel = nil // a click, not a drag
			}
			m.refreshTranscript()
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// selectionText extracts the selected text from the plain transcript.
func (m *View) selectionText() string {
	if m.sel == nil {
		return ""
	}
	lines := m.plainLines()
	l1, c1, l2, c2 := m.sel.ordered()
	var out []string
	for i := l1; i <= l2 && i < len(lines); i++ {
		r := []rune(lines[i])
		start, end := 0, len(r)
		if i == l1 {
			start = min(c1, len(r))
		}
		if i == l2 {
			end = min(c2+1, len(r))
		}
		if start > end {
			start = end
		}
		out = append(out, strings.TrimRight(string(r[start:end]), " "))
	}
	return strings.Join(out, "\n")
}

// highlighted renders the wrapped transcript with the selection in reverse
// video; unselected lines keep their colours.
func (m *View) highlighted() string {
	if m.sel == nil {
		return m.wrapped
	}
	colored := strings.Split(m.wrapped, "\n")
	plain := m.plainLines()
	l1, c1, l2, c2 := m.sel.ordered()
	hl := lipgloss.NewStyle().Reverse(true)
	for i := l1; i <= l2 && i < len(plain) && i < len(colored); i++ {
		r := []rune(plain[i])
		start, end := 0, len(r)
		if i == l1 {
			start = min(c1, len(r))
		}
		if i == l2 {
			end = min(c2+1, len(r))
		}
		if start > end {
			start = end
		}
		colored[i] = string(r[:start]) + hl.Render(string(r[start:end])) + string(r[end:])
	}
	return strings.Join(colored, "\n")
}

func (m *View) clearSelection() {
	if m.sel != nil {
		m.sel = nil
		m.refreshTranscript()
	}
}

func (m *View) selectAll() {
	lines := m.plainLines()
	last := len(lines) - 1
	if last < 0 {
		return
	}
	m.sel = &selection{anchorLine: 0, anchorCol: 0, headLine: last, headCol: max(0, len([]rune(lines[last]))-1), moved: true}
	m.refreshTranscript()
}

// copyText sends text to the clipboard and confirms in the transcript.
func (m *View) copyText(text, what string) {
	if strings.TrimSpace(text) == "" {
		m.appendEntryLocked(entry{Kind: entryDim, Text: "nothing to copy (" + what + ")"})
		return
	}
	if err := m.clipboardWrite(text); err != nil {
		m.appendEntryLocked(entry{Kind: entryError, Label: "copy failed: ", Text: err.Error()})
		return
	}
	m.appendEntryLocked(entry{Kind: entryDim, Text: fmt.Sprintf("copied %d chars (%s)", len([]rune(text)), what)})
}

// copyTarget implements /copy [selection|reply|tool|all].
func (m *View) copyTarget(target string) {
	switch strings.ToLower(strings.TrimSpace(target)) {
	case "", "selection", "sel":
		if m.sel != nil {
			m.copyText(m.selectionText(), "selection")
			m.clearSelection()
			return
		}
		if target != "" {
			m.appendEntryLocked(entry{Kind: entryDim, Text: "no selection; drag over the transcript first"})
			return
		}
		m.copyText(m.lastReply, "last reply")
	case "reply", "answer", "last":
		m.copyText(m.lastReply, "last reply")
	case "tool", "output":
		m.copyText(m.lastTool, "last tool output")
	case "all", "transcript":
		m.copyText(strings.Join(m.plainLines(), "\n"), "transcript")
	default:
		m.appendEntryLocked(entry{Kind: entryDim, Text: "usage: /copy [selection|reply|tool|all]"})
	}
}

// ---- right-click context menu ------------------------------------------------

// openContextMenu opens the right-click copy/paste popup. Like the menu it
// acts for this terminal: a paste lands in this terminal's draft.
func (m *View) openContextMenu() (tea.Model, tea.Cmd) {
	var items []pickItem
	if m.sel != nil {
		items = append(items, pickItem{id: "sel", label: "Copy selection", desc: fmt.Sprintf("%d chars", len([]rune(m.selectionText())))})
	}
	items = append(items,
		pickItem{id: "reply", label: "Copy last reply", desc: "the model's last answer"},
		pickItem{id: "tool", label: "Copy last tool output", desc: "full output of the last tool call"},
		pickItem{id: "all", label: "Select all", desc: "highlight the whole transcript"},
		pickItem{id: "paste", label: "Paste into input", desc: "system clipboard → input box"},
	)
	if m.sel != nil {
		items = append(items, pickItem{id: "clear", label: "Clear selection", desc: ""})
	}
	m.prevMode = m.mode
	m.picker = &picker{title: "Copy / paste", items: items, inline: true,
		onPick: func(m *View, it pickItem) (tea.Model, tea.Cmd) {
			m.mode = m.idleMode()
			switch it.id {
			case "sel":
				m.copyText(m.selectionText(), "selection")
				m.clearSelection()
			case "reply", "tool", "all":
				if it.id == "all" {
					m.selectAll()
				} else {
					m.copyTarget(it.id)
				}
			case "paste":
				text, err := m.clipboardRead()
				if err != nil {
					m.appendEntryLocked(entry{Kind: entryError, Label: "paste failed: ", Text: err.Error()})
				} else {
					m.input.SetValue(m.input.Value() + text)
					m.input.CursorEnd()
				}
			case "clear":
				m.clearSelection()
			}
			return m, nil
		}}
	m.mode = modeContextMenu
	return m, nil
}

// idleMode is where to return after a popup: busy if a run is in progress.
func (m *View) idleMode() mode {
	if m.running {
		return modeBusy
	}
	return modeInput
}

func (m *View) contextMenuBox() string {
	p := m.picker
	var b strings.Builder
	b.WriteString(m.st.ModalTi.Render(p.title) + m.st.Dim.Render("  ↑↓ pick · Enter · Esc close") + "\n")
	b.WriteString(m.renderPickList(p, m.popupRows(8), m.omitPopupDesc()))
	return m.st.Border.Width(m.width - 4).Render(b.String())
}
