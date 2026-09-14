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
// In a shared session the popup belongs to the client that opened it and
// lists only that client's own queued messages — nobody edits anyone else's
// draft — so the rows are a filtered view of the agent's queue and carry
// their index in the full queue with them.

// ownerQueue returns the queue owner's messages and their positions in the
// agent's full queue, so Remove still addresses the right message.
func (m *Model) ownerQueue() (texts []string, idx []int) {
	for i, it := range m.ag.Items() {
		if it.From == m.queueOwner {
			texts = append(texts, it.Text)
			idx = append(idx, i)
		}
	}
	return texts, idx
}

func (m *Model) openQueue(from int) (tea.Model, tea.Cmd) {
	m.queueOwner = from
	items, _ := m.ownerQueue()
	if len(items) == 0 {
		m.appendEntry(entry{Kind: entryDim, Text: "no queued messages (type while the agent works and press Enter to queue one)"})
		return m, nil
	}
	m.ag.Hold(true)
	m.queueCursor = 0
	m.mode = modeQueue
	return m, nil
}

func (m *Model) closeQueue() {
	m.ag.Hold(false)
	m.mode = m.idleMode()
}

func (m *Model) handleQueueKey(k tea.KeyMsg, from int) (tea.Model, tea.Cmd) {
	if from != m.queueOwner {
		// The popup belongs to the client that opened it; everyone else
		// keeps typing into their own input line (see handleGuestKey).
		return m.handleGuestKey(k, from)
	}
	items, idx := m.ownerQueue()
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
		owner := m.queueOwner
		text, ok := m.ag.Remove(idx[m.queueCursor])
		m.closeQueue()
		if !ok {
			m.appendEntry(entry{Kind: entryDim, Text: "that message was already delivered"})
			return m, nil
		}
		in := m.inputFor(owner)
		in.SetValue(text)
		in.CursorEnd()
		m.appendEntry(entry{Kind: entryDim, Text: "editing queued message (paused): Enter re-queues it, Esc keeps it out of the queue"})
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
func (m *Model) dropQueued(i int) (tea.Model, tea.Cmd) {
	text, ok := m.ag.Remove(i)
	if !ok {
		m.appendEntry(entry{Kind: entryDim, Text: "that message was already delivered"})
	} else {
		m.appendEntry(entry{Kind: entryDim, Text: "dropped queued message: " + firstLineOf(text, 80)})
	}
	left, _ := m.ownerQueue()
	if len(left) == 0 {
		m.closeQueue()
	} else if m.queueCursor >= len(left) {
		m.queueCursor = len(left) - 1
	}
	return m, nil
}

func (m *Model) queueBox() string {
	items, _ := m.ownerQueue()
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
