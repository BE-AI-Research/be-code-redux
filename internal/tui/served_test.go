package tui

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"github.com/brown-enterprises/be-code/internal/live"
)

// With two clients the bottom line counts them and names them all: there
// is no holder any more, so nobody is singled out.
func TestClientsMsgRendersRoster(t *testing.T) {
	m := newTestModel(t)
	// UTF8: true on both — this test is about the labels, not the ASCII
	// fallback (see TestASCIIFallbacks in compact_test.go); m.ascii has a
	// reader (the clients marker glyph), so a fixture that leaves UTF8 at its
	// zero value would render the ASCII marker instead.
	setClients(m, live.ClientInfo{ID: 1, Label: "vscode (pid 1)", UTF8: true}, live.ClientInfo{ID: 2, Label: "ssh from 10.0.0.5 (pid 2)", UTF8: true})
	v := m.View()
	for _, want := range []string{"⧉ 2", "vscode (pid 1), ssh from 10.0.0.5 (pid 2)"} {
		if !strings.Contains(v, want) {
			t.Fatalf("bottom line lacks %q:\n%s", want, v)
		}
	}
	for _, gone := range []string{"input:", "Ctrl+] d", "Ctrl+] t"} {
		if strings.Contains(v, gone) {
			t.Fatalf("holder text %q survives:\n%s", gone, v)
		}
	}
	if !strings.Contains(m.rendered.String(), "attached: ssh from 10.0.0.5 (pid 2)") {
		t.Fatalf("no attach line:\n%s", m.rendered.String())
	}
	setClients(m, live.ClientInfo{ID: 1, Label: "vscode (pid 1)"})
	if !strings.Contains(m.rendered.String(), "detached: ssh from 10.0.0.5 (pid 2)") {
		t.Fatal("no detach line")
	}
	if strings.Contains(m.View(), "⧉") {
		t.Fatal("marker shown with a single client")
	}
}

// /clients lists clients; /detach asks the host to drop the terminal the
// command was typed on — which, with one program per terminal, is this
// view's own client.
func TestClientsAndDetachCommands(t *testing.T) {
	m := newTestModel(t)
	m.id = 1
	setClients(m, live.ClientInfo{ID: 1, Label: "local (pid 1)"})
	m.slashCommand("/clients")
	flush(m)
	if !strings.Contains(m.rendered.String(), "local (pid 1)") {
		t.Fatal("/clients did not list")
	}
	detached := make(chan int, 1)
	m.detachClient = func(id int) { detached <- id }
	_, cmd := m.slashCommand("/detach")
	if cmd == nil {
		t.Fatal("/detach returned no command")
	}
	cmd() // Bubble Tea runs this on its own goroutine
	select {
	case id := <-detached:
		if id != 1 {
			t.Fatalf("detached client %d, want the one that typed /detach", id)
		}
	case <-time.After(time.Second):
		t.Fatal("/detach did not call the host")
	}
}

// tempSockDir is a short-pathed temp dir for unix sockets. t.TempDir() embeds
// the test name under a long per-run prefix, which on macOS overruns the
// 104-byte sun_path limit and makes Listen fail with "invalid argument".
func tempSockDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "bl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// /clients on an in-process (never served) session says so, rather than
// confusing "no clients" with "not served" (the two used to be conflated
// by testing len(m.clients) == 0 alone).
func TestClientsNotServedMessage(t *testing.T) {
	m := newTestModel(t)
	m.served = false
	m.clients = nil
	m.slashCommand("/clients")
	flush(m)
	if !strings.Contains(m.rendered.String(), "not served") {
		t.Fatalf("expected a not-served note:\n%s", m.rendered.String())
	}
}

// /clients on a served session with nobody currently attached reports that
// distinctly from the not-served case.
func TestClientsServedEmptyMessage(t *testing.T) {
	m := newTestModel(t)
	m.served = true
	m.clients = nil
	m.slashCommand("/clients")
	flush(m)
	if !strings.Contains(m.rendered.String(), "no terminals attached") {
		t.Fatalf("expected a no-terminals-attached note:\n%s", m.rendered.String())
	}
	if strings.Contains(m.rendered.String(), "not served") {
		t.Fatal("served-but-empty must not say \"not served\"")
	}
}

// With live_idle_limit set, a served session with no clients and no run in
// progress expires once the limit has passed; a client or a run resets it.
// The runner's idleLoop polls this; the decision itself is the session's.
func TestIdleLimitQuitsWhenUnattachedAndIdle(t *testing.T) {
	s := newTestSession(t)
	s.cfg.LiveIdleLimit = 1
	s.served = true
	s.clients = nil
	s.running = false
	s.idleSince = time.Now().Add(-2 * time.Minute)
	if !s.idleExpired(time.Now()) {
		t.Fatal("expected the idle limit to have expired")
	}

	// A run in progress keeps the session alive and resets the clock.
	s.idleSince = time.Now().Add(-2 * time.Minute)
	s.running = true
	now := time.Now()
	if s.idleExpired(now) {
		t.Fatal("expired while a run is in progress")
	}
	if s.idleSince.Before(now.Add(-time.Second)) {
		t.Fatal("running did not reset idleSince")
	}

	// So does an attached terminal.
	s.running = false
	s.clients = []live.ClientInfo{{ID: 1, Label: "desk"}}
	s.idleSince = time.Now().Add(-2 * time.Minute)
	if s.idleExpired(time.Now()) {
		t.Fatal("expired with a terminal attached")
	}

	// An in-process session, or one with no limit configured, never expires.
	s.clients = nil
	s.idleSince = time.Now().Add(-2 * time.Minute)
	s.served = false
	if s.idleExpired(time.Now()) {
		t.Fatal("an in-process session must never expire")
	}
	s.served = true
	s.cfg.LiveIdleLimit = 0
	if s.idleExpired(time.Now()) {
		t.Fatal("no live_idle_limit means no expiry")
	}
}

// The runner is what turns a roster into programs: one per attached
// terminal, keys routed to the view of the client that typed them, keys for
// a client with no program yet buffered until it has one, and a detached
// client's program taken down.
func TestRunnerStartsOneProgramPerClientAndStopsOnDetach(t *testing.T) {
	s := newTestSession(t)
	s.served = true
	r := &runner{s: s, programs: map[int]*program{}, early: map[int][]tea.Msg{}, quit: make(chan struct{}), noPrograms: true}
	r.onClients([]live.ClientInfo{{ID: 1, Label: "a", Cols: 80, Rows: 24, UTF8: true}, {ID: 2, Label: "b", Cols: 40, Rows: 15, UTF8: true}})
	if len(r.programs) != 2 {
		t.Fatalf("programs: %d", len(r.programs))
	}
	r.route(live.ClientKeyMsg{Client: 2, Key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")}})
	r.route(live.ClientKeyMsg{Client: 3, Key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("early")}})
	pump(r.programs[1], r.programs[2])
	if r.programs[2].v.input.Value() != "x" || r.programs[1].v.input.Value() != "" {
		t.Fatal("key routed to the wrong view")
	}
	if len(r.early[3]) != 1 {
		t.Fatal("a key for a client with no program yet must be buffered")
	}
	// Each program rendered at its own client's size, not a shared minimum.
	if w, h := r.programs[1].v.width, r.programs[1].v.height; w != 80 || h != 24 {
		t.Fatalf("view 1 is %dx%d", w, h)
	}
	if r.programs[2].v.compact() == r.programs[1].v.compact() {
		t.Fatal("a 40x15 terminal must lay out compact while an 80x24 one does not")
	}
	r.onClients([]live.ClientInfo{{ID: 1, Label: "a", Cols: 80, Rows: 24, UTF8: true}})
	if _, ok := r.programs[2]; ok {
		t.Fatal("detached client's program not removed")
	}
}

// waitForClients polls a live.Host until it reports at least n attached
// clients, or fails the test. Attaching over a real unix socket is
// asynchronous with respect to the test goroutine (the host registers the
// client on its own accept goroutine), so this replaces a fixed sleep.
func waitForClients(t *testing.T, h *live.Host, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(h.Clients()) >= n {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("host never reported %d client(s); have %d", n, len(h.Clients()))
}

// A served program renders for real terminals over a socket, but lipgloss
// detects its colour profile from the host process's own stdout — a log
// file — and would strip every style. RunServed pins the profile instead.
func TestPinColorProfileForcesColourAndRestores(t *testing.T) {
	before := lipgloss.ColorProfile()
	restore := pinColorProfile()
	if got := lipgloss.ColorProfile(); got != termenv.ANSI256 {
		t.Fatalf("pinned profile is %v, want ANSI256", got)
	}
	// A styled string must actually carry an SGR sequence now.
	out := lipgloss.NewStyle().Foreground(lipgloss.Color("205")).Render("x")
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("styled output carries no escape sequence: %q", out)
	}
	restore()
	if got := lipgloss.ColorProfile(); got != before {
		t.Fatalf("profile not restored: %v, want %v", got, before)
	}
}

// TestDetachDoesNotBlockTheUpdateLoop is the deadlock this wave fixed: the
// host's callbacks are p.Send on Bubble Tea's unbuffered message channel, and
// Update runs on the goroutine that receives from it — so calling into the
// host from inside Update blocks forever, holding the host's notify lock, and
// every later attach (and `sessions kill`) hangs with it.
//
// The fake event loop here reproduces exactly that shape: onClients blocks
// until something receives, and nothing does while "Update" (slashCommand) is
// running.
func TestDetachDoesNotBlockTheUpdateLoop(t *testing.T) {
	dir := tempSockDir(t)
	sock := filepath.Join(dir, "s.sock")
	h := live.NewHost("tok", io.Discard)
	if err := h.Listen(sock); err != nil {
		t.Fatal(err)
	}
	go h.Serve()
	t.Cleanup(func() { h.Close("test over") })

	// Stands in for tea.Program.Send: an unbuffered channel whose only
	// receiver is the update goroutine.
	msgs := make(chan any)
	h.OnClients(func(cl []live.ClientInfo) { msgs <- clientsMsg(cl) })
	h.OnClientSize(func(id, c, r int) { msgs <- tea.WindowSizeMsg{Width: c, Height: r} })

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	bye := make(chan string, 1)
	go func() {
		for {
			typ, p, err := live.ReadFrame(conn)
			if err != nil {
				return
			}
			if typ == live.FBye {
				var b live.Bye
				json.Unmarshal(p, &b)
				bye <- b.Reason
				return
			}
		}
	}()
	if err := live.WriteJSON(conn, live.FHello, live.Hello{
		Token: "tok", Cols: 80, Rows: 24, Label: "phone", UTF8: true,
	}); err != nil {
		t.Fatal(err)
	}
	waitForClients(t, h, 1)
	id := h.Clients()[0].ID

	m := newTestModel(t)
	m.served = true
	m.id = id
	m.detachClient = h.Detach

	// "Update" runs with nothing draining msgs, exactly as Bubble Tea does.
	type result struct{ cmd tea.Cmd }
	res := make(chan result, 1)
	go func() {
		_, cmd := m.slashCommand("/detach")
		res <- result{cmd}
	}()
	var cmd tea.Cmd
	select {
	case r := <-res:
		cmd = r.cmd
	case <-time.After(time.Second):
		t.Fatal("/detach blocked the update loop (it must not call the host synchronously)")
	}
	if cmd == nil {
		t.Fatal("/detach returned no command")
	}
	// Now the event loop is free again, as it would be after Update returns.
	// This drainer is deliberately never stopped: the host notifies on its own
	// goroutines (the detach below, and h.Close in the cleanup), and a
	// callback with no receiver left would block the host for good.
	go func() {
		for range msgs {
		}
	}()
	go cmd()
	select {
	case reason := <-bye:
		if reason == "" {
			t.Fatalf("bye reason = %q", reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the attached client was never told goodbye")
	}
}

// A roster change is somebody else's business. A terminal that is already
// running must not be sent a size for it: Bubble Tea treats any
// WindowSizeMsg as a full repaint, so re-sending one rewrote every other
// terminal's whole screen whenever anyone attached, detached or resized.
// The host used to announce roster changes with an FSize frame, which made
// clients clear, which is what the re-send was compensating for; FSize is
// gone, and with it the reason. (A view still gets its size from
// startLocked, and from onClientSize when it genuinely changes or the host
// asks for a repaint after evicting output.)
func TestRunnerDoesNotResizeRunningProgramsOnARosterChange(t *testing.T) {
	s := newTestSession(t)
	s.served = true
	r := &runner{s: s, programs: map[int]*program{}, early: map[int][]tea.Msg{}, quit: make(chan struct{}), noPrograms: true}
	r.onClients([]live.ClientInfo{{ID: 1, Label: "a", Cols: 80, Rows: 24, UTF8: true}})
	pr := r.programs[1]
	queued(pr.ctrl) // discard the size it was started with

	// Somebody else attaches…
	r.onClients([]live.ClientInfo{
		{ID: 1, Label: "a", Cols: 80, Rows: 24, UTF8: true},
		{ID: 2, Label: "b", Cols: 40, Rows: 15, UTF8: true}})
	// …resizes…
	r.onClients([]live.ClientInfo{
		{ID: 1, Label: "a", Cols: 80, Rows: 24, UTF8: true},
		{ID: 2, Label: "b", Cols: 60, Rows: 20, UTF8: true}})
	// …and leaves again.
	r.onClients([]live.ClientInfo{{ID: 1, Label: "a", Cols: 80, Rows: 24, UTF8: true}})
	for _, msg := range queued(pr.ctrl) {
		if w, ok := msg.(tea.WindowSizeMsg); ok {
			t.Fatalf("a roster change sent a running program a size (%dx%d): every other terminal repaints in full", w.Width, w.Height)
		}
	}
	// The roster itself still reaches it — that is what the bottom line
	// counts — it simply arrives as a clientsMsg on the mailbox.
	if len(queued(pr.mb.ch)) == 0 {
		t.Fatal("the roster change never reached the running program at all")
	}
}

// A terminal's own messages do not go through the session mailbox: that is
// drained by View.Update itself, which never goes through the Program, so
// Bubble Tea's renderer would never see them. Right for a transcript entry,
// wrong for a size (no repaint, and no width to erase lines to) — and keys
// go the same way so a keystroke and the size before it keep their order.
func TestKeysAndSizesGoToTheProgramsOwnQueueNotTheSessionMailbox(t *testing.T) {
	s := newTestSession(t)
	s.served = true
	r := &runner{s: s, programs: map[int]*program{}, early: map[int][]tea.Msg{}, quit: make(chan struct{}), noPrograms: true}
	r.onClients([]live.ClientInfo{{ID: 1, Label: "a", Cols: 80, Rows: 24, UTF8: true}})
	pr := r.programs[1]
	// The size it was started with is already on its own queue, not the mailbox.
	if got := queued(pr.ctrl); len(got) != 1 {
		t.Fatalf("start-up messages on the program's queue: %+v", got)
	}
	flush(pr.v)

	r.onClientSize(1, 90, 28)
	for _, ch := range "typing" {
		r.route(live.ClientKeyMsg{Client: 1, Key: tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{ch}}})
	}
	if n := len(queued(pr.mb.ch)); n != 0 {
		t.Fatalf("%d per-terminal message(s) went onto the session mailbox, where Bubble Tea never sees them", n)
	}
	msgs := queued(pr.ctrl)
	if _, ok := msgs[0].(tea.WindowSizeMsg); !ok {
		t.Fatalf("the resize is not first on the queue: %T", msgs[0])
	}
	var keys int
	for _, msg := range msgs {
		if _, ok := msg.(tea.KeyMsg); ok {
			keys++
		}
		pr.v.Update(msg) // Update drains the mailbox around each one
	}
	if keys != 6 {
		t.Fatalf("routed %d keys, want 6", keys)
	}
	if got := pr.v.input.Value(); got != "typing" {
		t.Fatalf("input = %q, want %q", got, "typing")
	}
	if pr.v.width != 90 || pr.v.height != 28 {
		t.Fatalf("view is %dx%d, want the size the host sent", pr.v.width, pr.v.height)
	}
}
