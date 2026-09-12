package live

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func startHost(t *testing.T) (*Host, string) {
	t.Helper()
	dir, _ := os.MkdirTemp("", "bl")
	t.Cleanup(func() { os.RemoveAll(dir) })
	h := NewHost("tok", io.Discard)
	sock := filepath.Join(dir, "s.sock")
	if err := h.Listen(sock); err != nil {
		t.Fatal(err)
	}
	go h.Serve()
	t.Cleanup(func() { h.Close("test over") })
	return h, sock
}

type fakeClient struct {
	conn    net.Conn
	out     chan []byte // FOutput payloads
	overlay chan []byte // FOverlay payloads
	size    chan Size
	cl      chan []ClientInfo
	bye     chan string
}

func dial(t *testing.T, sock, token, label string, cols, rows int) *fakeClient {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeClient{conn: c, out: make(chan []byte, 64), overlay: make(chan []byte, 64), size: make(chan Size, 8), cl: make(chan []ClientInfo, 8), bye: make(chan string, 1)}
	WriteJSON(c, FHello, Hello{Token: token, Cols: cols, Rows: rows, Label: label, UTF8: true})
	go func() {
		for {
			typ, p, err := ReadFrame(c)
			if err != nil {
				return
			}
			switch typ {
			case FOutput:
				fc.out <- p
			case FOverlay:
				fc.overlay <- p
			case FSize:
				var s Size
				json.Unmarshal(p, &s)
				fc.size <- s
			case FClients:
				var cl []ClientInfo
				json.Unmarshal(p, &cl)
				fc.cl <- cl
			case FBye:
				var b Bye
				json.Unmarshal(p, &b)
				fc.bye <- b.Reason
			}
		}
	}()
	return fc
}

func within(t *testing.T, d time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// idByLabel looks a client's id up by its label rather than its position in
// h.Clients(): two dials issued back to back race each other's own handle()
// goroutine for the lock that appends to h.clients, so which one lands first
// is not guaranteed even though the connections themselves were made in
// order — a test that assumed cl[0] was always the first dial flaked.
func idByLabel(t *testing.T, h *Host, label string) int {
	t.Helper()
	for _, c := range h.Clients() {
		if c.Label == label {
			return c.ID
		}
	}
	t.Fatalf("no client labeled %q in %+v", label, h.Clients())
	return 0
}

// TestHostFansOutToAllClients covers what remains of the old holder-election
// test once holder election is gone: every client gets the shared size (the
// minimum across attached clients) and every output frame.
func TestHostFansOutToAllClients(t *testing.T) {
	h, sock := startHost(t)
	a := dial(t, sock, "tok", "a", 100, 40)
	within(t, time.Second, func() bool { return len(h.Clients()) == 1 })
	b := dial(t, sock, "tok", "b", 80, 24)
	within(t, time.Second, func() bool { return len(h.Clients()) == 2 })
	// shared size is the minimum
	if c, r := h.Size(); c != 80 || r != 24 {
		t.Fatalf("size = %dx%d", c, r)
	}
	// output goes to both
	h.Output().Write([]byte("frame1"))
	for _, fc := range []*fakeClient{a, b} {
		select {
		case p := <-fc.out:
			if string(p) != "frame1" {
				t.Fatalf("got %q", p)
			}
		case <-time.After(time.Second):
			t.Fatal("client did not receive the frame")
		}
	}
}

func TestHostDeliversTaggedInputFromEveryClient(t *testing.T) {
	h, sock := startHost(t)
	type in struct {
		id int
		b  string
	}
	got := make(chan in, 8)
	h.OnInput(func(id int, b []byte) { got <- in{id, string(b)} })
	a := dial(t, sock, "tok", "a", 100, 40)
	within(t, time.Second, func() bool { return len(h.Clients()) == 1 })
	b := dial(t, sock, "tok", "b", 80, 24)
	within(t, time.Second, func() bool { return len(h.Clients()) == 2 })
	WriteFrame(a.conn, FInput, []byte("A"))
	WriteFrame(b.conn, FInput, []byte("B"))
	seen := map[int]string{}
	for i := 0; i < 2; i++ {
		select {
		case x := <-got:
			seen[x.id] += x.b
		case <-time.After(time.Second):
			t.Fatal("input not delivered")
		}
	}
	idA, idB := idByLabel(t, h, "a"), idByLabel(t, h, "b")
	if seen[idA] != "A" || seen[idB] != "B" {
		t.Fatalf("tagging: %+v (a=%d b=%d)", seen, idA, idB)
	}
}

func TestHostOverlayGoesToOneClientAndFollowsEveryFrame(t *testing.T) {
	h, sock := startHost(t)
	a := dial(t, sock, "tok", "a", 100, 40)
	b := dial(t, sock, "tok", "b", 100, 40)
	within(t, time.Second, func() bool { return len(h.Clients()) == 2 })
	idA := idByLabel(t, h, "a")
	h.SetOverlay(idA, "OVERLAY-A")
	select {
	case p := <-a.overlay:
		if string(p) != "OVERLAY-A" {
			t.Fatalf("overlay payload %q", p)
		}
	case <-time.After(time.Second):
		t.Fatal("a did not get its overlay")
	}
	select {
	case p := <-b.overlay:
		t.Fatalf("b received a's overlay: %q", p)
	case <-time.After(200 * time.Millisecond):
	}
	h.Output().Write([]byte("FRAME"))
	<-a.out
	select { // a's overlay is re-sent right after the shared frame
	case p := <-a.overlay:
		if string(p) != "OVERLAY-A" {
			t.Fatalf("re-sent overlay %q", p)
		}
	case <-time.After(time.Second):
		t.Fatal("overlay not re-sent after a frame")
	}
	<-b.out
	select {
	case <-b.overlay:
		t.Fatal("b has no overlay yet must not receive one")
	case <-time.After(200 * time.Millisecond):
	}
	h.SetOverlay(idA, "OVERLAY-A") // unchanged: no write
	select {
	case <-a.overlay:
		t.Fatal("unchanged overlay was re-sent")
	case <-time.After(200 * time.Millisecond):
	}
}

func TestHostSwitchAndDetachByID(t *testing.T) {
	h, sock := startHost(t)
	a := dial(t, sock, "tok", "a", 100, 40)
	b := dial(t, sock, "tok", "b", 100, 40)
	within(t, time.Second, func() bool { return len(h.Clients()) == 2 })
	idA, idB := idByLabel(t, h, "a"), idByLabel(t, h, "b")
	h.Switch(idA, "ZZZ999")
	select {
	case r := <-a.bye:
		if r != ReasonSwitchPrefix+"ZZZ999" {
			t.Fatalf("switch reason %q", r)
		}
	case <-time.After(time.Second):
		t.Fatal("no switch bye")
	}
	within(t, time.Second, func() bool { return len(h.Clients()) == 1 })
	h.Detach(idB)
	select {
	case r := <-b.bye:
		if r != ReasonDetached {
			t.Fatalf("detach reason %q", r)
		}
	case <-time.After(time.Second):
		t.Fatal("no detach bye")
	}
	h.Detach(12345) // unknown id: no-op
}

func TestHostRejectsBadToken(t *testing.T) {
	_, sock := startHost(t)
	c := dial(t, sock, "wrong", "x", 80, 24)
	select {
	case reason := <-c.bye:
		if reason == "" {
			t.Fatal("empty bye reason")
		}
	case <-time.After(time.Second):
		t.Fatal("no bye for a bad token")
	}
}

func TestHostSizeCallbackAndSlowClient(t *testing.T) {
	h, sock := startHost(t)
	sizes := make(chan Size, 8)
	h.OnSize(func(c, r int) { sizes <- Size{c, r} })
	expect := func(want Size) {
		t.Helper()
		select {
		case got := <-sizes:
			if got != want {
				t.Fatalf("OnSize %+v, want %+v", got, want)
			}
		case <-time.After(time.Second):
			t.Fatal("OnSize not called")
		}
	}
	dial(t, sock, "tok", "a", 120, 40)
	expect(Size{120, 40})
	slow := dial(t, sock, "tok", "slow", 60, 20)
	expect(Size{60, 20})
	// The slow client never reads; 200 frames must not block the writer.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 200; i++ {
			h.Output().Write([]byte("x"))
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Output blocked on a slow client")
	}
	_ = slow
}

func TestHostListenSocketPermissions(t *testing.T) {
	_, sock := startHost(t)
	info, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Fatalf("socket perm = %o, want no group/other bits (0700 or tighter)", perm)
	}
}

func TestHostDetachByIDKeepsOthers(t *testing.T) {
	h, sock := startHost(t)
	dial(t, sock, "tok", "a", 100, 40)
	within(t, time.Second, func() bool { return len(h.Clients()) == 1 })
	dial(t, sock, "tok", "b", 90, 30)
	within(t, time.Second, func() bool { return len(h.Clients()) == 2 })
	dial(t, sock, "tok", "c", 80, 24)
	within(t, time.Second, func() bool { return len(h.Clients()) == 3 })
	cl := h.Clients()
	var bID int
	for _, c := range cl {
		if c.Label == "b" {
			bID = c.ID
		}
	}
	h.Detach(bID)
	within(t, time.Second, func() bool {
		c := h.Clients()
		if len(c) != 2 {
			return false
		}
		labels := map[string]bool{c[0].Label: true, c[1].Label: true}
		return labels["a"] && labels["c"]
	})
	// size is recomputed from the remaining clients (a=100x40, c=80x24)
	if c, r := h.Size(); c != 80 || r != 24 {
		t.Fatalf("size after detach = %dx%d", c, r)
	}
}

func TestHostOnQuitViaFQuitFrame(t *testing.T) {
	h, sock := startHost(t)
	quit := make(chan struct{}, 1)
	h.OnQuit(func() { quit <- struct{}{} })
	a := dial(t, sock, "tok", "a", 80, 24)
	within(t, time.Second, func() bool { return len(h.Clients()) == 1 })
	WriteFrame(a.conn, FQuit, nil)
	select {
	case <-quit:
	case <-time.After(time.Second):
		t.Fatal("OnQuit not called for an FQuit frame")
	}
}

func TestHostCloseTwiceIsSafe(t *testing.T) {
	dir, _ := os.MkdirTemp("", "bl")
	t.Cleanup(func() { os.RemoveAll(dir) })
	h := NewHost("tok", io.Discard)
	sock := filepath.Join(dir, "s.sock")
	if err := h.Listen(sock); err != nil {
		t.Fatal(err)
	}
	go h.Serve()
	a := dial(t, sock, "tok", "a", 80, 24)
	within(t, time.Second, func() bool { return len(h.Clients()) == 1 })
	h.Close("bye once")
	h.Close("bye twice") // must not panic or hang
	select {
	case r := <-a.bye:
		if r != "bye once" {
			t.Fatalf("bye reason = %q", r)
		}
	case <-time.After(time.Second):
		t.Fatal("no bye received")
	}
}

// TestHostStalledClientDoesNotBlockOthersOrClose is the concrete failure mode
// behind routing every frame through a client's own writer goroutine: a
// client that attaches and then never reads again must not be able to block
// another client's attach/resize, or delay Close.
func TestHostStalledClientDoesNotBlockOthersOrClose(t *testing.T) {
	dir, _ := os.MkdirTemp("", "bl")
	t.Cleanup(func() { os.RemoveAll(dir) })
	h := NewHost("tok", io.Discard)
	sock := filepath.Join(dir, "s.sock")
	if err := h.Listen(sock); err != nil {
		t.Fatal(err)
	}
	go h.Serve()

	// Attach a client but never read from it again (no reader goroutine, and
	// we never touch the conn ourselves): its socket receive buffer will
	// fill and stay full for the rest of the test.
	stalled, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stalled.Close() })
	WriteJSON(stalled, FHello, Hello{Token: "tok", Cols: 80, Rows: 24, Label: "stalled", UTF8: true})
	within(t, time.Second, func() bool { return len(h.Clients()) == 1 })

	// Flood output well past any reasonable socket buffer while the stalled
	// client never drains it.
	go func() {
		big := make([]byte, 1<<16)
		for i := 0; i < 64; i++ {
			h.Output().Write(big)
		}
	}()

	// A second, well-behaved client must still be able to attach and have
	// its resize take effect promptly.
	b := dial(t, sock, "tok", "b", 100, 30)
	within(t, time.Second, func() bool { return len(h.Clients()) == 2 })
	resize, _ := json.Marshal(Size{Cols: 90, Rows: 20})
	WriteFrame(b.conn, FResize, resize)
	// shared size is the minimum across both clients (stalled is 80x24)
	within(t, time.Second, func() bool { c, r := h.Size(); return c == 80 && r == 20 })

	// Close must return promptly even though the stalled client can never be
	// told goodbye in any normal sense.
	done := make(chan struct{})
	go func() { h.Close("shutdown"); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on a stalled client")
	}
}

// TestHostAttachAlwaysNotifiesTheProgramOfTheSize covers the repaint an
// attacher depends on. A client clears its screen when it attaches, and the
// Bubble Tea renderer only repaints in full when it is given a
// WindowSizeMsg — which the host derives from OnSize. A second terminal at
// the same size (or a larger one, leaving the shared minimum unchanged)
// would otherwise see nothing but the next diff lines on a blank screen.
func TestHostAttachAlwaysNotifiesTheProgramOfTheSize(t *testing.T) {
	h, sock := startHost(t)
	sizes := make(chan Size, 8)
	h.OnSize(func(c, r int) { sizes <- Size{c, r} })
	expect := func(want Size, what string) {
		t.Helper()
		select {
		case got := <-sizes:
			if got != want {
				t.Fatalf("%s: OnSize %+v, want %+v", what, got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s: OnSize not called", what)
		}
	}
	dial(t, sock, "tok", "a", 80, 24)
	expect(Size{80, 24}, "first attach")

	// Same size: the shared minimum does not change, but the program must
	// still be told so the new terminal gets a full frame.
	same := dial(t, sock, "tok", "same", 80, 24)
	expect(Size{80, 24}, "attach at the same size")
	// Larger: still no change to the minimum, still a notification.
	bigger := dial(t, sock, "tok", "bigger", 120, 40)
	expect(Size{80, 24}, "attach at a larger size")

	// Each of those clients learns the render size exactly once: the
	// unchanged-minimum path must not broadcast to everyone, and the
	// changed path must not be doubled by a second direct send.
	for _, fc := range []*fakeClient{same, bigger} {
		select {
		case got := <-fc.size:
			if got != (Size{80, 24}) {
				t.Fatalf("client FSize %+v, want 80x24", got)
			}
		case <-time.After(time.Second):
			t.Fatal("client never received its attach-time FSize")
		}
		select {
		case got := <-fc.size:
			t.Fatalf("duplicate FSize %+v", got)
		case <-time.After(100 * time.Millisecond):
		}
	}
	// And a real change still notifies exactly once.
	smaller := dial(t, sock, "tok", "smaller", 60, 20)
	expect(Size{60, 20}, "attach at a smaller size")
	select {
	case got := <-sizes:
		t.Fatalf("second OnSize for one change: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case got := <-smaller.size:
		if got != (Size{60, 20}) {
			t.Fatalf("client FSize %+v, want 60x20", got)
		}
	case <-time.After(time.Second):
		t.Fatal("client never received the new shared size")
	}
	select {
	case got := <-smaller.size:
		t.Fatalf("duplicate FSize after a size change: %+v", got)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestHostReplaysQuitRequestedBeforeOnQuit covers the startup window: the
// host listens and serves before the program registers OnQuit, so a SIGTERM
// (`be-code sessions kill`) or a client's quit frame can arrive with no
// callback in place. It must not be swallowed, or the host keeps running
// while the caller believes it asked it to stop.
func TestHostReplaysQuitRequestedBeforeOnQuit(t *testing.T) {
	h, _ := startHost(t)
	h.RequestQuit() // no OnQuit yet
	quit := make(chan struct{}, 2)
	h.OnQuit(func() { quit <- struct{}{} })
	select {
	case <-quit:
	case <-time.After(time.Second):
		t.Fatal("a quit requested before OnQuit was registered was dropped")
	}
	// Replayed once, not on every later registration.
	h.OnQuit(func() { quit <- struct{}{} })
	select {
	case <-quit:
		t.Fatal("the pending quit was replayed twice")
	case <-time.After(100 * time.Millisecond):
	}
}
