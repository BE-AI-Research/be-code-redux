package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// The queue popup lets the user change their mind about messages queued
// during a run: edit one (it is pulled out of the queue into the input,
// paused until re-sent), drop one, or just look. While the popup is open
// the agent's delivery is held so the list cannot shift underneath.
//
// In a shared session the queue is the agent's, one for everyone, but the
// popup lists only the messages this terminal queued — nobody edits anyone
// else's draft — so the rows are a filtered view of that queue and carry
// their index in the full queue with them.

// myQueued returns this terminal's queued messages and their positions in
// the agent's full queue, so Remove still addresses the right message.
func (m *View) myQueued() (texts []string, idx []int) {
	for i, it := range m.ag.Items() {
		if it.From == m.id {
			texts = append(texts, it.Text)
			idx = append(idx, i)
		}
	}
	return texts, idx
}

func (m *View) openQueue() (tea.Model, tea.Cmd) {
	items, _ := m.myQueued()
	if len(items) == 0 {
		m.appendEntryLocked(entry{Kind: entryDim, Text: "no queued messages (type while the agent works and press Enter to queue one)"})
		return m, nil
	}
	m.holdQueueLocked(m.id, true)
	m.queueCursor = 0
	m.mode = modeQueue
	return m, nil
}

func (m *View) closeQueue() {
	m.holdQueueLocked(m.id, false)
	m.mode = m.idleMode()
}

func (m *View) handleQueueKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	items, idx := m.myQueued()
	if len(items) == 0 {
		m.closeQueue()
		return m, nil
	}
	if m.queueCursor >= len(items) {
		m.queueCursor = len(items) - 1
	}
	switch k.Type {
	case tea.KeyEsc, tea.KeyCtrlC:
		m.closeQueue()
		return m, nil
	case tea.KeyUp:
		if m.queueCursor > 0 {
			m.queueCursor--
		}
		return m, nil
	case tea.KeyDown:
		if m.queueCursor < len(items)-1 {
			m.queueCursor++
		}
		return m, nil
	case tea.KeyEnter:
		text, ok := m.ag.Remove(idx[m.queueCursor])
		m.closeQueue()
		if !ok {
			m.appendEntryLocked(entry{Kind: entryDim, Text: "that message was already delivered"})
			return m, nil
		}
		m.input.SetValue(text)
		m.input.CursorEnd()
		m.appendEntryLocked(entry{Kind: entryDim, Text: "editing queued message (paused): Enter re-queues it, Esc keeps it out of the queue"})
		return m, nil
	case tea.KeyDelete, tea.KeyBackspace:
		return m.dropQueued(idx[m.queueCursor])
	case tea.KeyRunes:
		if len(k.Runes) == 1 && (k.Runes[0] == 'd' || k.Runes[0] == 'x') {
			return m.dropQueued(idx[m.queueCursor])
		}
	}
	return m, nil
}

// dropQueued removes the message at index i of the agent's full queue.
func (m *View) dropQueued(i int) (tea.Model, tea.Cmd) {
	text, ok := m.ag.Remove(i)
	if !ok {
		m.appendEntryLocked(entry{Kind: entryDim, Text: "that message was already delivered"})
	} else {
		m.appendEntryLocked(entry{Kind: entryDim, Text: "dropped queued message: " + firstLineOf(text, 80)})
	}
	left, _ := m.myQueued()
	if len(left) == 0 {
		m.closeQueue()
	} else if m.queueCursor >= len(left) {
		m.queueCursor = len(left) - 1
	}
	return m, nil
}

func (m *View) queueBox() string {
	items, _ := m.myQueued()
	var b strings.Builder
	b.WriteString(m.st.ModalTi.Render("queued messages") + m.st.Dim.Render("  ↑↓ pick · Enter edit · d drop · Esc close") + "\n")
	maxRows := m.popupRows(len(items))
	start := 0
	if m.queueCursor >= maxRows {
		start = m.queueCursor - maxRows + 1
	}
	for i := start; i < len(items) && i < start+maxRows; i++ {
		it := items[i]
		line := fmt.Sprintf("%d  %s", i+1, firstLineOf(it, m.width-14))
		if i == m.queueCursor {
			b.WriteString(m.st.Accent.Render("> "+line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	return m.st.Border.Width(m.width - 4).Render(strings.TrimRight(b.String(), "\n"))
}

func firstLineOf(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + "…"
	}
	if max > 4 && len([]rune(s)) > max {
		s = string([]rune(s)[:max-1]) + "…"
	}
	return s
}
