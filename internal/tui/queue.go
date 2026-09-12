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

func (m *Model) openQueue() (tea.Model, tea.Cmd) {
	items := m.ag.Peek()
	if len(items) == 0 {
		m.appendLine(stDim.Render("no queued messages (type while the agent works and press Enter to queue one)"))
		return m, nil
	}
	m.ag.Hold(true)
	m.queueCursor = 0
	m.mode = modeQueue
	return m, nil
}

func (m *Model) closeQueue() {
	m.ag.Hold(false)
	m.mode = modeBusy
}

func (m *Model) handleQueueKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	items := m.ag.Peek()
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
		text, ok := m.ag.Remove(m.queueCursor)
		m.closeQueue()
		if !ok {
			m.appendLine(stDim.Render("that message was already delivered"))
			return m, nil
		}
		m.input.SetValue(text)
		m.input.CursorEnd()
		m.appendLine(stDim.Render("editing queued message (paused): Enter re-queues it, Esc keeps it out of the queue"))
		return m, nil
	case tea.KeyDelete, tea.KeyBackspace:
		return m.dropQueued(m.queueCursor)
	case tea.KeyRunes:
		if len(k.Runes) == 1 && (k.Runes[0] == 'd' || k.Runes[0] == 'x') {
			return m.dropQueued(m.queueCursor)
		}
	}
	return m, nil
}

func (m *Model) dropQueued(i int) (tea.Model, tea.Cmd) {
	text, ok := m.ag.Remove(i)
	if !ok {
		m.appendLine(stDim.Render("that message was already delivered"))
	} else {
		m.appendLine(stDim.Render("dropped queued message: " + firstLineOf(text, 80)))
	}
	if m.ag.Pending() == 0 {
		m.closeQueue()
	} else if m.queueCursor >= m.ag.Pending() {
		m.queueCursor = m.ag.Pending() - 1
	}
	return m, nil
}

func (m *Model) queueBox() string {
	items := m.ag.Peek()
	var b strings.Builder
	b.WriteString(stModalTi.Render("queued messages") + stDim.Render("  ↑↓ pick · Enter edit · d drop · Esc close") + "\n")
	for i, it := range items {
		line := fmt.Sprintf("%d  %s", i+1, firstLineOf(it, m.width-14))
		if i == m.queueCursor {
			b.WriteString(stAccent.Render("> "+line) + "\n")
		} else {
			b.WriteString("  " + line + "\n")
		}
	}
	return stBorder.Width(m.width - 4).Render(strings.TrimRight(b.String(), "\n"))
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
