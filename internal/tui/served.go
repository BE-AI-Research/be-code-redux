package tui

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"github.com/brown-enterprises/be-code/internal/live"
)

// clientsMsg carries the attached-terminal list from the session host.
type clientsMsg []live.ClientInfo

// idleTickMsg drives the live_idle_limit check in served mode.
type idleTickMsg time.Time

// idleTickInterval is the poll period for the live_idle_limit check.
// A package var (rather than a literal in idleTick) so tests can shrink it
// instead of genuinely sleeping 30s.
var idleTickInterval = 30 * time.Second

func idleTick() tea.Cmd {
	return tea.Tick(idleTickInterval, func(t time.Time) tea.Msg { return idleTickMsg(t) })
}

// seedFromHost seeds the client roster and the ascii flag from the host's
// state at the moment RunServed starts. This is the one place in served
// mode allowed to call back into the host directly (Clients/AnyASCII):
// it runs before OnSize/OnClients/OnQuit are registered, so there is no
// callback-reentrancy risk, and it is the only way to see clients that
// attached to the host before this program existed (the host is listening
// and serving before RunServed is called; see internal/live/host.go).
func (m *Model) seedFromHost(h *live.Host) {
	m.clients = h.Clients()
	m.ascii = h.AnyASCII()
}

// RunServed runs the program over a session host instead of a terminal:
// keystrokes arrive tagged with the client that typed them (a key pump per
// client turns raw bytes into Bubble Tea messages), frames go to every
// attached client, and sizes arrive as WindowSizeMsg from the host.
func (m *Model) RunServed(ctx context.Context, h *live.Host) error {
	defer pinColorProfile()()
	m.rootCtx = ctx
	m.host = h
	m.served = true
	m.idleSince = time.Now()
	m.seedFromHost(h)
	m.detachClient = h.Detach
	m.setOverlay = h.SetOverlay
	m.termWrite = func(s string) { io.WriteString(h.Output(), s) }
	m.clipboardWrite = func(s string) error { io.WriteString(h.Output(), osc52(s)); return writeClipboardTools(s) }
	if m.cfg.ThemeTerminalColors {
		m.termWrite(terminalColorSeq(m.cfg.Theme))
		defer m.termWrite(terminalColorReset())
	}
	// No terminal input of its own: every keystroke comes in over the socket
	// tagged with its sender (see OnInput below).
	p := tea.NewProgram(m, tea.WithInput(nil), tea.WithOutput(h.Output()),
		tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithoutSignalHandler())
	m.program = p
	// The pump's emit runs on its own goroutines (never the update
	// goroutine), so p.Send from it cannot deadlock the loop.
	pump := live.NewKeyPump("xterm-256color", func(msg tea.Msg) { p.Send(msg) })
	defer pump.Close()
	m.dropKeyClient = pump.Drop
	h.OnInput(pump.Feed)
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

// pinColorProfile forces lipgloss to render ANSI-256 colour and returns a
// func that restores the previous profile. Served mode needs this: lipgloss
// detects its profile from the process's own os.Stdout, which in the host is
// the <code>.log file, not a terminal — so it would pick the Ascii profile
// and strip every style from output that is in fact bound for real
// terminals over the socket. ANSI-256 is what the themes are written in
// (theme.go uses 256-colour codes).
func pinColorProfile() func() {
	prev := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	return func() { lipgloss.SetColorProfile(prev) }
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
			m.appendLine(stDim.Render("attached: " + c.Label))
		}
	}
	for _, c := range prev {
		if hasClient(m.clients, c.ID) {
			continue
		}
		m.appendLine(stDim.Render("detached: " + c.Label))
		// A terminal that has gone leaves no draft, no half-typed escape
		// sequence and no popup of its own behind it.
		m.dropInput(c.ID)
		if m.dropKeyClient != nil {
			m.dropKeyClient(c.ID)
		}
		if m.mode == modePalette && m.paletteOwner == c.ID {
			m.picker = nil
			m.mode = m.idleMode()
		}
		if (m.mode == modeMenu || m.mode == modeContextMenu) && m.menuOwner == c.ID {
			m.picker = nil
			m.mode = m.idleMode()
		}
		if m.mode == modeQueue && m.queueOwner == c.ID {
			m.closeQueue()
		}
	}
}

// overlayFor renders one client's private input rows as absolute-positioned
// terminal output. Rows are padded to the input width rather than cleared,
// because the shared wheel column sits to their right; the sequence ends by
// parking the cursor at the bottom-right corner so this client's own cursor
// never blinks in the middle of another client's draft.
func (m *Model) overlayFor(client int) string {
	ta := m.inputFor(client)
	lines := strings.Split(ta.View(), "\n")
	var b strings.Builder
	first := m.height - m.inputRows()
	w := m.inputWidth()
	for r := 0; r < m.inputRows(); r++ {
		line := ""
		if r < len(lines) {
			line = lines[r]
		}
		fmt.Fprintf(&b, "\x1b[%d;1H%s", first+r, padToWidth(line, w))
	}
	fmt.Fprintf(&b, "\x1b[%d;%dH", m.height, m.width)
	return b.String()
}

// padToWidth pads s with spaces to w display columns, or truncates it if it
// is already wider. Display width (not byte or rune count) matters here:
// the textarea's rendered line may carry ANSI styling.
func padToWidth(s string, w int) string {
	if w <= 0 {
		return ""
	}
	cur := lipgloss.Width(s)
	if cur > w {
		return ansi.Truncate(s, w, "")
	}
	return s + strings.Repeat(" ", w-cur)
}

// overlayVisible reports whether the current mode's View() renders the
// input row at all. A full-screen modal (approval, picker, menu, plan)
// replaces the whole frame with no reserved input row — overlayFor's
// absolute positioning (always height-inputRows) would land on the modal's
// own content instead, painting a client's stray draft over it.
func (m *Model) overlayVisible() bool {
	switch m.mode {
	case modeApproval, modePicker, modeMenu, modePlan:
		return false
	}
	return true
}

// publishOverlay sends one client's current input rows to the host, if this
// session is served, a publisher is wired up (nil in-process and in tests
// that don't care), and the current mode actually renders an input row.
func (m *Model) publishOverlay(client int) {
	if m.served && m.setOverlay != nil && m.overlayVisible() {
		m.setOverlay(client, m.overlayFor(client))
	}
}

// publishAllOverlays republishes every attached client's overlay: after a
// resize (row/column positions moved), a roster change (a dropped client's
// textarea must not be resurrected by a stray inputFor, so this walks
// m.clients, never m.inputs), or anything else that mutated every client's
// textarea at once (see startTurn, focusInputs, the /plan command). A no-op
// while the current mode hides the input row (see overlayVisible); Update's
// wrapper republishes everyone the moment such a mode gives the row back.
func (m *Model) publishAllOverlays() {
	if !m.served || m.setOverlay == nil || !m.overlayVisible() {
		return
	}
	for _, c := range m.clients {
		m.setOverlay(c.ID, m.overlayFor(c.ID))
	}
}

// clearAllOverlays clears every roster client's cached overlay at the host.
// Called the instant overlayVisible flips to false (see
// publishVisibilityChange in tui.go): Host.SetOverlay("") replaces whatever
// draft was last published there with an empty string, and an empty
// overlay is never re-appended by Host.fanout.Write — unlike
// publishAllOverlays, this must run unconditionally on the mode that hides
// the input row, so it does not gate on overlayVisible().
func (m *Model) clearAllOverlays() {
	if !m.served || m.setOverlay == nil {
		return
	}
	for _, c := range m.clients {
		m.setOverlay(c.ID, "")
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
