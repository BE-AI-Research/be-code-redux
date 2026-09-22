package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brown-enterprises/be-code/internal/live"
	"github.com/brown-enterprises/be-code/internal/store"
)

// The room: one per session, every attached terminal sees it, routed by the
// host through the same mailbox the transcript uses. It is not the
// transcript — the model sees it only through an @agent mention
// (mention.go) — and it is saved in the session file beside the transcript.

// roomCap bounds the room; the oldest lines go, once, with a marker.
const roomCap = 2000

// chatMsg is one new room line, broadcast to every view.
type chatMsg struct{ line store.ChatLine }

// Post appends a line to the room and tells every terminal.
func (s *Session) Post(user, text, kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.PostLocked(user, text, kind)
}

// PostLocked is Post for a caller holding mu.
func (s *Session) PostLocked(user, text, kind string) {
	line := store.ChatLine{TS: s.now(), User: user, Text: strings.TrimRight(text, "\n"), Kind: kind}
	s.room = append(s.room, line)
	if len(s.room) > roomCap {
		s.room = append([]store.ChatLine{{TS: line.TS, Text: "(older chat trimmed)"}}, s.room[len(s.room)-roomCap+1:]...)
	}
	room := s.room
	s.ag.UpdateSession(func(ss *store.Session) { ss.Chat = append([]store.ChatLine(nil), room...) })
	s.broadcast(chatMsg{line: line})
}

// Room is a snapshot of the room.
func (s *Session) Room() []store.ChatLine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.ChatLine(nil), s.room...)
}

// restoreRoom puts a saved room back (resume).
func (s *Session) restoreRoom(lines []store.ChatLine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.room = append([]store.ChatLine(nil), lines...)
}

// chatNameOf is how a roster entry signs a room line: its declared chat
// identity if the terminal sent one, else its device label. Used for the
// join/leave lines SetClients posts, where there is a ClientInfo but no View.
func (s *Session) chatNameOf(c live.ClientInfo) string {
	if c.User != "" {
		return c.User
	}
	return live.LabelKey(c.Label)
}

// chatName is how this terminal signs a room line. Task 8 replaces the
// body with the resolved user ID.
func (m *View) chatName() string {
	for _, c := range m.clients {
		if c.ID == m.id && c.User != "" {
			return c.User
		}
	}
	return live.LabelKey(m.label)
}

// enterChat opens the room on this terminal. Nothing shared changes: the run
// keeps running, the other terminals keep whatever they were doing.
func (m *View) enterChat() (tea.Model, tea.Cmd) {
	if !m.cfg.Chat.Enabled {
		m.appendEntryLocked(entry{Kind: entryDim, Text: "chat is disabled in config (chat.enabled)"})
		return m, nil
	}
	if !m.joinedChat {
		m.joinedChat = true
		if m.joined == nil {
			m.joined = map[int]bool{}
		}
		m.joined[m.id] = true
		m.PostLocked("", m.chatName()+" joined", "join")
	}
	m.mode = modeChat
	m.chatUnseen = 0
	m.input.Reset()
	m.input.Placeholder = "message the room… (/back to return)"
	m.layoutChat()
	m.chatVP.GotoBottom()
	return m, nil
}

// leaveMode returns this terminal from chat, inbox or dm to the transcript.
func (m *View) leaveMode() (tea.Model, tea.Cmd) {
	m.mode = m.idleMode()
	m.input.Reset()
	m.input.Placeholder = inputPlaceholder
	return m, nil
}

func (m *View) handleChatKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.Type {
	case tea.KeyEsc:
		if m.input.Value() == "" {
			return m.leaveMode()
		}
		m.input.Reset()
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
		m.PostLocked(m.chatName(), text, "")
		return m, nil
	case tea.KeyPgUp:
		m.chatVP.HalfViewUp()
		return m, nil
	case tea.KeyPgDown:
		m.chatVP.HalfViewDown()
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return m, cmd
}

// layoutChat sizes the room viewport: everything above the input rows and
// the footer.
func (m *View) layoutChat() {
	h := m.height - m.inputRows() - 2
	if h < 3 {
		h = 3
	}
	m.chatVP.Width, m.chatVP.Height = m.width, h
	m.chatVP.SetContent(m.renderRoom())
}

func (m *View) renderRoom() string {
	var b strings.Builder
	for _, l := range m.room {
		b.WriteString(m.renderChatLine(l))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *View) renderChatLine(l store.ChatLine) string {
	ts := m.st.Dim.Render(l.TS.Format("15:04"))
	switch {
	case l.User == "":
		return ts + " " + m.st.ChatSystem.Render(l.Text)
	case l.User == "agent":
		return ts + " " + m.st.Cowork.Render("agent: ") + wrapTo(l.Text, m.width-15)
	default:
		return ts + " " + m.st.ChatUser.Render(l.User+": ") + wrapTo(l.Text, m.width-8-lipgloss.Width(l.User))
	}
}

func (m *View) viewChat() string {
	here := len(m.clients)
	if here == 0 {
		here = 1
	}
	footer := fmt.Sprintf(" chat · %d here · Esc back", here)
	if m.compact() {
		footer = " chat · Esc"
	}
	if m.mentionBusy != "" {
		footer += m.st.Dim.Render(" · agent is working on " + m.mentionBusy + "'s question")
	}
	return m.chatVP.View() + "\n" + m.inputView() + "\n" + m.st.Dim.Render(footer)
}

// wrapTo wraps text to width columns, the same way the transcript itself is
// wrapped (refreshTranscript): lipgloss's own ANSI-aware algorithm, not a
// hand-rolled one, so a room line with the same content wraps identically to
// everything else on screen.
func wrapTo(text string, width int) string {
	if width < 1 {
		width = 1
	}
	return lipgloss.NewStyle().Width(width).Render(text)
}
