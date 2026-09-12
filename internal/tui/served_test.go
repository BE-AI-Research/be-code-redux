package tui

import (
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/live"
)

// With two clients the bottom line names the holder and the chords.
func TestClientsMsgRendersHolder(t *testing.T) {
	m := newTestModel(t)
	// UTF8: true on both — this test is about the holder label and chord
	// hints, not the ASCII fallback (see TestASCIIFallbacks in compact_test.go);
	// m.ascii now has a reader (the clients marker glyph), so a fixture that
	// leaves UTF8 at its zero value would render the ASCII marker instead.
	m.Update(clientsMsg{{ID: 1, Label: "vscode (pid 1)", UTF8: true}, {ID: 2, Label: "ssh from 10.0.0.5 (pid 2)", Holder: true, UTF8: true}})
	v := m.View()
	for _, want := range []string{"⧉ 2", "input: ssh from 10.0.0.5", "Ctrl+] d", "Ctrl+] t"} {
		if !strings.Contains(v, want) {
			t.Fatalf("bottom line lacks %q:\n%s", want, v)
		}
	}
	if !strings.Contains(m.transcript.String(), "attached: ssh from 10.0.0.5 (pid 2), now holding input") {
		t.Fatalf("no attach line:\n%s", m.transcript.String())
	}
	m.Update(clientsMsg{{ID: 1, Label: "vscode (pid 1)", Holder: true}})
	if !strings.Contains(m.transcript.String(), "detached: ssh from 10.0.0.5 (pid 2)") {
		t.Fatal("no detach line")
	}
	if strings.Contains(m.View(), "⧉") {
		t.Fatal("marker shown with a single client")
	}
}

// /clients lists clients; /detach asks the host to drop the holder.
func TestClientsAndDetachCommands(t *testing.T) {
	m := newTestModel(t)
	m.Update(clientsMsg{{ID: 1, Label: "local (pid 1)", Holder: true}})
	m.slashCommand("/clients")
	if !strings.Contains(m.transcript.String(), "local (pid 1)") {
		t.Fatal("/clients did not list")
	}
	detached := false
	m.detachHolder = func() { detached = true }
	m.slashCommand("/detach")
	if !detached {
		t.Fatal("/detach did not call the host")
	}
}

// /clients on an in-process (never served) session says so, rather than
// confusing "no clients" with "not served" (the two used to be conflated
// by testing len(m.clients) == 0 alone).
func TestClientsNotServedMessage(t *testing.T) {
	m := newTestModel(t)
	m.served = false
	m.clients = nil
	m.slashCommand("/clients")
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
	m.slashCommand("/clients")
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
	dir := t.TempDir()
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
