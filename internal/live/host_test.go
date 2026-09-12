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
	conn net.Conn
	out  chan []byte // FOutput payloads
	size chan Size
	cl   chan []ClientInfo
	bye  chan string
}

func dial(t *testing.T, sock, token, label string, cols, rows int) *fakeClient {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeClient{conn: c, out: make(chan []byte, 64), size: make(chan Size, 8), cl: make(chan []ClientInfo, 8), bye: make(chan string, 1)}
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

func TestHostFansOutAndElectsHolder(t *testing.T) {
	h, sock := startHost(t)
	a := dial(t, sock, "tok", "a", 100, 40)
	within(t, time.Second, func() bool { return len(h.Clients()) == 1 })
	b := dial(t, sock, "tok", "b", 80, 24)
	within(t, time.Second, func() bool { return len(h.Clients()) == 2 })
	// newest attacher holds input
	cl := h.Clients()
	if !cl[1].Holder || cl[0].Holder {
		t.Fatalf("holder election: %+v", cl)
	}
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
	// only the holder's input reaches the program
	buf := make([]byte, 8)
	aInputDone := make(chan struct{})
	go func() { WriteFrame(a.conn, FInput, []byte("A")); close(aInputDone) }()
	WriteFrame(b.conn, FInput, []byte("B"))
	n, _ := h.InputReader().Read(buf)
	if string(buf[:n]) != "B" {
		t.Fatalf("program got %q, want B", buf[:n])
	}
	<-aInputDone // wait for the goroutine's write to finish before reusing a.conn: two
	// goroutines writing frames on the same net.Conn without ordering can interleave a
	// header from one frame with the payload of another and corrupt the wire protocol.
	// takeover moves input; detach of the holder passes it back
	WriteFrame(a.conn, FTakeover, nil)
	within(t, time.Second, func() bool { c := h.Clients(); return len(c) == 2 && c[0].Holder })
	WriteFrame(a.conn, FDetach, nil)
	within(t, time.Second, func() bool { c := h.Clients(); return len(c) == 1 && c[0].Holder && c[0].Label == "b" })
	if c, r := h.Size(); c != 80 || r != 24 {
		t.Fatalf("size after detach = %dx%d", c, r)
	}
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

func TestHostDetachHolderPassesToMostRecentRemaining(t *testing.T) {
	h, sock := startHost(t)
	dial(t, sock, "tok", "a", 100, 40)
	within(t, time.Second, func() bool { return len(h.Clients()) == 1 })
	dial(t, sock, "tok", "b", 90, 30)
	within(t, time.Second, func() bool { return len(h.Clients()) == 2 })
	dial(t, sock, "tok", "c", 80, 24)
	within(t, time.Second, func() bool { return len(h.Clients()) == 3 })
	cl := h.Clients()
	if !cl[2].Holder || cl[2].Label != "c" {
		t.Fatalf("expected newest attacher c to hold: %+v", cl)
	}
	h.DetachHolder()
	within(t, time.Second, func() bool {
		c := h.Clients()
		return len(c) == 2 && c[1].Holder && c[1].Label == "b"
	})
	// DetachHolder with a single remaining client, then with none: neither panics.
	h.DetachHolder()
	within(t, time.Second, func() bool { return len(h.Clients()) == 1 })
	h.DetachHolder()
	within(t, time.Second, func() bool { return len(h.Clients()) == 0 })
	h.DetachHolder() // no holder left; must be a no-op
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
