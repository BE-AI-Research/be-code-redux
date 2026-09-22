package tui

import (
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

// bindLocked records a chosen or typed name for a client.
func (s *Session) bindLocked(client int, name string) error {
	id, err := inbox.ValidID(name)
	if err != nil {
		return err
	}
	var tm inbox.Terminal
	for _, c := range s.clients {
		if c.ID == client {
			tm = terminalOf(c)
		}
	}
	bind := s.bindFn
	if bind == nil {
		bind = func(id string, tm inbox.Terminal) error { return inbox.Bind(s.usersPath, id, tm, "") }
	}
	if err := bind(id, tm); err != nil {
		return err
	}
	s.ids[client] = identity{ID: id, How: "asked"}
	return nil
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
	m.nameSel, m.nameErr = 0, ""
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
		if err := m.bindLocked(m.id, name); err != nil {
			m.nameErr = err.Error()
			return m, nil
		}
		m.input.Reset()
		m.input.Placeholder = inputPlaceholder
		then := m.afterName
		m.afterName = nil
		m.mode = m.idleMode()
		if then != nil {
			return then(m)
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return m, cmd
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
