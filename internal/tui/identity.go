package tui

import (
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/inbox"
	"github.com/brown-enterprises/be-code/internal/live"
)

// Who each terminal is, for chat and DMs (spec §3). The session resolves an
// identity when a terminal attaches; a terminal that needs a prompt gets it
// the first time it opens /chat, /inbox or /dm, never before.

type identity struct {
	ID      string
	How     string
	Choices []string
}

// terminalOf is what the host told us about a client.
func terminalOf(c live.ClientInfo) inbox.Terminal {
	return inbox.Terminal{IP: c.IP, Login: c.Login, User: c.User, PID: c.PID}
}

// resolvedID is one terminal's resolution, with the error to show if any.
type resolvedID struct {
	id  identity
	err error
}

// resolveNew resolves every terminal in infos that has no identity yet,
// without holding mu (see SetClients). A terminal already in s.ids is
// skipped, so a roster that repeats an id costs nothing.
func (s *Session) resolveNew(infos []live.ClientInfo) map[int]resolvedID {
	s.mu.Lock()
	resolve, path := s.resolveFn, s.usersPath
	var todo []live.ClientInfo
	for _, c := range infos {
		if _, done := s.ids[c.ID]; !done {
			todo = append(todo, c)
		}
	}
	s.mu.Unlock()
	if len(todo) == 0 {
		return nil
	}
	if resolve == nil {
		resolve = func(tm inbox.Terminal) (inbox.Resolution, error) {
			return inbox.Resolve(path, tm, inbox.MACFor)
		}
	}
	out := make(map[int]resolvedID, len(todo))
	for _, c := range todo {
		r, err := resolve(terminalOf(c))
		out[c.ID] = resolvedID{id: identity{ID: r.ID, How: r.How, Choices: r.Choices}, err: err}
	}
	return out
}

// recordIdentityLocked stores a resolution. Caller holds mu. An error is a
// transcript line once; the terminal is then treated as unresolved.
func (s *Session) recordIdentityLocked(client int, r resolvedID) {
	if _, done := s.ids[client]; done {
		return
	}
	if r.err != nil {
		s.appendEntryLocked(entry{Kind: entryDim, Text: "chat identity: " + r.err.Error()})
	}
	s.ids[client] = r.id
}

// terminalFor is what the roster knows about one client. ok is false when
// the roster has no such terminal — which means there is no address to bind
// a name to, and binding one anyway would write an empty device row: an ID
// usable from nowhere.
func (s *Session) terminalFor(client int) (inbox.Terminal, bool) {
	for _, c := range s.clients {
		if c.ID == client {
			return terminalOf(c), true
		}
	}
	return inbox.Terminal{}, false
}

// bindCmdLocked validates a chosen or typed name for a client and returns
// the command that records it in users.json. The caller holds mu.
//
// Everything decidable here is decided here — the ID's validity, the
// terminal it belongs to, which bind seam applies — because inbox.Bind takes
// a cross-process file lock and may wait on another session host. That wait
// must not happen under mu, which every terminal's Update holds for its
// whole body, so it goes out as a tea.Cmd and comes back as a nameBoundMsg.
func (s *Session) bindCmdLocked(client int, name string) (tea.Cmd, error) {
	id, err := inbox.ValidID(name)
	if err != nil {
		return nil, err
	}
	tm, ok := s.terminalFor(client)
	if !ok {
		return nil, errors.New("no terminal to bind")
	}
	bind, path := s.bindFn, s.usersPath
	if bind == nil {
		bind = func(id string, tm inbox.Terminal) error { return inbox.Bind(path, id, tm, "") }
	}
	return func() tea.Msg {
		return nameBoundMsg{client: client, id: id, err: bind(id, tm)}
	}, nil
}

// recordBoundLocked records a name a bind has just written. Caller holds mu.
func (s *Session) recordBoundLocked(client int, id string) {
	s.ids[client] = identity{ID: id, How: "asked"}
}

// usedFrom is the address a name is already bound to when that address is
// not this terminal's own (spec §9): two people, or one person on two
// machines, choosing the same ID. "" means the name is free here, and a
// mailbox this session cannot read is not evidence of anything.
func (s *Session) usedFrom(client int, id string) string {
	if s.usedFromFn != nil {
		return s.usedFromFn(id)
	}
	if s.usersPath == "" {
		return ""
	}
	u, err := inbox.Load(s.usersPath)
	if err != nil || u.Users[id] == nil {
		return ""
	}
	tm, _ := s.terminalFor(client)
	other := ""
	for _, d := range u.Users[id].Devices {
		if d.IP == tm.IP {
			return "" // already this terminal's own name
		}
		if other == "" {
			other = d.IP
		}
	}
	return other
}

func (s *Session) identityOf(client int) identity { return s.ids[client] }

// userID is this terminal's resolved ID, "" when it has none yet.
func (m *View) userID() string { return m.identityOf(m.id).ID }

// needName runs then when this terminal has an ID, else prompts for one and
// runs then once it is bound.
func (m *View) needName(then func(*View) (tea.Model, tea.Cmd)) (tea.Model, tea.Cmd) {
	if m.userID() != "" {
		return then(m)
	}
	m.afterName = then
	m.nameChoices = m.identityOf(m.id).Choices
	m.nameSel, m.nameErr, m.nameShared = 0, "", ""
	m.clearSelection() // the transcript this prompt covers is not selectable from here
	m.mode = modeName
	m.input.Reset()
	m.input.Placeholder = "your name for chat and DMs"
	return m, nil
}

func (m *View) handleNameKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.Type {
	case tea.KeyEsc:
		m.afterName = nil
		return m.leaveMode()
	case tea.KeyUp:
		if m.nameSel > 0 {
			m.nameSel--
		}
		return m, nil
	case tea.KeyDown:
		if m.nameSel < len(m.nameChoices) { // len(choices) = "type a name" row
			m.nameSel++
		}
		return m, nil
	case tea.KeyEnter:
		name := strings.TrimSpace(m.input.Value())
		if name == "" && m.nameSel < len(m.nameChoices) {
			name = m.nameChoices[m.nameSel]
		}
		id, err := inbox.ValidID(name)
		if err != nil {
			m.nameErr = err.Error()
			return m, nil
		}
		// Spec §9: a name already used from another address is shared only
		// deliberately. The warning is shown once for that name; the same
		// name entered again goes through, a different one is checked afresh.
		if m.nameShared != id {
			if from := m.usedFrom(m.id, id); from != "" {
				m.nameErr = fmt.Sprintf("%s is already used from %s; pick another or press Enter to share it", id, from)
				m.nameShared = id
				return m, nil
			}
		}
		cmd, err := m.bindCmdLocked(m.id, id)
		if err != nil {
			m.nameErr = err.Error()
			return m, nil
		}
		m.nameErr = ""
		return m, cmd
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return m, cmd
}

// nameBoundMsg is the answer to a bind that ran off the session lock (see
// bindCmdLocked): the terminal it was for, the name, and whether the mailbox
// took it.
type nameBoundMsg struct {
	client int
	id     string
	err    error
}

// nameBound finishes the naming prompt once the bind has returned. Like
// every other update it runs under mu.
func (m *View) nameBound(msg nameBoundMsg) (tea.Model, tea.Cmd) {
	if msg.client != m.id || m.mode != modeName {
		return m, nil // this terminal has moved on, or the message is another's
	}
	if msg.err != nil {
		m.nameErr = msg.err.Error()
		return m, nil
	}
	m.recordBoundLocked(msg.client, msg.id)
	m.input.Reset()
	m.input.Placeholder = inputPlaceholder
	m.nameErr, m.nameShared = "", ""
	then := m.afterName
	m.afterName = nil
	m.mode = m.idleMode()
	if then != nil {
		return then(m)
	}
	return m, nil
}

func (m *View) viewName() string {
	var b strings.Builder
	b.WriteString(m.st.ModalTi.Render("Your name for chat and DMs") + "\n\n")
	for i, c := range m.nameChoices {
		cur := "  "
		if i == m.nameSel {
			cur = "> "
		}
		b.WriteString(cur + c + m.st.Dim.Render("  (seen from this address)") + "\n")
	}
	cur := "  "
	if m.nameSel == len(m.nameChoices) {
		cur = "> "
	}
	b.WriteString(cur + "type a name below\n")
	if m.nameErr != "" {
		b.WriteString("\n" + m.st.Err.Render(m.nameErr) + "\n")
	}
	body := m.st.Border.Width(m.width - 4).Render(b.String())
	return body + "\n" + m.inputView() + "\n" + m.st.Dim.Render(" Enter choose · Esc cancel")
}

// whoami is /whoami.
func (m *View) whoami() {
	if m.chatOff() {
		return
	}
	id := m.identityOf(m.id)
	if id.ID == "" {
		m.appendEntryLocked(entry{Kind: entryDim, Text: "no name yet; /chat, /inbox or /dm will ask"})
		return
	}
	how := map[string]string{"config": "from config", "ip": "bound to " + m.clientIP(), "mac": "recognised by MAC", "asked": "asked this session"}[id.How]
	m.appendEntryLocked(entry{Kind: entryDim, Text: fmt.Sprintf("you are %s (%s)", id.ID, how)})
}

func (m *View) clientIP() string {
	for _, c := range m.clients {
		if c.ID == m.id {
			return c.IP
		}
	}
	return ""
}

// attachedIDs is the chat ID of each terminal in infos that has one, read
// under mu: s.ids is written by binds from any terminal's Update.
func (s *Session) attachedIDs(infos []live.ClientInfo) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	ids := make([]string, 0, len(infos))
	for _, c := range infos {
		if id := s.ids[c.ID].ID; id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}
