package tui

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/brown-enterprises/be-code/internal/live"
)

// A stand-in for an attached terminal, for the tests that run the real
// runner over a real live.Host. It is internal/live's own fakeClient with
// the output side changed: a real Bubble Tea program renders whenever
// anything at all changes, so a channel of frames would fill and the host
// would start evicting output for a client the test has not read from yet.
// Here the reader appends every FOutput payload to one buffer instead, and
// the assertions read that buffer — which is also what makes bytesSoFar an
// honest count of everything this terminal has been sent.

type fakeClient struct {
	conn net.Conn

	mu  sync.Mutex
	out bytes.Buffer // every FOutput payload, concatenated

	bye chan string
}

// newTestHostFor starts a live host on a socket short enough for AF_UNIX
// (sun_path is 108 bytes, and t.TempDir() under the test cache is often
// longer) and returns it with its socket path and token.
func newTestHostFor(t *testing.T) (*live.Host, string, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "bl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	h := live.NewHost("tok", io.Discard)
	sock := filepath.Join(dir, "s.sock")
	if err := h.Listen(sock); err != nil {
		t.Fatal(err)
	}
	go h.Serve()
	t.Cleanup(func() { h.Close("test over") })
	return h, sock, "tok"
}

// idFor looks a client's id up by its label rather than its position in
// h.Clients(): two dials issued back to back race each other's handle()
// goroutine for the lock that appends to the roster, so which one is first
// is not guaranteed even though the connections were made in order.
func idFor(t *testing.T, h *live.Host, label string) int {
	t.Helper()
	for _, c := range h.Clients() {
		if c.Label == label {
			return c.ID
		}
	}
	t.Fatalf("no client labeled %q in %+v", label, h.Clients())
	return 0
}

// viewSize reports one terminal's view's own size. It reads under the
// session lock because View.update is what writes those fields, and it does
// so under exactly that lock.
func viewSize(t *testing.T, s *Session, id int) (int, int) {
	t.Helper()
	v := s.viewByID(id)
	if v == nil {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return v.width, v.height
}

// dialFake attaches one fake terminal of its own size and starts reading
// its frames.
func dialFake(t *testing.T, sock, token, label string, cols, rows int) *fakeClient {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	fc := &fakeClient{conn: c, bye: make(chan string, 1)}
	t.Cleanup(func() { c.Close() })
	if err := live.WriteJSON(c, live.FHello, live.Hello{Token: token, Cols: cols, Rows: rows, Label: label, UTF8: true}); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			typ, p, err := live.ReadFrame(c)
			if err != nil {
				return
			}
			switch typ {
			case live.FOutput:
				fc.mu.Lock()
				fc.out.Write(p)
				fc.mu.Unlock()
			case live.FBye:
				var b live.Bye
				json.Unmarshal(p, &b)
				select {
				case fc.bye <- b.Reason:
				default:
				}
				return
			}
		}
	}()
	return fc
}

// bytesSoFar is how much this terminal has been rendered to.
func (fc *fakeClient) bytesSoFar() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.out.Len()
}

// text is everything rendered to this terminal so far, with the renderer's
// cursor moves and clears stripped so layout markers can be matched.
func (fc *fakeClient) text() string {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return ansi.Strip(fc.out.String())
}

// collect waits until everything rendered to this terminal contains needle
// and returns it. Bubble Tea writes ANSI around (and inside) the frame and
// may split a frame across frames, so the search is over the whole
// concatenated, de-escaped stream rather than one payload.
func (fc *fakeClient) collect(t *testing.T, needle string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got := fc.text()
		if strings.Contains(got, needle) {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%q never reached this terminal in %s; got:\n%s", needle, timeout, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// resize sends an FResize frame as a real client does when its terminal
// changes size.
func (fc *fakeClient) resize(t *testing.T, cols, rows int) {
	t.Helper()
	if err := live.WriteJSON(fc.conn, live.FResize, live.Size{Cols: cols, Rows: rows}); err != nil {
		t.Fatal(err)
	}
}

// detach asks the host to drop this terminal, as Ctrl+] d does.
func (fc *fakeClient) detach(t *testing.T) {
	t.Helper()
	if err := live.WriteFrame(fc.conn, live.FDetach, nil); err != nil {
		t.Fatal(err)
	}
}

// readBye waits for the host's goodbye and returns its reason.
func (fc *fakeClient) readBye(t *testing.T) string {
	t.Helper()
	select {
	case reason := <-fc.bye:
		return reason
	case <-time.After(3 * time.Second):
		t.Fatal("no bye frame")
		return ""
	}
}
