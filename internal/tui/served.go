package tui

import (
	"context"
	"io"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/live"
)

// clientsMsg carries the attached-terminal list from the session host.
type clientsMsg []live.ClientInfo

// idleTickMsg drives the live_idle_limit check in served mode.
type idleTickMsg time.Time

func idleTick() tea.Cmd {
	return tea.Tick(30*time.Second, func(t time.Time) tea.Msg { return idleTickMsg(t) })
}

// RunServed runs the program over a session host instead of a terminal:
// keystrokes come from the host's input pipe, frames go to every attached
// client, and sizes arrive as WindowSizeMsg from the host.
func (m *Model) RunServed(ctx context.Context, h *live.Host) error {
	m.rootCtx = ctx
	m.host = h
	m.served = true
	m.idleSince = time.Now()
	m.detachHolder = h.DetachHolder
	m.termWrite = func(s string) { io.WriteString(h.Output(), s) }
	m.clipboardWrite = func(s string) error { io.WriteString(h.Output(), osc52(s)); return writeClipboardTools(s) }
	if m.cfg.ThemeTerminalColors {
		m.termWrite(terminalColorSeq(m.cfg.Theme))
		defer m.termWrite(terminalColorReset())
	}
	p := tea.NewProgram(m, tea.WithInput(h.InputReader()), tea.WithOutput(h.Output()),
		tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithoutSignalHandler())
	m.program = p
	h.OnSize(func(cols, rows int) { p.Send(tea.WindowSizeMsg{Width: cols, Height: rows}) })
	h.OnClients(func(cl []live.ClientInfo) { p.Send(clientsMsg(cl)) })
	h.OnQuit(func() { p.Send(tea.Quit()) })
	if c, r := h.Size(); c > 0 {
		go p.Send(tea.WindowSizeMsg{Width: c, Height: r})
	}
	if m.cfg.LiveIdleLimit > 0 {
		go p.Send(idleTickMsg(time.Now()))
	}
	_, err := p.Run()
	m.histFile.save()
	return err
}

// hasClient reports whether id is present in list.
func hasClient(list []live.ClientInfo, id int) bool {
	for _, c := range list {
		if c.ID == id {
			return true
		}
	}
	return false
}

// updateClients handles clientsMsg: it records the new roster, notes
// whether any attached client cannot render UTF-8 glyphs, and appends
// attach/detach lines for the difference from the previous roster.
func (m *Model) updateClients(msg clientsMsg) {
	prev := m.clients
	m.clients = []live.ClientInfo(msg)
	m.ascii = false
	for _, c := range m.clients {
		if !c.UTF8 {
			m.ascii = true
		}
	}
	for _, c := range m.clients {
		if !hasClient(prev, c.ID) {
			note := "attached: " + c.Label
			if c.Holder {
				note += ", now holding input"
			}
			m.appendLine(stDim.Render(note))
		}
	}
	for _, c := range prev {
		if !hasClient(m.clients, c.ID) {
			m.appendLine(stDim.Render("detached: " + c.Label))
		}
	}
}

// updateIdleTick handles idleTickMsg: in served mode with a configured
// live_idle_limit, a session with no attached clients and no run in
// progress quits once the limit has passed; a client or a run resets the
// idle clock.
func (m *Model) updateIdleTick(msg idleTickMsg) (tea.Model, tea.Cmd) {
	if !m.served || m.cfg.LiveIdleLimit <= 0 {
		return m, nil
	}
	if len(m.clients) > 0 || m.running {
		m.idleSince = time.Time(msg)
	} else if time.Time(msg).Sub(m.idleSince) >= time.Duration(m.cfg.LiveIdleLimit)*time.Minute {
		return m, tea.Quit
	}
	return m, idleTick()
}
