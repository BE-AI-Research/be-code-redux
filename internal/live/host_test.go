package live

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

	// pauseMu guards paused: stopReading installs a gate the reader goroutine
	// blocks on before its next ReadFrame; resumeReading closes it.
	pauseMu sync.Mutex
	paused  chan struct{}
}

// pauseGate returns the current pause gate, if stopReading has been called
// and resumeReading has not yet closed it.
func (fc *fakeClient) pauseGate() chan struct{} {
	fc.pauseMu.Lock()
	defer fc.pauseMu.Unlock()
	return fc.paused
}

// stopReading makes the reader goroutine block before its next ReadFrame,
// simulating a stalled terminal whose queue can fill and evict frames.
func (fc *fakeClient) stopReading() {
	fc.pauseMu.Lock()
	fc.paused = make(chan struct{})
	fc.pauseMu.Unlock()
}

// resumeReading releases a reader goroutine parked by stopReading.
func (fc *fakeClient) resumeReading() {
	fc.pauseMu.Lock()
	if fc.paused != nil {
		close(fc.paused)
		fc.paused = nil
	}
	fc.pauseMu.Unlock()
}

// resize sends an FResize frame as a real client would.
func (fc *fakeClient) resize(t *testing.T, cols, rows int) {
	t.Helper()
	WriteJSON(fc.conn, FResize, Size{Cols: cols, Rows: rows})
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
			if p := fc.pauseGate(); p != nil {
				<-p
			}
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
// test once holder election is gone: every attached client gets every output
// frame.
func TestHostFansOutToAllClients(t *testing.T) {
	h, sock := startHost(t)
	a := dial(t, sock, "tok", "a", 100, 40)
	within(t, time.Second, func() bool { return len(h.Clients()) == 1 })
	b := dial(t, sock, "tok", "b", 80, 24)
	within(t, time.Second, func() bool { return len(h.Clients()) == 2 })
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

// TestHostBuffersEarlyInputAndReplaysOnOnInput covers the window between a
// client attaching and the served program registering OnInput: keystrokes
// typed in that window must not be lost.
func TestHostBuffersEarlyInputAndReplaysOnOnInput(t *testing.T) {
	h, sock := startHost(t)
	a := dial(t, sock, "tok", "a", 100, 40)
	within(t, time.Second, func() bool { return len(h.Clients()) == 1 })

	// Send more than the per-client cap in one frame: only the first
	// maxEarlyInput bytes must be kept, the rest silently dropped.
	early := strings.Repeat("E", 5*1024)
	WriteFrame(a.conn, FInput, []byte(early))
	// Force a round trip on the same connection: handle() reads frames from
	// one connection strictly in order on a single goroutine, so once this
	// resize is visible the FInput frame above (sent first, while OnInput
	// was still nil) is guaranteed to have already been read and buffered.
	WriteJSON(a.conn, FResize, Size{Cols: 90, Rows: 30})
	within(t, time.Second, func() bool {
		for _, c := range h.Clients() {
			if c.Cols == 90 {
				return true
			}
		}
		return false
	})

	got := make(chan string, 1)
	h.OnInput(func(id int, b []byte) { got <- string(b) })
	var replayed string
	select {
	case replayed = <-got:
	case <-time.After(time.Second):
		t.Fatal("early input was never replayed")
	}
	if len(replayed) != maxEarlyInput {
		t.Fatalf("replayed %d bytes, want the %d-byte cap", len(replayed), maxEarlyInput)
	}
	if replayed != early[:maxEarlyInput] {
		t.Fatal("replayed bytes are not exactly the first bytes sent")
	}

	// A second registration must not redeliver what the first already
	// consumed: pendingIn is cleared the moment it is replayed.
	redelivered := make(chan string, 1)
	h.OnInput(func(id int, b []byte) { redelivered <- string(b) })
	select {
	case s := <-redelivered:
		t.Fatalf("second OnInput registration redelivered early input: %q", s)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestResizeSendsNoSizeFrameToOtherClients covers the removal of the shared
// minimum size: one client's resize must never send an FSize frame to any
// client (its own roster row simply carries its new size).
func TestResizeSendsNoSizeFrameToOtherClients(t *testing.T) {
	h, sock := startHost(t)
	a := dial(t, sock, "tok", "a", 80, 24)
	b := dial(t, sock, "tok", "b", 120, 40)
	within(t, 2*time.Second, func() bool { return len(h.Clients()) == 2 })
	for len(b.cl) > 0 {
		<-b.cl
	}
	a.resize(t, 40, 15)
	select {
	case <-b.size:
		t.Fatal("b received a shared-size frame after a's resize")
	case <-time.After(300 * time.Millisecond):
	}
	select {
	case cl := <-b.cl:
		for _, c := range cl {
			if c.Label == "a" && (c.Cols != 40 || c.Rows != 15) {
				t.Fatalf("roster carries a's old size: %+v", c)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("b did not receive the roster update carrying a's new size")
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

// The old TestHostSizeCallbackAndSlowClient lived here. Its OnSize
// assertions covered the shared minimum size, removed in 0.8.0; its
// slow-client-does-not-block-Output coverage duplicates
// TestHostStalledClientDoesNotBlockOthersOrClose below, which exercises a
// client that truly never reads (rather than one whose buffered test
// channel merely fills), so nothing is lost by dropping this one.

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
	within(t, time.Second, func() bool {
		for _, c := range h.Clients() {
			if c.Label == "b" {
				return c.Cols == 90 && c.Rows == 20
			}
		}
		return false
	})

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

// The old TestHostAttachAlwaysNotifiesTheProgramOfTheSize lived here. It
// covered OnSize's shared-minimum semantics on attach, all removed in
// 0.8.0: there is no shared size left to (not) change, and no FSize frame
// sent to already-attached clients. TestEvictedOutputTriggersARepaintRequest
// below still exercises attach calling OnClientSize for the attaching
// client, and TestResizeNotifiesPerClient covers it on resize.

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

// The old TestHostClearOverlaysStopsReappending lived here. It covered
// Host.ClearOverlays and fanout.Write's overlay re-append, both removed in
// 0.8.0: each client now runs its own program with no shared closing-line
// frame for a stale overlay to be stamped onto.

// TestSanitizeLabel: a client's label is text it chose, and it lands in
// every other terminal's transcript, bottom line and /clients list — so the
// host, not the renderer, is where it stops being able to move the cursor,
// recolour the screen or wrap the status row.
func TestSanitizeLabel(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain label survives", "ssh from 10.0.0.2 (pid 12)", "ssh from 10.0.0.2 (pid 12)"},
		{"newlines and tabs go", "two\nlines\there", "twolineshere"},
		{"csi colour goes", "\x1b[31mred\x1b[0m", "red"},
		{"cursor move goes", "\x1b[2J\x1b[Hwiped", "wiped"},
		{"osc title goes", "\x1b]0;title\x07after", "after"},
		{"osc with st goes", "\x1b]0;title\x1b\\after", "after"},
		{"alt key sequence goes", "\x1bxab", "ab"},
		{"del byte goes", "a\x7fb", "ab"},
		{"empty becomes client", "", "client"},
		{"control-only becomes client", "\x1b[31m\n\t", "client"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := sanitizeLabel(c.in); got != c.want {
				t.Fatalf("sanitizeLabel(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
	long := sanitizeLabel(strings.Repeat("x", 100))
	if r := []rune(long); len(r) != maxLabelRunes || r[len(r)-1] != '…' {
		t.Fatalf("100-rune label = %q (%d runes), want %d ending in an ellipsis", long, len(r), maxLabelRunes)
	}
}

// TestHostSanitizesHelloLabel checks the trust boundary itself: the label
// is cleaned where the hello frame is accepted, so nothing downstream (the
// roster, the transcript, the bottom line) ever sees the raw bytes.
func TestHostSanitizesHelloLabel(t *testing.T) {
	h, sock := startHost(t)
	fc := dial(t, sock, "tok", "\x1b[31mevil\nname"+strings.Repeat("x", 100), 80, 24)
	defer fc.conn.Close()
	within(t, 2*time.Second, func() bool { return len(h.Clients()) == 1 })
	cl := h.Clients()
	if len(cl) != 1 {
		t.Fatalf("client never attached (%d in the roster)", len(cl))
	}
	label := cl[0].Label
	if strings.ContainsAny(label, "\x1b\n") {
		t.Fatalf("roster label still carries control bytes: %q", label)
	}
	if len([]rune(label)) > maxLabelRunes {
		t.Fatalf("roster label is %d runes: %q", len([]rune(label)), label)
	}
	if !strings.HasPrefix(label, "evilname") {
		t.Fatalf("roster label = %q, want it to start with evilname", label)
	}
}

func TestClientOutputReachesOneClientOnly(t *testing.T) {
	h, sock := startHost(t)
	a := dial(t, sock, "tok", "a", 80, 24)
	b := dial(t, sock, "tok", "b", 80, 24)
	within(t, 2*time.Second, func() bool { return len(h.Clients()) == 2 })
	<-a.cl // roster frames from the attaches
	io.WriteString(h.ClientOutput(idByLabel(t, h, "a")), "only-a")
	select {
	case p := <-a.out:
		if !strings.Contains(string(p), "only-a") {
			t.Fatalf("a got %q", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a never received its private output")
	}
	select {
	case p := <-b.out:
		t.Fatalf("b received a's private output: %q", p)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestResizeNotifiesPerClient(t *testing.T) {
	h, sock := startHost(t)
	sizes := make(chan string, 16)
	h.OnClientSize(func(id, cols, rows int) { sizes <- fmt.Sprintf("%d:%dx%d", id, cols, rows) })
	a := dial(t, sock, "tok", "a", 80, 24)
	within(t, 2*time.Second, func() bool { return len(h.Clients()) == 1 })
	id := idByLabel(t, h, "a")
	a.resize(t, 40, 15)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case got := <-sizes:
			if got == fmt.Sprintf("%d:40x15", id) {
				return
			}
		case <-deadline:
			t.Fatal("OnClientSize never reported a's new size")
		}
	}
}

func TestDropSaysGoodbyeWithTheReason(t *testing.T) {
	h, sock := startHost(t)
	a := dial(t, sock, "tok", "a", 80, 24)
	within(t, 2*time.Second, func() bool { return len(h.Clients()) == 1 })
	h.Drop(idByLabel(t, h, "a"), "view error")
	select {
	case reason := <-a.bye:
		if reason != "view error" {
			t.Fatalf("bye reason %q", reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no bye")
	}
	within(t, 2*time.Second, func() bool { return len(h.Clients()) == 0 })
}

func TestEvictedOutputTriggersARepaintRequest(t *testing.T) {
	h, sock := startHost(t)
	repaints := make(chan int, 64)
	h.OnClientSize(func(id, _, _ int) { repaints <- id })
	a := dial(t, sock, "tok", "a", 80, 24)
	within(t, 2*time.Second, func() bool { return len(h.Clients()) == 1 })
	for len(repaints) > 0 { // the attach itself reports the size once
		<-repaints
	}
	a.stopReading()
	w := h.ClientOutput(idByLabel(t, h, "a"))
	big := strings.Repeat("x", 256*1024)
	for i := 0; i < maxQueuedOutput*4; i++ {
		io.WriteString(w, big)
	}
	a.resumeReading()
	select {
	case <-repaints:
	case <-time.After(5 * time.Second):
		t.Fatal("no repaint request after output frames were evicted")
	}
}

// A control connection (what `sessions kill` opens) requests the quit and
// is never a client: the roster and the transcript must not see it.
func TestHostControlQuitIsNeverAClient(t *testing.T) {
	h, sock := startHost(t)
	quit := make(chan struct{}, 1)
	h.OnQuit(func() { quit <- struct{}{} })
	rosterChanges := 0
	h.OnClients(func([]ClientInfo) { rosterChanges++ })
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := WriteJSON(conn, FHello, Hello{Token: "tok", Cols: 9999, Rows: 9999, Label: "sessions kill", UTF8: true, Control: true}); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(conn, FQuit, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case <-quit:
	case <-time.After(time.Second):
		t.Fatal("OnQuit not called for a control quit")
	}
	if n := len(h.Clients()); n != 0 {
		t.Fatalf("control connection registered as a client (%d)", n)
	}
	if rosterChanges != 0 {
		t.Fatalf("roster notified %d times for a control connection", rosterChanges)
	}
	// The host closes the control connection without a bye.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := ReadFrame(conn); err == nil {
		t.Fatal("expected the control connection to be closed, got a frame")
	}
}
