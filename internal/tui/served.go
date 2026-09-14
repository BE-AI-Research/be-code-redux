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

// hostQuitMsg is the host asking the program to stop (a quit frame from a
// client, or a signal the host turned into one). It goes through Update
// rather than being a bare tea.Quit so the same teardown as /quit runs:
// every client's overlay is cleared before Bubble Tea's own final frame.
type hostQuitMsg struct{}

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
	m.switchClient = h.Switch
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
	// Not tea.Quit() directly: Bubble Tea answers a QuitMsg in its event
	// loop without ever showing it to Update, and this quit has to clear
	// every client's overlay on its way out (see hostQuitMsg).
	h.OnQuit(func() { p.Send(hostQuitMsg{}) })
	if c, r := h.Size(); c > 0 {
		go p.Send(tea.WindowSizeMsg{Width: c, Height: r})
	}
	if m.cfg.LiveIdleLimit > 0 {
		go p.Send(idleTickMsg(time.Now()))
	}
	_, err := p.Run()
	m.clearAllOverlays() // nothing is rendered any more; the host's closing lines follow
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
// attach/detach lines for the difference from the previous roster. It
// returns tea.Quit when this host has nothing left to do (see the switch
// below), or nil.
func (m *Model) updateClients(msg clientsMsg) tea.Cmd {
	prev := m.clients
	m.clients = []live.ClientInfo(msg)
	m.ascii = false
	for _, c := range m.clients {
		if !c.UTF8 {
			m.ascii = true
		}
	}
	if len(m.clients) > 0 {
		// Someone is still watching, so the switch that set this flag did
		// not empty the session. Clearing it here, not only on an attach,
		// keeps a later ordinary detach from quitting a host whose client
		// was told it is still running.
		m.switchPending = false
	}
	for _, c := range m.clients {
		if !hasClient(prev, c.ID) {
			m.appendLine(m.st.Dim.Render("attached: " + c.Label))
		}
	}
	for _, c := range prev {
		if hasClient(m.clients, c.ID) {
			continue
		}
		m.appendLine(m.st.Dim.Render("detached: " + c.Label))
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
	// A terminal that switched to another session leaves this host behind.
	// A fresh session nobody ever typed into, with nobody left watching, is
	// exactly the empty host the switch created — it quits rather than
	// accumulating. A session with turns in it keeps running: its work is
	// worth coming back to with be-code attach. So does one with a run in
	// flight, whose first turn has not reached the session file yet.
	if m.switchPending && !m.running && len(m.clients) == 0 &&
		m.ag.Session != nil && len(m.ag.Session.Messages) == 0 {
		return tea.Quit
	}
	return nil
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
	first := m.inputTop()
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

// inputTop is the 1-based terminal row of the first input row, derived from
// the same layout View composes: header, then the transcript viewport, then
// the input block. It is not height-inputRows: layout() keeps one slack row
// under the bottom line, so anchoring to the terminal height lands one row
// too low and the overlay erases the bottom line.
func (m *Model) inputTop() int {
	return m.headerHeight() + m.vp.Height + 1
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
// that don't care), the model has a size to lay them out against, and the
// current mode actually renders an input row.
//
// The m.ready gate matters: the host replays whatever a client typed before
// the program registered OnInput, so a key can genuinely arrive before the
// first WindowSizeMsg. Publishing then would place the rows by a zero-width
// layout, at row 1 of the terminal, over the frame the program is about to
// draw. The first WindowSizeMsg republishes the whole roster anyway.
func (m *Model) publishOverlay(client int) {
	if m.served && m.ready && m.setOverlay != nil && m.overlayVisible() {
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
	if !m.served || !m.ready || m.setOverlay == nil || !m.overlayVisible() {
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
// the input row (and on the way into a quit), so it gates on neither
// overlayVisible() nor m.ready: clearing an overlay that was never
// published is free, and one that was must go.
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
		m.clearAllOverlays() // see the /quit path: nothing may ride on the teardown frame
		return m, tea.Quit
	}
	return m, idleTick()
}
