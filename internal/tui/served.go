package tui

import (
	"context"
	"io"
	"runtime/debug"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/brown-enterprises/be-code/internal/live"
)

// One program per attached terminal.
//
// A shared session is one Session and many Views: each terminal runs its own
// Bubble Tea program, rendering its own frame at its own size into its own
// output writer, while the transcript, the agent, the queue, the roster and
// the shared prompts stay on the Session underneath. The runner below is
// what keeps the two in step — it is the only thing that creates and
// destroys programs, and the host's roster is its single source of truth.

// clientsMsg carries the attached-terminal list from the session host.
type clientsMsg []live.ClientInfo

// idleTickInterval is the poll period for the live_idle_limit check.
// A package var (rather than a literal in idleLoop) so tests can shrink it
// instead of genuinely sleeping 30s.
var idleTickInterval = 30 * time.Second

// programStopWait is how long a program is given to answer Quit before it is
// killed. A terminal that has stopped reading must not hold up the teardown
// of every other one.
const programStopWait = 2 * time.Second

// earlyKeyLimit caps the keys buffered for a client whose program has not
// started yet. The host replays what a terminal typed before the program
// existed, and a terminal that pastes into the socket in that window must
// not grow this without bound.
const earlyKeyLimit = 256

// ctrlDepth is how deep one terminal's own message queue runs (see
// program.ctrl). Generous: a paste is thousands of bytes, and dropping a
// keystroke is worse than holding it.
const ctrlDepth = 4096

// program is one attached terminal: its view, the Bubble Tea program
// rendering that view, the mailbox session broadcasts reach it through, its
// own control queue, and a channel closed when the program has returned.
type program struct {
	v  *View
	p  *tea.Program
	mb *mailbox
	// ctrl carries what belongs to this terminal alone — its keystrokes, its
	// mouse events and its size — straight to Bubble Tea, in order.
	//
	// It is deliberately not the mailbox. A mailbox is drained by
	// View.Update itself (mailbox.drainInto), which reaches the model
	// without ever going through the Program — so Bubble Tea's renderer
	// never sees what arrives that way. That is right for a transcript
	// entry and wrong for a size, which the renderer needs in order to
	// repaint in full and to know the width it erases lines to; a size
	// delivered by the drain leaves a terminal showing only the lines that
	// happened to change, with stale text past the end of any line that
	// shrank. Keys go the same way, so that a keystroke and the size that
	// may precede it cannot be reordered against each other.
	ctrl chan tea.Msg
	done chan struct{}
}

// send hands this terminal one of its own messages. It never blocks: the
// queue is deeper than any burst a terminal can produce, and a program too
// wedged to drain it is one stop is about to kill anyway.
func (pr *program) send(msg tea.Msg) {
	select {
	case pr.ctrl <- msg:
	default:
	}
}

// runner owns the set of running programs for a served session. Its own
// mutex guards nothing but that set: nothing under it ever blocks (every
// hand-off to a program is a non-blocking queue send), so it is safe to take
// from the host's notify goroutine, from a key pump goroutine and from a
// program's own exit goroutine alike.
type runner struct {
	s        *Session
	h        *live.Host
	mu       sync.Mutex
	programs map[int]*program
	early    map[int][]tea.Msg // keys that arrived before the program started
	quit     chan struct{}
	quitOnce sync.Once
	// stopping counts the stops running on goroutines of their own (a
	// detached client's program, which onClients has already taken out of
	// programs). RunServed waits on it, or it could return — and let the
	// closing lines be written — while a program is still rendering.
	stopping sync.WaitGroup

	// noPrograms is a test seam: the view, its mailbox and its control queue
	// are built as usual, but no Bubble Tea program is started and no
	// delivery goroutine runs, so a test can drive the views by hand (see
	// the pump helper).
	noPrograms bool
}

// RunServed runs the session over a session host instead of a terminal: one
// program per attached client, each rendering to that client alone, with
// keystrokes arriving over the socket tagged with the client that typed
// them and sizes arriving per client. It returns when the session has quit
// and no program is left running — the invariant the closing lines in
// cmd/live.go depend on, since nothing may render over them.
func (s *Session) RunServed(ctx context.Context, h *live.Host) error {
	defer pinColorProfile()()

	r := &runner{s: s, h: h, programs: map[int]*program{}, early: map[int][]tea.Msg{}, quit: make(chan struct{})}
	pump := live.NewKeyPump("xterm-256color", r.route)
	defer pump.Close()

	s.mu.Lock()
	s.rootCtx, s.served, s.idleSince = ctx, true, time.Now()
	s.detachClient, s.switchClient = h.Detach, h.Switch
	s.dropKeyClient = pump.Drop
	s.onQuit = r.onSessionQuit
	s.mu.Unlock()

	h.OnClientSize(r.onClientSize)
	h.OnClients(r.onClients)
	h.OnQuit(s.Quit)
	h.OnInput(pump.Feed)
	r.onClients(h.Clients()) // clients that attached before this call

	if s.cfg.LiveIdleLimit > 0 {
		go r.idleLoop()
	}
	<-r.quit    // closed by the last program exiting after Quit, or by the idle/empty rules
	r.stopAll() // quits any program still running and waits for it
	s.histFile.save()
	return nil
}

// RunLocal runs the session in this process's own terminal (alt screen) and
// blocks until exit. One view, one program: the in-process TUI is client 0,
// and it reads the real stdin rather than a socket.
func (s *Session) RunLocal(ctx context.Context) error {
	s.mu.Lock()
	s.rootCtx = ctx
	s.mu.Unlock()
	v := s.NewView(0, "local")
	if s.cfg.ThemeTerminalColors {
		v.termWrite(terminalColorSeq(v.st.Name()))
		defer v.termWrite(terminalColorReset())
	}
	p := tea.NewProgram(v, tea.WithAltScreen(), tea.WithMouseCellMotion())
	defer s.retireView(v)
	go v.mb.run(p.Send)
	_, err := p.Run()
	s.histFile.save()
	return err
}

// onClients is the single source of attach and detach: every program is
// started and stopped from the host's roster, so the set of programs can
// never drift from the set of terminals.
func (r *runner) onClients(infos []live.ClientInfo) {
	r.s.SetClients(infos)
	r.mu.Lock()
	defer r.mu.Unlock()
	// A session on its way out starts nothing new: a program born after
	// RunServed stopped waiting would render over the closing lines, and
	// stopAll would never take it down. Read under r.mu, which stopAll takes
	// to swap the map, so a quit landing between the roster and here cannot
	// slip a program past both. (Lock order is r.mu → s.mu, as it already is
	// for startLocked's NewView and emptyAfterSwitch below.)
	quitting := r.s.isQuitting()
	seen := map[int]bool{}
	for _, c := range infos {
		seen[c.ID] = true
		// A terminal already running is left alone. It used to be re-sent its
		// own size here, because the host announced every roster change with
		// an FSize frame and a client clears its screen on one — so every
		// terminal had to be made to repaint in full. FSize is gone (a roster
		// change is an FClients frame now, and no client clears on that), and
		// the re-send was not free: Bubble Tea treats *any* WindowSizeMsg as a
		// full repaint, dropping its frame cache, so one terminal attaching or
		// resizing rewrote every other terminal's whole screen — some 5 KiB
		// down each socket — for a frame that had not changed. A view still
		// gets its size when its program starts (startLocked) and whenever it
		// genuinely changes or the host asks for a repaint (onClientSize).
		if _, ok := r.programs[c.ID]; !ok && !quitting {
			r.startLocked(c)
		}
	}
	for id, pr := range r.programs {
		if !seen[id] {
			// On a goroutine: stop waits on the program, and this runs under
			// the host's notify lock. Counted, because the program is out of
			// r.programs from here on and neither programExited nor stopAll
			// can see it any more.
			r.stopping.Add(1)
			go func(pr *program, id int) {
				defer r.stopping.Done()
				r.stop(pr, id)
			}(pr, id)
			delete(r.programs, id)
		}
	}
	if len(infos) == 0 && r.s.emptyAfterSwitch() {
		r.s.Quit()
	}
}

// startLocked brings up one terminal's program. The caller holds r.mu.
func (r *runner) startLocked(c live.ClientInfo) {
	v := r.s.NewView(c.ID, c.Label)
	v.ascii = !c.UTF8 // this terminal's own glyph capability, not the roster's
	pr := &program{v: v, mb: v.mb, ctrl: make(chan tea.Msg, ctrlDepth), done: make(chan struct{})}
	r.programs[c.ID] = pr
	if r.noPrograms {
		close(pr.done) // a test drives this view by hand; see the pump helper
	} else {
		w := r.h.ClientOutput(c.ID)
		v.termWrite = func(s string) { io.WriteString(w, s) }
		v.clipboardWrite = func(s string) error { io.WriteString(w, osc52(s)); return writeClipboardTools(s) }
		p := tea.NewProgram(v, tea.WithInput(nil), tea.WithOutput(w), tea.WithAltScreen(),
			tea.WithMouseCellMotion(), tea.WithoutSignalHandler(), tea.WithoutCatchPanics())
		pr.p = p
		go pr.mb.run(func(msg tea.Msg) { p.Send(msg) })
		// This terminal's own messages, in order, on a goroutine of their
		// own: p.Send blocks until the program's update loop takes each one.
		go func() {
			for msg := range pr.ctrl {
				p.Send(msg)
			}
		}()
		if r.s.cfg.ThemeTerminalColors {
			v.termWrite(terminalColorSeq(v.st.Name()))
		}
		go func() {
			// Order matters, and defers run last-first: the panic is caught,
			// then done is closed, and only then is the exit counted — a
			// programExited that ran before done was closed would count this
			// program as still running and never let RunServed return.
			defer r.programExited()
			defer close(pr.done)
			defer func() {
				if rec := recover(); rec != nil {
					r.h.Logf("view %d panicked: %v\n%s", c.ID, rec, debug.Stack())
					r.h.Drop(c.ID, "view error")
				}
			}()
			_, _ = p.Run()
		}()
	}
	pr.send(tea.WindowSizeMsg{Width: c.Cols, Height: c.Rows})
	for _, k := range r.early[c.ID] {
		pr.send(k)
	}
	delete(r.early, c.ID)
}

// stop takes one terminal's program down and lets go of its view. It never
// runs under r.mu: it waits on the program, which may be mid-frame.
func (r *runner) stop(pr *program, id int) {
	if pr.p != nil {
		pr.p.Quit()
		select {
		case <-pr.done:
		case <-time.After(programStopWait):
			pr.p.Kill() // a terminal that stopped reading holds up nobody
			<-pr.done
		}
	}
	// Detach before closing: a broadcast holding a reference to this mailbox
	// would otherwise send on a closed channel (see mailbox.close). The same
	// goes for ctrl, which is why every send to it happens under r.mu while
	// the program is still in r.programs — this runs only once it is not.
	r.s.retireView(pr.v)
	close(pr.ctrl)
}

// route delivers a client's key or mouse event to that client's program.
// It is the key pump's emit, so it runs on the pump's own goroutines.
func (r *runner) route(msg tea.Msg) {
	var id int
	var out tea.Msg
	switch t := msg.(type) {
	case live.ClientKeyMsg:
		id, out = t.Client, t.Key
	case live.ClientMouseMsg:
		id, out = t.Client, t.Mouse
	default:
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if pr, ok := r.programs[id]; ok {
		pr.send(out)
		return
	}
	// The host replays input that arrived before the program existed; hold
	// it until startLocked can deliver it.
	if len(r.early[id]) < earlyKeyLimit {
		r.early[id] = append(r.early[id], out)
	}
}

// onClientSize gives one terminal its own size — on attach, on every resize
// it sends, and as a repaint request after output to it was evicted. There
// is no shared minimum any more: each program lays out for its own terminal.
func (r *runner) onClientSize(id, cols, rows int) {
	// Under r.mu, like route: ctrl is closed by stop once the program has
	// left r.programs, and a send that had already found it would be a send
	// on a closed channel. Nothing here blocks.
	r.mu.Lock()
	defer r.mu.Unlock()
	if pr, ok := r.programs[id]; ok {
		pr.send(tea.WindowSizeMsg{Width: cols, Height: rows})
	}
}

// onSessionQuit is the runner's half of Session.Quit (Session.onQuit),
// always called on a goroutine of its own. Every view is told to stop by
// the quitMsg broadcast; asking each program directly as well covers the
// view whose mailbox was full when that broadcast went out, which would
// otherwise leave RunServed waiting on r.quit for a program that never
// learned to stop. Each Quit goes on its own goroutine because it blocks
// until that program's update loop takes the message.
func (r *runner) onSessionQuit() {
	r.mu.Lock()
	prs := make([]*program, 0, len(r.programs))
	for _, pr := range r.programs {
		prs = append(prs, pr)
	}
	r.mu.Unlock()
	for _, pr := range prs {
		if pr.p != nil {
			go pr.p.Quit()
		}
	}
	// With no program running — an idle quit with nobody attached — this is
	// what lets RunServed return at all.
	r.programExited()
}

// programExited: when the session is quitting and no program is left,
// let RunServed return.
func (r *runner) programExited() {
	if !r.s.isQuitting() {
		return
	}
	r.mu.Lock()
	running := 0
	for _, pr := range r.programs {
		select {
		case <-pr.done:
		default:
			running++
		}
	}
	r.mu.Unlock()
	if running == 0 {
		r.quitOnce.Do(func() { close(r.quit) })
	}
}

// stopAll takes down whatever is still running once the session has quit —
// a program that never answered its quitMsg, or one whose client detached
// at the last moment. After this returns, nothing renders any more, which
// is what makes the host's closing lines safe to write.
func (r *runner) stopAll() {
	r.mu.Lock()
	prs := r.programs
	r.programs = map[int]*program{}
	r.mu.Unlock()
	for id, pr := range prs {
		// The terminal-colour reset belongs here and not in stop: this is
		// the path where the client is still attached (the session is
		// ending, and the host says goodbye afterwards). A stop reached
		// from onClients is a client that has already left the host's
		// roster, so anything written to it is dropped by ClientOutput —
		// the terminal is back at its own shell by then in any case.
		if r.h != nil && r.s.cfg.ThemeTerminalColors {
			io.WriteString(r.h.ClientOutput(id), terminalColorReset())
		}
		r.stop(pr, id)
	}
	// And whatever is still being stopped for a client that detached on the
	// way out: nothing may still be rendering when this returns.
	r.stopping.Wait()
}

// idleLoop enforces live_idle_limit: a served session left with no terminal
// attached and no run in progress ends itself rather than holding the
// workspace open for ever.
func (r *runner) idleLoop() {
	t := time.NewTicker(idleTickInterval)
	defer t.Stop()
	for {
		select {
		case <-r.quit:
			return
		case now := <-t.C:
			if r.s.idleExpired(now) {
				r.s.Quit()
				return
			}
		}
	}
}

// retireView lets go of one terminal's view: it releases whatever that
// terminal was holding on the shared session, then takes the view out of the
// broadcast set and closes its mailbox, in that order — once no broadcast can
// reach the mailbox, closing it is safe and the delivery goroutine ends.
func (s *Session) retireView(v *View) {
	s.mu.Lock()
	// A terminal that has gone cannot close its own queue popup, and the
	// delivery hold that popup took would then stop every later run's
	// queued messages from ever being delivered.
	s.holdQueueLocked(v.id, false)
	s.mu.Unlock()
	s.detachView(v.id)
	v.mb.close()
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
