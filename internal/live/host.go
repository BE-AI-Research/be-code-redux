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

// maxQueuedOutput caps how many FOutput frames a client's queue may hold
// before the oldest is evicted. Only FOutput entries are ever evicted (each
// is a full repaint, so only the newest matters); control frames (FSize,
// FClients, FBye) are never dropped.
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
// blocks: a full queue of FOutput frames evicts its oldest FOutput entry to
// make room (control frames are never evicted, so this queue can grow beyond
// maxQueuedOutput only through control traffic, which is bounded by real
// events — attach/detach/resize/takeover — not by output volume).
func (c *client) enqueue(t FrameType, payload []byte) {
	c.qmu.Lock()
	if t == FOutput {
		n := 0
		for _, f := range c.queue {
			if f.typ == FOutput {
				n++
			}
		}
		if n >= maxQueuedOutput {
			for i, f := range c.queue {
				if f.typ == FOutput {
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
	holder  *client
	nextID  int
	cols    int
	rows    int

	// notifyMu orders recompute's compute-and-notify as a whole (see
	// recompute for why h.mu alone is not enough).
	notifyMu sync.Mutex

	inR       *io.PipeReader
	inW       *io.PipeWriter
	inCh      chan []byte   // holder keystrokes, drained by pumpInput into inW
	stopInput chan struct{} // closed once by Close to stop pumpInput

	onSize    func(cols, rows int)
	onClients func([]ClientInfo)
	onQuit    func()
	closing   bool
}

func NewHost(token string, log io.Writer) *Host {
	r, w := io.Pipe()
	h := &Host{token: token, log: log, inR: r, inW: w, inCh: make(chan []byte, 256), stopInput: make(chan struct{})}
	go h.pumpInput()
	return h
}

// pumpInput is the sole writer to inW. It is a dedicated goroutine so that a
// program that is momentarily not reading InputReader() (or never reads it,
// as in a headless caller) cannot block a client connection's read loop: a
// per-connection goroutine only ever sends to inCh, which is non-blocking,
// so it stays free to process that same client's control frames (resize,
// takeover, detach, quit) instead of stalling behind an unconsumed pipe.
// It exits on stopInput rather than a closed inCh, since inCh may still have
// concurrent senders right up to Close.
func (h *Host) pumpInput() {
	for {
		select {
		case <-h.stopInput:
			return
		case p := <-h.inCh:
			if _, err := h.inW.Write(p); err != nil {
				return
			}
		}
	}
}

func (h *Host) OnSize(f func(int, int))        { h.mu.Lock(); h.onSize = f; h.mu.Unlock() }
func (h *Host) OnClients(f func([]ClientInfo)) { h.mu.Lock(); h.onClients = f; h.mu.Unlock() }
func (h *Host) OnQuit(f func())                { h.mu.Lock(); h.onQuit = f; h.mu.Unlock() }
func (h *Host) InputReader() io.Reader         { return h.inR }
func (h *Host) Output() io.Writer              { return fanout{h} }

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
	h.holder = c // newest attacher holds input
	h.mu.Unlock()
	go h.writer(c)
	h.recompute()
	// recompute only broadcasts FSize when the shared size changed, so a
	// client whose own dimensions are not the new minimum would otherwise
	// never learn the render size at all; tell it directly.
	if cols, rows := h.Size(); cols > 0 {
		c.enqueueJSON(FSize, Size{Cols: cols, Rows: rows})
	}
	h.logf("attached %d %s %dx%d\n", c.id, c.label, c.cols, c.rows)

	for {
		typ, payload, err := ReadFrame(conn)
		if err != nil {
			break
		}
		switch typ {
		case FInput:
			h.mu.Lock()
			isHolder := h.holder == c
			h.mu.Unlock()
			if isHolder {
				select {
				case h.inCh <- payload:
				default: // program isn't keeping up; drop rather than block this connection
				}
			}
		case FResize:
			var s Size
			if json.Unmarshal(payload, &s) == nil && s.Cols > 0 && s.Rows > 0 {
				h.mu.Lock()
				c.cols, c.rows = s.Cols, s.Rows
				h.mu.Unlock()
				h.recompute()
			}
		case FTakeover:
			h.mu.Lock()
			h.holder = c
			h.mu.Unlock()
			h.recompute()
		case FDetach:
			h.detach(c, "detached")
			return
		case FQuit:
			h.mu.Lock()
			f := h.onQuit
			h.mu.Unlock()
			if f != nil {
				f()
			}
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
		if h.holder == c {
			h.holder = nil
			if n := len(h.clients); n > 0 {
				h.holder = h.clients[n-1] // most recently attached remaining client
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
func (h *Host) recompute() {
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

	if changed && cols > 0 {
		for _, c := range clients {
			c.enqueueJSON(FSize, Size{Cols: cols, Rows: rows})
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

func (h *Host) infosLocked() []ClientInfo {
	out := make([]ClientInfo, 0, len(h.clients))
	for _, c := range h.clients {
		out = append(out, ClientInfo{ID: c.id, Label: c.label, Holder: c == h.holder, Cols: c.cols, Rows: c.rows, UTF8: c.utf8})
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

func (h *Host) DetachHolder() {
	h.mu.Lock()
	c := h.holder
	h.mu.Unlock()
	if c != nil {
		h.detach(c, "detached")
	}
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
	close(h.stopInput)
	h.inW.Close()
}

// fanout copies program output to every attached client.
type fanout struct{ h *Host }

func (f fanout) Write(p []byte) (int, error) {
	cp := append([]byte(nil), p...)
	f.h.mu.Lock()
	clients := append([]*client(nil), f.h.clients...)
	f.h.mu.Unlock()
	for _, c := range clients {
		c.enqueue(FOutput, cp)
	}
	return len(p), nil
}
