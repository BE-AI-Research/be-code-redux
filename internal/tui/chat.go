package tui

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/inbox"
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

// roomResetMsg tells every view the room was replaced wholesale — currently
// only /clear, which starts a fresh session with an empty one. A view that
// applied every chatMsg since it attached would already agree with the
// session, but a broadcast makes that explicit and lets a terminal sitting
// in modeChat re-render immediately instead of showing stale lines until its
// next post or resize.
type roomResetMsg struct{}

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
	// A person's own line naming the model (mention.go) becomes a request —
	// tagged here, before the cap and the broadcast, so the saved room, what
	// every terminal sees and the request built from it all agree on what
	// happened. "agent" itself and any system line (join/leave, kind != "")
	// are exempt: an @agent inside the model's own reply is not a request.
	if kind == "" && user != "" && user != "agent" && IsMention(line.Text) {
		line.Kind = "mention"
		s.room[len(s.room)-1].Kind = "mention"
	}
	if len(s.room) > roomCap {
		s.room = append([]store.ChatLine{{TS: line.TS, Text: "(older chat trimmed)"}}, s.room[len(s.room)-roomCap+1:]...)
	}
	room := s.room
	s.ag.UpdateSession(func(ss *store.Session) { ss.Chat = append([]store.ChatLine(nil), room...) })
	s.broadcast(chatMsg{line: line})
	// A person's line naming another person pings that person's transcript
	// — one line stating who and where, never the text — on every terminal
	// attached under their ID (entry.For) and nobody else's. Never for the
	// rest of the room, never for one's own line, never for the model's own
	// reply (its text is not a person's, and it already lands in the
	// transcript), only for an ID attached right now (as a DM prints only
	// for an attached recipient), and not for a user who switched their
	// pings off (chat.pings in the host's config).
	if user != "" && user != inbox.Reserved {
		for _, who := range MentionedUsers(line.Text) {
			if who == user || !s.attachedLocked(who) || !s.cfg.Chat.PingsOn(who) {
				continue
			}
			s.appendEntryLocked(entry{Kind: entryPing, Text: "User replied: " + user + " (chat)", For: who})
		}
	}
	if line.Kind == "mention" {
		// The request must include the mention itself, so this runs after
		// the line has already been appended to the room above.
		s.noteMentionLocked(line)
	}
}

// userMentionRe finds @name in a room line: a chat user ID (inbox.ValidID's
// alphabet), not preceded by a word character, a dot or another @ (an
// e-mail address is not a mention). Case is folded: IDs are lower-case.
var userMentionRe = regexp.MustCompile(`(?i)(^|[^\w.@])@([a-z0-9_.-]+)`)

// MentionedUsers is every user ID a room line names with @, in order,
// once each, trailing punctuation dropped ("@alice." names alice), and
// only what inbox.ValidID accepts as an ID — which excludes "agent", the
// model's own mention (IsMention), and anything over its length.
func MentionedUsers(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range userMentionRe.FindAllStringSubmatch(text, -1) {
		id, err := inbox.ValidID(strings.TrimRight(m[2], ".-_"))
		if err != nil || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// attachedLocked reports whether some terminal is attached under id right
// now. Caller holds mu.
func (s *Session) attachedLocked(id string) bool {
	for _, c := range s.clients {
		if s.ids[c.ID].ID == id {
			return true
		}
	}
	return false
}

// shows reports whether this terminal renders e: every entry but one that
// is for a user ID this terminal is not attached under.
func (m *View) shows(e entry) bool {
	return e.For == "" || e.For == m.userID()
}

// chatPings is /chat pings [on|off]: this terminal's user's own switch for
// the transcript line a mention prints, saved in the host's config so every
// terminal of theirs through this host agrees. Reached from Update, so mu
// is held; the save goes through config.Update (a file write, not a
// broadcast), never through the session's own Config, which carries
// session-only approvals under keys the file has.
func (m *View) chatPings(args []string) (tea.Model, tea.Cmd) {
	if m.chatOff() {
		return m, nil
	}
	return m.needName(func(m *View) (tea.Model, tea.Cmd) {
		me := m.userID()
		state := func() string {
			if m.cfg.Chat.PingsOn(me) {
				return "on"
			}
			return "off"
		}
		if len(args) == 0 {
			return m.sayLocal(entryDim, "chat pings are "+state()+" for "+me+" (/chat pings on|off)")
		}
		var on bool
		switch args[0] {
		case "on":
			on = true
		case "off":
			on = false
		default:
			return m.sayLocal(entryWarn, "usage: /chat pings on|off")
		}
		if m.cfg.Chat.Pings == nil {
			m.cfg.Chat.Pings = map[string]bool{}
		}
		m.cfg.Chat.Pings[me] = on
		confirm := "chat pings " + state() + " for " + me
		err := config.Update(func(c *config.Config) {
			if c.Chat.Pings == nil {
				c.Chat.Pings = map[string]bool{}
			}
			c.Chat.Pings[me] = on
		})
		if err != nil {
			return m.sayLocal(entryWarn, confirm+" for this session; could not save config: "+err.Error())
		}
		return m.sayLocal(entryOK, confirm)
	})
}

// sayLocal tells this terminal one thing: a transcript line of its own when
// the transcript is on screen, a toast of its own when the room is (a line
// written under a hidden transcript is one nobody sees).
func (m *View) sayLocal(kind entryKind, text string) (tea.Model, tea.Cmd) {
	if m.mode != modeChat {
		m.renderEntryLocal(entry{Kind: kind, Text: text})
		return m, nil
	}
	m.ownToast, m.ownToastUntil = text, m.now().Add(toastFor)
	return m, tea.Tick(toastFor+100*time.Millisecond, func(t time.Time) tea.Msg { return toastTickMsg(t) })
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
	if id := s.ids[c.ID].ID; id != "" {
		return id // the resolved name, which may have been asked for after attach
	}
	if c.User != "" {
		return c.User
	}
	return live.LabelKey(c.Label)
}

// chatName is how this terminal signs a room line: its resolved chat
// identity. The fallback to the device label only matters before that
// identity exists, which cannot happen once needName has run — enterChat
// asks for one before the room ever opens.
func (m *View) chatName() string {
	if id := m.userID(); id != "" {
		return id
	}
	return live.LabelKey(m.label)
}

// chatOff reports whether chat is switched off in config, saying so once on
// this terminal when it is. Every entry point asks before anything else —
// above all before the naming prompt, since a terminal must not be asked who
// it is for a feature it cannot use.
func (m *View) chatOff() bool {
	if !m.cfg.Chat.Enabled {
		m.appendEntryLocked(entry{Kind: entryDim, Text: "chat is disabled in config"})
		return true
	}
	return false
}

// enterChat opens the room on this terminal, asking for a name first if this
// terminal does not have one yet. Nothing shared changes: the run keeps
// running, the other terminals keep whatever they were doing.
func (m *View) enterChat() (tea.Model, tea.Cmd) {
	if m.chatOff() {
		return m, nil
	}
	return m.needName(func(m *View) (tea.Model, tea.Cmd) {
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
		// A selection belongs to the transcript this mode covers: left alive
		// it would keep highlighting lines nobody can see, and Ctrl+C in the
		// room would copy them instead of clearing the draft.
		m.clearSelection()
		m.input.Reset()
		m.input.Placeholder = "message the room… (/back to return)"
		m.layoutChat()
		m.chatVP.GotoBottom()
		return m, nil
	})
}

// modeViewport is the viewport the mode this terminal is in scrolls, or nil
// when that is the transcript's own (every other mode). It is what keeps a
// wheel event in the room off the hidden transcript — see handleMouse.
func (m *View) modeViewport() *viewport.Model {
	switch m.mode {
	case modeChat:
		return &m.chatVP
	case modeInbox:
		return &m.dm.listVP
	case modeDM:
		return &m.dm.vp
	}
	return nil
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
	case tea.KeyRunes:
		if m.input.Value() == "" && string(k.Runes) == "/" {
			return m.openPalette("")
		}
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
	footer := " chat · Esc"
	if !m.compact() {
		footer = fmt.Sprintf(" chat · %d here · Esc back", here)
		if !m.cfg.Chat.PingsOn(m.userID()) {
			footer += " · pings off"
		}
		if m.mentionBusy != "" {
			footer += " · agent is working on " + m.mentionBusy + "'s question"
		}
	}
	return m.chatVP.View() + "\n" + m.inputView() + "\n" + m.footerLine(footer)
}

// footerLine is the last row of the room, the inbox and a DM thread: cut to
// this terminal's width with an ellipsis and padded out to it. Both halves
// matter — a footer one cell too wide wraps the row and scrolls the whole
// frame up on a phone-sized terminal, and one short of the width leaves
// whatever the previous frame drew in those cells on screen.
func (m *View) footerLine(text string) string {
	if m.width <= 0 {
		return ""
	}
	if lipgloss.Width(text) > m.width {
		text = ansi.Truncate(text, m.width, "…")
	}
	return m.st.Dim.Render(padToWidth(text, m.width))
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
