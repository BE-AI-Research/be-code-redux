package live

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// byeWait bounds how long detach waits for a client's writer goroutine to
// drain its queue (ideally including the FBye it just enqueued) before the
// connection is forced closed regardless. A stalled client (suspended
// terminal, dead link) must never be able to delay detach or Close beyond
// this.
const byeWait = 500 * time.Millisecond

// maxQueuedOutput caps how many frames of a screen-frame type (FOutput,
// FOverlay) a client's queue may hold before the oldest of that type is
// evicted — each type independently, so 8 FOutput and 8 FOverlay frames can
// be queued at once. Each is a full repaint (of the shared view, or of one
// client's private rows), so only the newest of each matters; control frames
// (FSize, FClients, FBye) are never dropped.
const maxQueuedOutput = 8

// qframe is one frame pending delivery to a client.
type qframe struct {
	typ     FrameType
	payload []byte
}

type client struct {
	id    int
	label string
	cols  int
	rows  int
	utf8  bool
	conn  net.Conn

	// overlay is this client's private input rows, guarded by Host.mu (not
	// qmu: it is read and written alongside the client slice in fanout.Write
	// and SetOverlay, never on its own).
	overlay string

	// qmu guards queue. Only writer() ever reads/drains it; enqueue is called
	// from any goroutine (recompute, detach, fanout, handle) and must never
	// block its caller — that is the whole point of routing every frame
	// through this queue and a single per-client writer goroutine instead of
	// writing to conn directly.
	qmu   sync.Mutex
	queue []qframe

	wake       chan struct{} // size 1: signals writer() there is new work
	writerDone chan struct{} // closed when writer() returns
	once       sync.Once
}

func newClient(hello Hello, conn net.Conn) *client {
	return &client{
		label: hello.Label, cols: hello.Cols, rows: hello.Rows, utf8: hello.UTF8, conn: conn,
		wake:       make(chan struct{}, 1),
		writerDone: make(chan struct{}),
	}
}

// enqueue appends a frame for the client's writer goroutine to send. It never
// blocks: a full queue of FOutput or FOverlay frames evicts that type's
// oldest entry to make room (control frames are never evicted, so this queue
// can grow beyond maxQueuedOutput only through control traffic, which is
// bounded by real events — attach/detach/resize — not by output volume).
// FOutput and FOverlay are both screen bytes for which only the newest
// matters, so each is capped and evicted independently by its own type.
func (c *client) enqueue(t FrameType, payload []byte) {
	c.qmu.Lock()
	if t == FOutput || t == FOverlay {
		n := 0
		for _, f := range c.queue {
			if f.typ == t {
				n++
			}
		}
		if n >= maxQueuedOutput {
			for i, f := range c.queue {
				if f.typ == t {
					c.queue = append(c.queue[:i], c.queue[i+1:]...)
					break
				}
			}
		}
	}
	c.queue = append(c.queue, qframe{t, payload})
	c.qmu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *client) enqueueJSON(t FrameType, v any) {
	b, _ := json.Marshal(v)
	c.enqueue(t, b)
}

// drain removes and returns every frame currently queued.
func (c *client) drain() []qframe {
	c.qmu.Lock()
	defer c.qmu.Unlock()
	if len(c.queue) == 0 {
		return nil
	}
	out := c.queue
	c.queue = nil
	return out
}

// Host serves one TUI session to any number of attached terminals.
type Host struct {
	token string
	log   io.Writer
	logMu sync.Mutex // serializes writes to log; log may not be concurrency-safe on its own
	ln    net.Listener

	mu      sync.Mutex
	clients []*client // attach order
	nextID  int
	cols    int
	rows    int

	// notifyMu orders recompute's compute-and-notify as a whole (see
	// recompute for why h.mu alone is not enough).
	notifyMu sync.Mutex

	onInput   func(client int, b []byte)
	onSize    func(cols, rows int)
	onClients func([]ClientInfo)
	onQuit    func()
	// quitPending records a RequestQuit that arrived before OnQuit was
	// registered — the host listens and serves before the program is
	// started (see tui.RunServed), so a SIGTERM from `sessions kill`, or a
	// quit frame from a client that attached during startup, can genuinely
	// land in that window. Dropping it would leave the host running with
	// the caller convinced it had asked it to stop.
	quitPending bool
	closing     bool
}

func NewHost(token string, log io.Writer) *Host {
	return &Host{token: token, log: log}
}

// OnInput registers the program's tagged-keystroke callback: f is called
// with the id of the client that sent them and the raw bytes, once per
// FInput frame, for every attached client (there is no holder any more — the
// served program decides what each client's bytes mean). f runs on that
// client's own connection-handling goroutine (see handle), not a dedicated
// one, so it must not block for long: doing so stalls that same client's
// resize, detach and quit frames behind it.
func (h *Host) OnInput(f func(client int, b []byte)) { h.mu.Lock(); h.onInput = f; h.mu.Unlock() }

func (h *Host) OnSize(f func(int, int))        { h.mu.Lock(); h.onSize = f; h.mu.Unlock() }
func (h *Host) OnClients(f func([]ClientInfo)) { h.mu.Lock(); h.onClients = f; h.mu.Unlock() }
func (h *Host) Output() io.Writer              { return fanout{h} }

// OnQuit registers the program's shutdown hook. A quit requested before this
// call is replayed into f now (outside h.mu: f is the program's callback and
// may do anything, including calling back into the host).
func (h *Host) OnQuit(f func()) {
	h.mu.Lock()
	h.onQuit = f
	replay := h.quitPending && f != nil
	h.quitPending = false
	h.mu.Unlock()
	if replay {
		f()
	}
}

func (h *Host) logf(format string, args ...any) {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	fmt.Fprintf(h.log, format, args...)
}

func (h *Host) Listen(sockPath string) error {
	_ = os.Remove(sockPath)
	old := syscallUmask(0o077)
	ln, err := net.Listen("unix", sockPath)
	syscallUmask(old)
	if err != nil {
		return err
	}
	h.ln = ln
	return nil
}

func (h *Host) Serve() {
	for {
		conn, err := h.ln.Accept()
		if err != nil {
			return
		}
		go h.handle(conn)
	}
}

func (h *Host) handle(conn net.Conn) {
	typ, payload, err := ReadFrame(conn)
	var hello Hello
	if err != nil || typ != FHello || json.Unmarshal(payload, &hello) != nil || hello.Token != h.token {
		WriteJSON(conn, FBye, Bye{Reason: "bad token"})
		conn.Close()
		return
	}

	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		// Close raced this attach: never registered, so nothing to detach or
		// drain — say goodbye and close directly.
		WriteJSON(conn, FBye, Bye{Reason: "session closed"})
		conn.Close()
		return
	}
	c := newClient(hello, conn)
	h.nextID++
	c.id = h.nextID
	h.clients = append(h.clients, c)
	h.mu.Unlock()
	go h.writer(c)
	h.recomputeAttach(c)
	h.logf("attached %d %s %dx%d\n", c.id, c.label, c.cols, c.rows)

	for {
		typ, payload, err := ReadFrame(conn)
		if err != nil {
			break
		}
		switch typ {
		case FInput:
			h.mu.Lock()
			f := h.onInput
			h.mu.Unlock()
			if f != nil {
				f(c.id, append([]byte(nil), payload...))
			}
		case FResize:
			var s Size
			if json.Unmarshal(payload, &s) == nil && s.Cols > 0 && s.Rows > 0 {
				h.mu.Lock()
				c.cols, c.rows = s.Cols, s.Rows
				h.mu.Unlock()
				h.recompute()
			}
		case FDetach:
			h.detach(c, ReasonDetached)
			return
		case FQuit:
			h.RequestQuit()
		}
	}
	h.detach(c, "connection closed")
}

// writer is the sole goroutine that writes to c.conn, draining c's queue as
// frames arrive. A client that cannot keep up only ever blocks this
// goroutine (on the network Write itself, if the client's socket buffer is
// full) — never a caller of enqueue, and never another client's writer.
// It exits after successfully writing an FBye frame (nothing follows it) or
// on a write error (the connection is done either way).
func (h *Host) writer(c *client) {
	defer close(c.writerDone)
	for range c.wake {
		for _, f := range c.drain() {
			if err := WriteFrame(c.conn, f.typ, f.payload); err != nil {
				go h.detach(c, "write failed")
				return
			}
			if f.typ == FBye {
				return
			}
		}
	}
}

func (h *Host) detach(c *client, reason string) {
	c.once.Do(func() {
		h.mu.Lock()
		for i, x := range h.clients {
			if x == c {
				h.clients = append(h.clients[:i], h.clients[i+1:]...)
				break
			}
		}
		h.mu.Unlock()

		c.enqueueJSON(FBye, Bye{Reason: reason})
		select {
		case <-c.writerDone:
		case <-time.After(byeWait):
		}
		c.conn.Close() // idempotent; unblocks writer() if it's still stuck mid-write
		h.logf("detached %d %s (%s)\n", c.id, c.label, reason)
		h.recompute()
	})
}

// recompute derives the shared size, notifies clients and the program.
//
// It is wrapped in notifyMu — a lock distinct from h.mu — for the whole
// compute-and-notify sequence. h.mu alone is not enough: it only protects the
// commit of h.cols/h.rows, and notifications go out after it is released, so
// two overlapping recompute calls could commit in one order but notify in
// the other, leaving clients (and the program, via onSize/onClients) with a
// stale view that never resolves until the next change. Serializing the
// whole function makes "last to commit" and "last to notify" the same call.
//
// onSize/onClients are the caller's callbacks; they run here, under
// notifyMu, not under h.mu — but they still must return quickly and must not
// call back into anything that itself calls recompute (including via
// OnSize/OnClients replacing themselves) or the host will deadlock.
//
// The same constraint runs the other way, and is easy to miss: the served
// program must never call into the host from inside its own update loop.
// tui.RunServed's callbacks are `p.Send(...)` on an unbuffered channel that
// only the program's update goroutine receives from, so a synchronous
// Detach/Close/RequestQuit from inside Update would block that
// goroutine on its own p.Send — with notifyMu held, which then hangs every
// later attach and detach too. Route such calls through a tea.Cmd (they run
// on their own goroutine) instead.
func (h *Host) recompute() { h.recomputeAttach(nil) }

// recomputeAttach is recompute for the attach path: attached is the client
// that has just joined. It is always sent the current shared size, and the
// program is always notified of it — even when the shared minimum did not
// change, because a client that has just cleared its screen (and a Bubble Tea
// renderer that only repaints in full on a WindowSizeMsg) would otherwise be
// left with nothing but the next diff lines. A size change still notifies
// exactly once: the broadcast below already includes the new client.
func (h *Host) recomputeAttach(attached *client) {
	h.notifyMu.Lock()
	defer h.notifyMu.Unlock()

	h.mu.Lock()
	cols, rows := 0, 0
	for _, c := range h.clients {
		if cols == 0 || c.cols < cols {
			cols = c.cols
		}
		if rows == 0 || c.rows < rows {
			rows = c.rows
		}
	}
	changed := cols != h.cols || rows != h.rows
	h.cols, h.rows = cols, rows
	infos := h.infosLocked()
	onSize, onClients := h.onSize, h.onClients
	clients := append([]*client(nil), h.clients...)
	h.mu.Unlock()

	if cols > 0 && (changed || attached != nil) {
		if changed {
			for _, c := range clients {
				c.enqueueJSON(FSize, Size{Cols: cols, Rows: rows})
			}
		} else {
			attached.enqueueJSON(FSize, Size{Cols: cols, Rows: rows})
		}
		if onSize != nil {
			onSize(cols, rows)
		}
	}
	b, _ := json.Marshal(infos)
	for _, c := range clients {
		c.enqueue(FClients, b)
	}
	if onClients != nil {
		onClients(infos)
	}
}

// infosLocked snapshots the attached clients. There is no holder: every
// client's input is tagged with its id and the served program decides what
// each one means.
func (h *Host) infosLocked() []ClientInfo {
	out := make([]ClientInfo, 0, len(h.clients))
	for _, c := range h.clients {
		out = append(out, ClientInfo{ID: c.id, Label: c.label, Cols: c.cols, Rows: c.rows, UTF8: c.utf8})
	}
	return out
}

func (h *Host) Clients() []ClientInfo {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.infosLocked()
}

func (h *Host) Size() (int, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cols, h.rows
}

func (h *Host) AnyASCII() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.clients {
		if !c.utf8 {
			return true
		}
	}
	return false
}

// RequestQuit asks the served program to end the session, exactly as a
// client's quit frame does. It is also how the host process routes a signal
// (SIGTERM from `be-code sessions kill`, SIGINT) to the program, so the
// program's own shutdown — handoff briefing, tool cleanup — still runs.
func (h *Host) RequestQuit() {
	h.mu.Lock()
	f := h.onQuit
	if f == nil {
		h.quitPending = true // replayed by OnQuit
	}
	h.mu.Unlock()
	if f != nil {
		f()
	}
}

// ReasonSwitchPrefix prefixes a bye that tells the client to reattach to
// another live session in the same terminal.
const ReasonSwitchPrefix = "switch:"

// byIDLocked is byID's search, for callers that already hold h.mu.
func (h *Host) byIDLocked(id int) *client {
	for _, c := range h.clients {
		if c.id == id {
			return c
		}
	}
	return nil
}

func (h *Host) byID(id int) *client {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.byIDLocked(id)
}

// Detach drops one client with the ordinary detached reason.
func (h *Host) Detach(id int) {
	if c := h.byID(id); c != nil {
		h.detach(c, ReasonDetached)
	}
}

// Switch hands one client over to another live session.
func (h *Host) Switch(id int, code string) {
	if c := h.byID(id); c != nil {
		h.detach(c, ReasonSwitchPrefix+code)
	}
}

// SetOverlay records a client's private input rows and sends them. The
// same rows are appended to every shared frame that client receives, so a
// full repaint never erases them. An unchanged overlay is not re-sent.
//
// h.mu is held across the enqueue (enqueue itself never blocks — it only
// appends under c.qmu and does a non-blocking wake — and nothing that holds
// c.qmu ever takes h.mu), so this can never interleave with fanout.Write's
// own read-then-enqueue of the same client's overlay: without that, a
// concurrent frame write could snapshot this client's overlay just before
// this call updates it, then enqueue that stale value after this call's own
// FOverlay, permanently reverting the client's rendered overlay with no next
// frame to correct it (a private keystroke need not change the shared view).
func (h *Host) SetOverlay(id int, s string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.byIDLocked(id)
	if c == nil || c.overlay == s {
		return
	}
	c.overlay = s
	c.enqueue(FOverlay, []byte(s))
}

// Close says goodbye to every client and stops accepting.
func (h *Host) Close(reason string) {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return
	}
	h.closing = true
	// Close the listener before detaching: this stops Serve's Accept loop
	// and, combined with handle's closing check, guarantees a connection
	// accepted concurrently with this Close is told goodbye directly instead
	// of being registered (and its writer leaked) after the snapshot below.
	if h.ln != nil {
		h.ln.Close()
	}
	clients := append([]*client(nil), h.clients...)
	h.mu.Unlock()
	for _, c := range clients {
		h.detach(c, reason) // each bounded by byeWait; a stalled client cannot delay this loop
	}
}

// fanout copies program output to every attached client, followed by that
// client's private overlay (if it has one) so a full repaint never erases
// it.
type fanout struct{ h *Host }

// Write holds h.mu across every client's pair of enqueues (see SetOverlay's
// doc for why: releasing it between reading c.overlay and enqueueing it
// would let a concurrent SetOverlay's own newer value be overwritten by this
// call's now-stale one). enqueue itself never blocks, so this never holds
// h.mu across anything that could stall.
func (f fanout) Write(p []byte) (int, error) {
	cp := append([]byte(nil), p...)
	f.h.mu.Lock()
	defer f.h.mu.Unlock()
	for _, c := range f.h.clients {
		c.enqueue(FOutput, cp)
		if c.overlay != "" {
			c.enqueue(FOverlay, []byte(c.overlay))
		}
	}
	return len(p), nil
}
