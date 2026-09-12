package tui

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sort"
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
	m.Update(clientsMsg{{ID: 1, Label: "vscode (pid 1)", UTF8: true}, {ID: 2, Label: "ssh from 10.0.0.5 (pid 2)", UTF8: true}})
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
	if !strings.Contains(m.transcript.String(), "attached: ssh from 10.0.0.5 (pid 2)") {
		t.Fatalf("no attach line:\n%s", m.transcript.String())
	}
	m.Update(clientsMsg{{ID: 1, Label: "vscode (pid 1)"}})
	if !strings.Contains(m.transcript.String(), "detached: ssh from 10.0.0.5 (pid 2)") {
		t.Fatal("no detach line")
	}
	if strings.Contains(m.View(), "⧉") {
		t.Fatal("marker shown with a single client")
	}
}

// /clients lists clients; /detach asks the host to drop the terminal the
// command was typed on.
func TestClientsAndDetachCommands(t *testing.T) {
	m := newTestModel(t)
	m.Update(clientsMsg{{ID: 1, Label: "local (pid 1)"}})
	m.slashCommand("/clients", 1)
	if !strings.Contains(m.transcript.String(), "local (pid 1)") {
		t.Fatal("/clients did not list")
	}
	detached := make(chan int, 1)
	m.detachClient = func(id int) { detached <- id }
	_, cmd := m.slashCommand("/detach", 1)
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
	m.slashCommand("/clients", 0)
	if !strings.Contains(m.transcript.String(), "not served") {
		t.Fatalf("expected a not-served note:\n%s", m.transcript.String())
	}
}

// /clients on a served session with nobody currently attached reports that
// distinctly from the not-served case.
func TestClientsServedEmptyMessage(t *testing.T) {
	m := newTestModel(t)
	m.served = true
	m.clients = nil
	m.slashCommand("/clients", 0)
	if !strings.Contains(m.transcript.String(), "no terminals attached") {
		t.Fatalf("expected a no-terminals-attached note:\n%s", m.transcript.String())
	}
	if strings.Contains(m.transcript.String(), "not served") {
		t.Fatal("served-but-empty must not say \"not served\"")
	}
}

// With live_idle_limit set, a served session with no clients and no run
// in progress quits once the limit has passed; a client or a run resets it.
func TestIdleLimitQuitsWhenUnattachedAndIdle(t *testing.T) {
	// idleTick() is a real tea.Cmd (tea.Tick) that only yields after the
	// configured interval, even when — as here — it's invoked synchronously
	// outside the Bubble Tea runtime. Shrink the interval for the test so
	// the assertion below does not block for the production 30s.
	orig := idleTickInterval
	idleTickInterval = time.Millisecond
	t.Cleanup(func() { idleTickInterval = orig })

	m := newTestModel(t)
	m.cfg.LiveIdleLimit = 1
	m.served = true
	m.clients = nil
	m.running = false
	m.idleSince = time.Now().Add(-2 * time.Minute)
	_, cmd := m.Update(idleTickMsg(time.Now()))
	if cmd == nil || fmt.Sprint(cmd()) != fmt.Sprint(tea.Quit()) {
		t.Fatal("expected tea.Quit after the idle limit")
	}
	m.idleSince = time.Now().Add(-2 * time.Minute)
	m.running = true
	if _, cmd := m.Update(idleTickMsg(time.Now())); cmd != nil && fmt.Sprint(cmd()) == fmt.Sprint(tea.Quit()) {
		t.Fatal("quit while a run is in progress")
	}
	if m.idleSince.Before(time.Now().Add(-time.Minute)) {
		t.Fatal("running did not reset idleSince")
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

// A client already attached to the host before RunServed starts (this is
// how Task 6 wires it: the host listens and serves first) must be visible
// immediately, not only after the next attach/detach/resize event.
// seedFromHost is the extracted, directly-testable piece of RunServed that
// is responsible for this.
func TestSeedFromHostClientsAlreadyAttached(t *testing.T) {
	dir := tempSockDir(t)
	sock := filepath.Join(dir, "s.sock")
	h := live.NewHost("tok", io.Discard)
	if err := h.Listen(sock); err != nil {
		t.Fatal(err)
	}
	go h.Serve()
	t.Cleanup(func() { h.Close("test over") })

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := live.WriteJSON(conn, live.FHello, live.Hello{
		Token: "tok", Cols: 80, Rows: 24, Label: "already-there", UTF8: false,
	}); err != nil {
		t.Fatal(err)
	}
	waitForClients(t, h, 1)

	m := newTestModel(t)
	m.seedFromHost(h)

	if len(m.clients) != 1 || m.clients[0].Label != "already-there" {
		t.Fatalf("clients not seeded from host: %+v", m.clients)
	}
	if !m.ascii {
		t.Fatal("ascii flag not seeded: the attached client is not UTF8-capable")
	}
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
	h.OnSize(func(c, r int) { msgs <- tea.WindowSizeMsg{Width: c, Height: r} })

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
	m.detachClient = h.Detach

	// "Update" runs with nothing draining msgs, exactly as Bubble Tea does.
	type result struct{ cmd tea.Cmd }
	res := make(chan result, 1)
	go func() {
		_, cmd := m.slashCommand("/detach", id)
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

func TestOverlayPositionsEachRowAndParksTheCursor(t *testing.T) {
	m := twoClients(t) // 100x30, served, 3 input rows
	m.Update(live.ClientKeyMsg{Client: 2, Key: runes("typed")})
	ov := m.overlayFor(2)
	first := m.height - m.inputRows() // 1-based row of the first input line
	for r := 0; r < m.inputRows(); r++ {
		if !strings.Contains(ov, fmt.Sprintf("\x1b[%d;1H", first+r)) {
			t.Fatalf("overlay lacks a move to row %d:\n%q", first+r, ov)
		}
	}
	if !strings.Contains(ov, "typed") {
		t.Fatal("overlay lacks the client's text")
	}
	if !strings.HasSuffix(ov, fmt.Sprintf("\x1b[%d;%dH", m.height, m.width)) {
		t.Fatalf("overlay must park the cursor at the bottom-right:\n%q", ov)
	}
	if strings.Contains(ov, "\x1b[K") {
		t.Fatal("overlay must pad rows, not clear to end of line (the wheel lives to the right)")
	}
}

func TestOverlayIsPublishedAfterAKeyAndForEveryoneOnResize(t *testing.T) {
	m := twoClients(t)
	var pub []int
	m.setOverlay = func(id int, s string) { pub = append(pub, id) }
	m.Update(live.ClientKeyMsg{Client: 1, Key: runes("a")})
	if len(pub) != 1 || pub[0] != 1 {
		t.Fatalf("after a key: %v", pub)
	}
	pub = nil
	m.Update(tea.WindowSizeMsg{Width: 90, Height: 28})
	sort.Ints(pub)
	if len(pub) != 2 || pub[0] != 1 || pub[1] != 2 {
		t.Fatalf("after a resize: %v", pub)
	}
}

// A full-screen modal (approval, picker, menu, plan) replaces the whole
// frame with no reserved input row: overlayFor's absolute positioning would
// land on the modal's own content, so publishing must stop while one is up,
// and every roster client's draft must be repainted the moment it closes.
func TestOverlayHiddenDuringApprovalAndRepublishedOnClose(t *testing.T) {
	m := twoClients(t)
	var pub []int
	m.setOverlay = func(id int, s string) { pub = append(pub, id) }

	m.Update(approvalMsg{action: "shell", detail: "echo hi", resp: make(chan bool, 1)})
	if m.mode != modeApproval {
		t.Fatalf("mode = %v, want modeApproval", m.mode)
	}
	pub = nil
	m.Update(live.ClientKeyMsg{Client: 2, Key: runes("x")}) // scrolls the modal viewport, does not close it
	if len(pub) != 0 {
		t.Fatalf("a key during the modal published: %v", pub)
	}

	m.Update(live.ClientKeyMsg{Client: 2, Key: runes("n")}) // denies and closes the modal
	if m.mode == modeApproval {
		t.Fatal("modal did not close")
	}
	sort.Ints(pub)
	if len(pub) != 2 || pub[0] != 1 || pub[1] != 2 {
		t.Fatalf("closing the modal did not republish every roster client: %v", pub)
	}
}

// startTurn mutates every client's textarea (the placeholder flips to the
// busy hint), not only the one whose Enter started the turn.
func TestStartTurnRepublishesEveryClientsOverlay(t *testing.T) {
	m := twoClients(t)
	m.Update(live.ClientKeyMsg{Client: 1, Key: runes("hi")})
	var pub []int
	m.setOverlay = func(id int, s string) { pub = append(pub, id) }
	m.Update(live.ClientKeyMsg{Client: 1, Key: tea.KeyMsg{Type: tea.KeyEnter}})
	seen := map[int]bool{}
	for _, id := range pub {
		seen[id] = true
	}
	if !seen[1] || !seen[2] {
		t.Fatalf("starting a turn did not republish every client's overlay: %v", pub)
	}
}
