package tui

import (
	"fmt"
	"strings"
	"time"

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
	// No session-wide toast: several terminals may be attached under
	// different identities, and a DM is for one of them. Each view raises
	// its own toast when it sees the message is for its ID.
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
	// listVP is the /inbox thread list. It is a viewport for the same reason
	// the room and a thread are: a hundred correspondents rendered straight
	// into the frame push the input row and the footer off the bottom of the
	// terminal.
	listVP viewport.Model
	// online is the last answer to "is `with` attached anywhere", and when
	// it was taken: View renders on the cursor blink, and reading every
	// live record each frame is a disk scan for nothing.
	online   bool
	onlineAt time.Time
}

// onlineTTL is how long a thread's online answer is trusted.
const onlineTTL = 5 * time.Second

// withOnline is online(with), cached per thread for onlineTTL.
func (m *View) withOnline() bool {
	if m.dm.onlineAt.IsZero() || m.now().Sub(m.dm.onlineAt) > onlineTTL {
		m.dm.online, m.dm.onlineAt = m.online(m.dm.with), m.now()
	}
	return m.dm.online
}

// reloadThreads re-derives this terminal's thread list from the mailbox:
// when it opens /inbox or /dm, when it opens or marks a thread read, when a
// new message arrives for it, and once in NewView so the bottom-line badge
// is right from the first frame. The read mark is per correspondent (spec
// §5.1, amended), so a reload right after marking one thread read leaves
// every other thread's badge exactly as the disk has it.
//
// This runs under the session lock, and may: it is a bounded read of local
// files with no cross-process lock taken — unlike inbox.Bind, whose flock
// waits on other hosts and is therefore driven from a tea.Cmd instead (see
// handleNameKey).
func (m *View) reloadThreads() {
	m.dm.threads, _ = inbox.Threads(m.inboxDir, m.userID())
}

// noMailbox reports whether this session has no inbox directory — chat is on
// but ~/.be-code/inbox could not be opened — saying so once when it has not.
func (m *View) noMailbox() bool {
	if m.inboxDir == "" {
		m.appendEntryLocked(entry{Kind: entryDim, Text: "DMs are unavailable: no inbox directory"})
		return true
	}
	return false
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
	if m.chatOff() || m.noMailbox() {
		return m, nil
	}
	return m.needName(func(m *View) (tea.Model, tea.Cmd) {
		m.reloadThreads()
		m.dm.sel = 0
		m.mode = modeInbox
		m.clearSelection() // the transcript this mode covers is not selectable from here
		m.input.Reset()
		m.layoutInbox()
		return m, nil
	})
}

func (m *View) enterDM(with string) (tea.Model, tea.Cmd) {
	if m.chatOff() || m.noMailbox() {
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
		m.mode = modeDM
		m.openThread(id)
		m.clearSelection() // the transcript this mode covers is not selectable from here
		m.input.Reset()
		m.input.Placeholder = "message " + id + "… (/back to return)"
		return m, nil
	})
}

// openThread loads a thread and marks it read up to its newest line. The
// read mark is per correspondent, so the other threads' badges are exactly
// what the disk says after a reload.
func (m *View) openThread(with string) {
	if with != m.dm.with {
		m.dm.onlineAt = time.Time{}
	}
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

// inboxHeaderRows is how many rows renderInbox puts above the first thread
// (the title and the blank line under it), so a selected row can be found in
// the viewport's own coordinates.
const inboxHeaderRows = 2

// layoutInbox sizes the thread list the same way layoutChat sizes the room —
// everything above the input rows and the footer — and keeps the selected
// row on screen.
func (m *View) layoutInbox() {
	h := m.height - m.inputRows() - 2
	if h < 3 {
		h = 3
	}
	m.dm.listVP.Width, m.dm.listVP.Height = m.width, h
	m.dm.listVP.SetContent(m.renderInbox())
	row := inboxHeaderRows + m.dm.sel
	switch {
	case row < m.dm.listVP.YOffset:
		m.dm.listVP.SetYOffset(row)
	case row >= m.dm.listVP.YOffset+h:
		m.dm.listVP.SetYOffset(row - h + 1)
	}
}

func (m *View) renderInbox() string {
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
		// Runes, not bytes: a snippet cut mid-rune renders as a replacement
		// character, and one cut by byte count is the wrong length in any
		// language that needs more than one byte a letter.
		snippet := []rune(strings.SplitN(t.Latest.Text, "\n", 2)[0])
		room := m.width - 30
		if room > 80 {
			room = 80
		}
		if room < 0 {
			room = 0
		}
		if len(snippet) > room {
			if room == 0 {
				snippet = nil
			} else {
				snippet = append(snippet[:room-1:room-1], '…')
			}
		}
		b.WriteString(fmt.Sprintf("%s%s %-16s %s  %s\n", cur, dot, t.With, m.st.Dim.Render(t.Latest.TS.Format("15:04")), string(snippet)))
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *View) viewInbox() string {
	m.layoutInbox()
	footer := " inbox · Esc"
	if !m.compact() {
		footer = " inbox · Enter open · d mark read · Esc back"
	}
	return m.dm.listVP.View() + "\n" + m.inputView() + "\n" + m.footerLine(footer)
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
	if !m.withOnline() {
		name += " (not online)"
	}
	footer := " dm " + name + " · Esc back"
	if m.compact() {
		footer = " dm " + name + " · Esc"
	}
	if !m.showContacts() {
		// The hint stays in compact layout: with the contact column hidden,
		// Tab is the only way to reach another thread.
		footer += " · Tab"
		if !m.compact() {
			footer += " next thread"
		}
	}
	return body + "\n" + m.inputView() + "\n" + m.footerLine(footer)
}
