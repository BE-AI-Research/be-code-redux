package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brown-enterprises/be-code/internal/inbox"
	"github.com/brown-enterprises/be-code/internal/live"
)

// DMs and the inbox (spec §5, §7). The mailbox is the disk; this file is the
// views over it and the ping when the host's watcher sees a new file. A DM
// never goes near the agent: nothing here imports internal/agent's queue.

// inboxMsg is a new DM for one of this session's terminals, or (with a zero
// Message) a request that every view refresh its unread count — see
// deliverDM and NewView.
type inboxMsg struct{ m inbox.Message }

// deliverDM is the watcher's callback: a transient for the terminals that
// own the recipient ID, and a broadcast so their views update.
func (s *Session) deliverDM(m inbox.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	owned := false
	for _, c := range s.clients {
		if s.ids[c.ID].ID == m.To {
			owned = true
		}
	}
	if !owned {
		return // another host's user, or nobody's yet
	}
	s.toastLocked("DM from " + m.From)
	s.broadcast(inboxMsg{m: m})
}

// online reports whether any session host on this machine has id attached.
func (s *Session) online(id string) bool {
	if s.onlineFn != nil {
		return s.onlineFn(id)
	}
	dir, err := live.Dir()
	if err != nil {
		return false
	}
	recs, _ := live.List(dir)
	for _, r := range recs {
		for _, u := range r.Users {
			if u == id {
				return true
			}
		}
	}
	return false
}

// dmState is one terminal's view state for the inbox and threads.
type dmState struct {
	threads []inbox.ThreadSummary
	sel     int
	with    string
	msgs    []inbox.Message
	vp      viewport.Model
}

// reloadThreads is a genuine refresh from disk: called when this terminal
// explicitly opens /inbox or /dm, or when a new message arrives for it. It
// is deliberately not called by openThread (see below): the mailbox has one
// global read watermark per user (spec §5.1), so re-deriving every thread's
// unread count right after marking one of them read would silently clear an
// older, still-unopened thread's badge along with it.
func (m *View) reloadThreads() {
	m.dm.threads, _ = inbox.Threads(m.inboxDir, m.userID())
}

// unreadDMs is the count on the bottom line.
func (m *View) unreadDMs() int {
	n := 0
	for _, t := range m.dm.threads {
		n += t.Unread
	}
	return n
}

func (m *View) enterInbox() (tea.Model, tea.Cmd) {
	if !m.cfg.Chat.Enabled || m.inboxDir == "" {
		m.appendEntryLocked(entry{Kind: entryDim, Text: "DMs are disabled (chat.enabled, or no inbox directory)"})
		return m, nil
	}
	return m.needName(func(m *View) (tea.Model, tea.Cmd) {
		m.reloadThreads()
		m.dm.sel = 0
		m.mode = modeInbox
		m.input.Reset()
		return m, nil
	})
}

func (m *View) enterDM(with string) (tea.Model, tea.Cmd) {
	if !m.cfg.Chat.Enabled || m.inboxDir == "" {
		m.appendEntryLocked(entry{Kind: entryDim, Text: "DMs are disabled (chat.enabled, or no inbox directory)"})
		return m, nil
	}
	return m.needName(func(m *View) (tea.Model, tea.Cmd) {
		m.reloadThreads()
		if with == "" {
			if len(m.dm.threads) == 0 {
				m.appendEntryLocked(entry{Kind: entryDim, Text: "no messages yet · /dm <name> to send one"})
				return m, nil
			}
			with = m.dm.threads[0].With
		}
		id, err := inbox.ValidID(with)
		if err != nil {
			m.appendEntryLocked(entry{Kind: entryDim, Text: "/dm: " + err.Error()})
			return m, nil
		}
		m.openThread(id)
		m.mode = modeDM
		m.input.Reset()
		m.input.Placeholder = "message " + id + "… (/back to return)"
		return m, nil
	})
}

// openThread loads a thread and marks it read up to its newest line. The
// read mark is per correspondent, so the other threads' badges are exactly
// what the disk says after a reload.
func (m *View) openThread(with string) {
	m.dm.with = with
	m.dm.msgs, _ = inbox.Thread(m.inboxDir, m.userID(), with)
	if n := len(m.dm.msgs); n > 0 {
		_ = inbox.MarkRead(m.inboxDir, m.userID(), with, m.dm.msgs[n-1].TS)
	}
	m.reloadThreads()
	m.dm.sel = 0
	for i, t := range m.dm.threads {
		if t.With == with {
			m.dm.sel = i
		}
	}
	m.layoutDM()
	m.dm.vp.GotoBottom()
}

func (m *View) handleInboxKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	if v := strings.TrimSpace(m.input.Value()); v != "" {
		// A slash command is being typed (started by the "/" rune case
		// below): keys here edit and submit that line, not the thread list,
		// so /back and /dm <name> work from the inbox too.
		switch k.Type {
		case tea.KeyEnter:
			m.input.Reset()
			if strings.HasPrefix(v, "/") {
				return m.slashCommand(v)
			}
			return m, nil
		case tea.KeyEsc:
			m.input.Reset()
			return m, nil
		}
		var cmd tea.Cmd
		m.input, cmd = m.input.Update(k)
		return m, cmd
	}
	switch k.Type {
	case tea.KeyEsc:
		return m.leaveMode()
	case tea.KeyUp:
		if m.dm.sel > 0 {
			m.dm.sel--
		}
	case tea.KeyDown:
		if m.dm.sel < len(m.dm.threads)-1 {
			m.dm.sel++
		}
	case tea.KeyEnter:
		if m.dm.sel < len(m.dm.threads) {
			return m.enterDM(m.dm.threads[m.dm.sel].With)
		}
	case tea.KeyRunes:
		switch string(k.Runes) {
		case "d":
			if m.dm.sel < len(m.dm.threads) {
				t := m.dm.threads[m.dm.sel]
				_ = inbox.MarkRead(m.inboxDir, m.userID(), t.With, t.Latest.TS)
				m.reloadThreads()
			}
		case "/":
			m.input.SetValue("/")
		}
	}
	return m, nil
}

func (m *View) handleDMKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.Type {
	case tea.KeyEsc:
		if m.input.Value() == "" {
			return m.leaveMode()
		}
		m.input.Reset()
		return m, nil
	case tea.KeyTab:
		if n := len(m.dm.threads); n > 1 {
			m.openThread(m.dm.threads[(m.dm.sel+1)%n].With)
		}
		return m, nil
	case tea.KeyEnter:
		text := strings.TrimSpace(m.input.Value())
		m.input.Reset()
		if text == "" {
			return m, nil
		}
		if strings.HasPrefix(text, "/") {
			return m.slashCommand(text)
		}
		if _, err := inbox.Send(m.inboxDir, m.userID(), m.dm.with, text); err != nil {
			m.toastLocked("could not send: " + err.Error())
			return m, nil
		}
		m.openThread(m.dm.with)
		return m, nil
	case tea.KeyPgUp:
		m.dm.vp.HalfViewUp()
		return m, nil
	case tea.KeyPgDown:
		m.dm.vp.HalfViewDown()
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return m, cmd
}

const contactWidth = 18

func (m *View) showContacts() bool { return m.width >= 70 }

func (m *View) layoutDM() {
	w := m.width
	if m.showContacts() {
		w = m.width - contactWidth - 1
	}
	h := m.height - m.inputRows() - 2
	if h < 3 {
		h = 3
	}
	m.dm.vp.Width, m.dm.vp.Height = w, h
	var b strings.Builder
	for _, x := range m.dm.msgs {
		who := x.From
		style := m.st.ChatUser
		if x.From == m.userID() {
			style = m.st.Accent
		}
		b.WriteString(m.st.Dim.Render(x.TS.Format("15:04")) + " " + style.Render(who+": ") + wrapTo(x.Text, w-8-len(who)) + "\n")
	}
	m.dm.vp.SetContent(strings.TrimRight(b.String(), "\n"))
}

func (m *View) viewInbox() string {
	var b strings.Builder
	b.WriteString(m.st.ModalTi.Render("Inbox — "+m.userID()) + "\n\n")
	if len(m.dm.threads) == 0 {
		b.WriteString(m.st.Dim.Render("no messages yet · /dm <name> to send one") + "\n")
	}
	for i, t := range m.dm.threads {
		cur, dot := "  ", " "
		if i == m.dm.sel {
			cur = "> "
		}
		if t.Unread > 0 {
			dot = "●"
		}
		snippet := strings.SplitN(t.Latest.Text, "\n", 2)[0]
		room := m.width - 30
		if room > 0 && len(snippet) > room {
			snippet = snippet[:room-1] + "…"
		}
		b.WriteString(fmt.Sprintf("%s%s %-16s %s  %s\n", cur, dot, t.With, m.st.Dim.Render(t.Latest.TS.Format("15:04")), snippet))
	}
	body := b.String()
	footer := " inbox · Enter open · d mark read · Esc back"
	if m.compact() {
		footer = " inbox · Esc"
	}
	return body + "\n" + m.inputView() + "\n" + m.st.Dim.Render(footer)
}

func (m *View) viewDM() string {
	right := m.dm.vp.View()
	body := right
	if m.showContacts() {
		var col strings.Builder
		for i, t := range m.dm.threads {
			cur, dot := "  ", " "
			if i == m.dm.sel {
				cur = "> "
			}
			if t.Unread > 0 {
				dot = "●"
			}
			name := t.With
			if len(name) > contactWidth-4 {
				name = name[:contactWidth-5] + "…"
			}
			col.WriteString(fmt.Sprintf("%s%s%s\n", cur, dot, name))
		}
		left := lipgloss.NewStyle().Width(contactWidth).Height(m.dm.vp.Height).Render(col.String())
		body = lipgloss.JoinHorizontal(lipgloss.Top, left, m.st.Dim.Render(strings.Repeat("│\n", m.dm.vp.Height)), right)
	}
	name := m.dm.with
	if !m.online(m.dm.with) {
		name += " (not online)"
	}
	footer := " dm " + name + " · Esc back"
	if !m.showContacts() {
		footer += " · Tab next thread"
	}
	return body + "\n" + m.inputView() + "\n" + m.st.Dim.Render(footer)
}
