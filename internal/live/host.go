package live

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
)

type client struct {
	id     int
	label  string
	cols   int
	rows   int
	utf8   bool
	conn   net.Conn
	queue  chan []byte
	closed chan struct{}
	once   sync.Once

	// writeMu serializes frames onto conn. WriteFrame/WriteJSON each issue two
	// separate Write calls (header, then payload); this client's own writer
	// goroutine (fanout output) and Host.recompute/detach (broadcasting size,
	// client-list and bye frames to every connection from whichever goroutine
	// triggered the change) can call into the same conn concurrently, and
	// without this lock their header/payload writes can interleave and
	// corrupt the frame stream.
	writeMu sync.Mutex
}

func (c *client) writeFrame(t FrameType, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return WriteFrame(c.conn, t, payload)
}

func (c *client) writeJSON(t FrameType, v any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return WriteJSON(c.conn, t, v)
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
	c := &client{label: hello.Label, cols: hello.Cols, rows: hello.Rows, utf8: hello.UTF8, conn: conn,
		queue: make(chan []byte, 8), closed: make(chan struct{})}
	h.mu.Lock()
	h.nextID++
	c.id = h.nextID
	h.clients = append(h.clients, c)
	h.holder = c // newest attacher holds input
	h.mu.Unlock()
	go h.writer(c)
	h.recompute()
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

// writer drains a client's queue; a client that cannot keep up loses its
// oldest frames (each frame is a full repaint) instead of stalling others.
func (h *Host) writer(c *client) {
	for {
		select {
		case <-c.closed:
			return
		case p := <-c.queue:
			if err := c.writeFrame(FOutput, p); err != nil {
				h.detach(c, "write failed")
				return
			}
		}
	}
}

func (c *client) enqueue(p []byte) {
	select {
	case c.queue <- p:
	default:
		select { // drop the oldest, keep the newest
		case <-c.queue:
		default:
		}
		select {
		case c.queue <- p:
		default:
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
		close(c.closed)
		c.writeJSON(FBye, Bye{Reason: reason})
		c.conn.Close()
		h.logf("detached %d %s (%s)\n", c.id, c.label, reason)
		h.recompute()
	})
}

// recompute derives the shared size, notifies clients and the program.
func (h *Host) recompute() {
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
			c.writeJSON(FSize, Size{Cols: cols, Rows: rows})
		}
		if onSize != nil {
			onSize(cols, rows)
		}
	}
	b, _ := json.Marshal(infos)
	for _, c := range clients {
		c.writeFrame(FClients, b)
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
	clients := append([]*client(nil), h.clients...)
	h.mu.Unlock()
	for _, c := range clients {
		h.detach(c, reason)
	}
	if h.ln != nil {
		h.ln.Close()
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
		c.enqueue(cp)
	}
	return len(p), nil
}
