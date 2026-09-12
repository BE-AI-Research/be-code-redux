# Session Handoff Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A running BE-Code TUI session can be attached to from any other terminal on the same machine (including over SSH) by resume code; all attached terminals mirror it at the smallest size, the newest attacher holds the keyboard, and the session persists with zero clients.

**Architecture:** The TUI process is the session ("wish"-style): `be-code` launches a detached `be-code --session-host <code>` that runs the Bubble Tea program with `WithInput`/`WithOutput` on internal pipes and serves it over a Unix socket in `~/.be-code/live/`. Thin clients (`be-code attach`) put their terminal in raw mode and forward keystrokes and sizes; the host fans identical frames out to every client, elects the input holder, computes the shared size, and injects `WindowSizeMsg`. A compact layout keeps the TUI usable at phone-sized shared sizes.

**Tech Stack:** Go 1.25, Bubble Tea v1.3.10 (`tea.WithInput`, `tea.WithOutput`, `tea.WithoutSignalHandler`; no `WithWindowSize`, so the host sends `tea.WindowSizeMsg`), `github.com/charmbracelet/x/term` (already in go.mod as indirect; make it direct) for raw mode and size, Unix domain sockets via `net` (Windows 10+ supports AF_UNIX in Go).

**Spec:** `be-code/docs/superpowers/specs/2026-09-11-session-handoff-design.md`

## Global Constraints

- Records live in `~/.be-code/live/<code>.json` (mode 0600) with fields `pid, socket, workspace, model, startedAt, token`; the socket is `~/.be-code/live/<code>.sock`. Dead-pid records are pruned.
- Frame: 1-byte type, 4-byte big-endian length, payload. Client→host: `hello` {token, cols, rows, label, utf8}, `input`, `resize` {cols, rows}, `detach`, `takeover`, `quit`. Host→client: `output`, `size` {cols, rows}, `clients` (list, holder marked), `bye` {reason}. Wrong token → `bye` + close.
- The most recently attached client holds input; viewers' keystrokes are swallowed; chords: Ctrl+] then `t` = takeover, Ctrl+] then `d` or Ctrl+] Ctrl+] = detach (handled in the client, sent as frames). Holder detach passes input to the most recently attached remaining client.
- Program size = min cols × min rows over attached clients; every change sends exactly one `WindowSizeMsg`; clients clear their screen on attach and on every `size` change.
- Per-client bounded frame queue (capacity 8): a slow client drops its oldest frames; the host never blocks on a client.
- Compact layout engages when the shared size is under 70 columns or under 20 rows unless config `layout` is `full`; `layout: compact` forces it.
- Headless `run` and `--plain` never host; `--no-host` runs the TUI in-process.
- **Deviation from the spec (ruled during planning):** Windows uses an AF_UNIX socket at the same `~/.be-code/live/<code>.sock` path (supported by Go's `net` on Windows 10 1803+) instead of a named pipe, so one transport serves every platform.
- **Deviation from the spec (ruled during planning):** all clients receive identical frames, so the bottom line cannot differ per client. Instead it shows `⧉ N · input: <holder label> · Ctrl+] d detach · Ctrl+] t take over` for everyone.
- Commits go on the feature branch, each message ending with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`; every task ends with `make -f build.mk verify`.

---

### Task 1: Frame protocol and live-session records

**Files:**
- Create: `internal/live/frame.go`, `internal/live/record.go`
- Test: `internal/live/frame_test.go`, `internal/live/record_test.go`

**Interfaces (produced):**

```go
package live

type FrameType byte
const (
	FHello FrameType = iota + 1; FInput; FResize; FDetach; FTakeover; FQuit // client → host
	FOutput; FSize; FClients; FBye                                          // host → client
)
type Hello struct { Token string `json:"token"`; Cols, Rows int; Label string `json:"label"`; UTF8 bool `json:"utf8"` }
type Size struct { Cols int `json:"cols"`; Rows int `json:"rows"` }
type ClientInfo struct { ID int `json:"id"`; Label string `json:"label"`; Holder bool `json:"holder"`; Cols, Rows int; UTF8 bool `json:"utf8"` }
type Bye struct { Reason string `json:"reason"` }
func WriteFrame(w io.Writer, t FrameType, payload []byte) error
func ReadFrame(r io.Reader) (FrameType, []byte, error)   // io.EOF at a clean end; error on a frame > 16 MiB
func WriteJSON(w io.Writer, t FrameType, v any) error
type Record struct { Code string `json:"code"`; PID int `json:"pid"`; Socket string `json:"socket"`; Workspace string `json:"workspace"`; Model string `json:"model"`; StartedAt time.Time `json:"startedAt"`; Token string `json:"token"` }
func Dir() (string, error)                       // ~/.be-code/live, created 0700
func SocketPath(dir, code string) string         // <dir>/<code>.sock
func (r Record) Save(dir string) error           // <dir>/<code>.json, 0600, atomic (tmp + rename)
func Load(dir, code string) (*Record, error)
func List(dir string) ([]Record, error)          // live only; dead-pid records and their sockets are removed
func Remove(dir, code string) error              // json + sock
```

- [ ] **Step 1: Write the failing tests**

`internal/live/frame_test.go`:

```go
package live

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, FInput, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(&buf, FSize, Size{Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	typ, payload, err := ReadFrame(&buf)
	if err != nil || typ != FInput || string(payload) != "abc" {
		t.Fatalf("first frame: %v %v %q", typ, err, payload)
	}
	typ, payload, err = ReadFrame(&buf)
	var s Size
	if err != nil || typ != FSize || json.Unmarshal(payload, &s) != nil || s.Cols != 80 || s.Rows != 24 {
		t.Fatalf("second frame: %v %v %q", typ, err, payload)
	}
	if _, _, err = ReadFrame(&buf); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestFrameRejectsOversize(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{byte(FInput), 0xff, 0xff, 0xff, 0xff})
	if _, _, err := ReadFrame(&buf); err == nil || err == io.EOF {
		t.Fatal("oversize frame must be rejected")
	}
}
```

`internal/live/record_test.go`:

```go
package live

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordSaveLoadListPrune(t *testing.T) {
	dir := t.TempDir()
	me := Record{Code: "ABC123", PID: os.Getpid(), Socket: SocketPath(dir, "ABC123"), Workspace: "/w", Model: "m", StartedAt: time.Now(), Token: "t"}
	if err := me.Save(dir); err != nil {
		t.Fatal(err)
	}
	dead := Record{Code: "DEAD00", PID: 999999999, Socket: SocketPath(dir, "DEAD00"), Token: "x"}
	dead.Save(dir)
	os.WriteFile(dead.Socket, nil, 0o600) // stale socket file
	got, err := Load(dir, "ABC123")
	if err != nil || got.Token != "t" || got.Workspace != "/w" {
		t.Fatalf("load: %+v %v", got, err)
	}
	info, _ := os.Stat(filepath.Join(dir, "ABC123.json"))
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode %o", info.Mode().Perm())
	}
	live, err := List(dir)
	if err != nil || len(live) != 1 || live[0].Code != "ABC123" {
		t.Fatalf("list: %+v %v", live, err)
	}
	if _, err := os.Stat(dead.Socket); !os.IsNotExist(err) {
		t.Fatal("stale socket not removed")
	}
	if err := Remove(dir, "ABC123"); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, "ABC123"); err == nil {
		t.Fatal("record still loadable after Remove")
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/live`; expected: package does not exist / undefined symbols.

- [ ] **Step 3: Implement**

`internal/live/frame.go`:

```go
// Package live hosts a running BE-Code TUI session over a local socket so
// other terminals (local, IDE, SSH) can attach to it by resume code. The
// TUI process is the session; clients are thin raw-mode pipes.
package live

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

type FrameType byte

const (
	FHello FrameType = iota + 1
	FInput
	FResize
	FDetach
	FTakeover
	FQuit
	FOutput
	FSize
	FClients
	FBye
)

const maxFrame = 16 << 20

type Hello struct {
	Token string `json:"token"`
	Cols  int    `json:"cols"`
	Rows  int    `json:"rows"`
	Label string `json:"label"`
	UTF8  bool   `json:"utf8"`
}

type Size struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

type ClientInfo struct {
	ID     int    `json:"id"`
	Label  string `json:"label"`
	Holder bool   `json:"holder"`
	Cols   int    `json:"cols"`
	Rows   int    `json:"rows"`
	UTF8   bool   `json:"utf8"`
}

type Bye struct {
	Reason string `json:"reason"`
}

// WriteFrame writes one frame: type byte, big-endian uint32 length, payload.
func WriteFrame(w io.Writer, t FrameType, payload []byte) error {
	hdr := make([]byte, 5)
	hdr[0] = byte(t)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// WriteJSON writes a frame whose payload is v as JSON.
func WriteJSON(w io.Writer, t FrameType, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(w, t, b)
}

// ReadFrame reads one frame. A clean end of stream is io.EOF.
func ReadFrame(r io.Reader) (FrameType, []byte, error) {
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(r, hdr); err != nil {
		if err == io.ErrUnexpectedEOF {
			err = io.EOF
		}
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, errors.New("live: frame too large")
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return FrameType(hdr[0]), payload, nil
}
```

`internal/live/record.go`:

```go
package live

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
)

// Record advertises a live session host.
type Record struct {
	Code      string    `json:"code"`
	PID       int       `json:"pid"`
	Socket    string    `json:"socket"`
	Workspace string    `json:"workspace"`
	Model     string    `json:"model"`
	StartedAt time.Time `json:"startedAt"`
	Token     string    `json:"token"`
}

// Dir is ~/.be-code/live, created on demand.
func Dir() (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(base, "live")
	return d, os.MkdirAll(d, 0o700)
}

func SocketPath(dir, code string) string { return filepath.Join(dir, code+".sock") }
func recordPath(dir, code string) string { return filepath.Join(dir, code+".json") }

// Save writes the record atomically with mode 0600.
func (r Record) Save(dir string) error {
	b, err := json.MarshalIndent(r, "", " ")
	if err != nil {
		return err
	}
	p := recordPath(dir, r.Code)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func Load(dir, code string) (*Record, error) {
	b, err := os.ReadFile(recordPath(dir, code))
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if r.Code == "" || r.Socket == "" {
		return nil, errors.New("live: malformed record")
	}
	return &r, nil
}

// List returns live records; records whose process is gone are removed
// along with their stale socket files.
func List(dir string) ([]Record, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		code := strings.TrimSuffix(e.Name(), ".json")
		r, err := Load(dir, code)
		if err != nil || r.PID <= 0 || !processAlive(r.PID) {
			_ = Remove(dir, code)
			continue
		}
		out = append(out, *r)
	}
	return out, nil
}

func Remove(dir, code string) error {
	err := os.Remove(recordPath(dir, code))
	_ = os.Remove(SocketPath(dir, code))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
```

Copy `processAlive` from `internal/ide/pid_unix.go` and `pid_windows.go` into `internal/live/pid_unix.go` / `pid_windows.go` (same build tags and bodies; the Windows one includes `defer p.Release()`).

- [ ] **Step 4: Run** `go test ./internal/live -v`; expected PASS.
- [ ] **Step 5: Verify and commit** `make -f build.mk verify`; `git add internal/live && git commit -m "live: frame protocol and session records"` (append the co-author line).

---

### Task 2: Session host

**Files:**
- Create: `internal/live/host.go`
- Test: `internal/live/host_test.go`

**Interfaces:**
- Consumes: Task 1 frames and records.
- Produces:

```go
type Host struct { /* unexported */ }
func NewHost(token string, log io.Writer) *Host
func (h *Host) Listen(sockPath string) error          // removes a stale socket file first; 0600 via umask handling
func (h *Host) Serve()                                // accept loop; returns when Close is called
func (h *Host) Close(reason string)                   // bye to every client, close listener
func (h *Host) InputReader() io.Reader                // the program's stdin: holder keystrokes
func (h *Host) Output() io.Writer                     // the program's stdout: fanned out to every client
func (h *Host) OnSize(func(cols, rows int))           // called on every shared-size change (send WindowSizeMsg)
func (h *Host) OnClients(func([]ClientInfo))          // called on every attach/detach/holder change
func (h *Host) OnQuit(func())                         // a client sent `quit` (sessions kill)
func (h *Host) Clients() []ClientInfo
func (h *Host) Size() (cols, rows int)                // 0,0 with no clients
func (h *Host) DetachHolder()                         // used by /detach
func (h *Host) AnyASCII() bool                        // some attached client lacks UTF-8
```

- [ ] **Step 1: Write the failing tests** (`internal/live/host_test.go`) using a real Unix socket in a temp dir (short path: `t.TempDir()` can exceed the 104-byte limit on macOS; use `os.MkdirTemp("/tmp", "live")` when `runtime.GOOS != "windows"`):

```go
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
	go func() { WriteFrame(a.conn, FInput, []byte("A")) }()
	WriteFrame(b.conn, FInput, []byte("B"))
	n, _ := h.InputReader().Read(buf)
	if string(buf[:n]) != "B" {
		t.Fatalf("program got %q, want B", buf[:n])
	}
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
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/live -run TestHost`; expected undefined `NewHost`.

- [ ] **Step 3: Implement** `internal/live/host.go`:

```go
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
}

// Host serves one TUI session to any number of attached terminals.
type Host struct {
	token string
	log   io.Writer
	ln    net.Listener

	mu      sync.Mutex
	clients []*client // attach order
	holder  *client
	nextID  int
	cols    int
	rows    int

	inR *io.PipeReader
	inW *io.PipeWriter

	onSize    func(cols, rows int)
	onClients func([]ClientInfo)
	onQuit    func()
	closing   bool
}

func NewHost(token string, log io.Writer) *Host {
	r, w := io.Pipe()
	return &Host{token: token, log: log, inR: r, inW: w}
}

func (h *Host) OnSize(f func(int, int))         { h.mu.Lock(); h.onSize = f; h.mu.Unlock() }
func (h *Host) OnClients(f func([]ClientInfo)) { h.mu.Lock(); h.onClients = f; h.mu.Unlock() }
func (h *Host) OnQuit(f func())                { h.mu.Lock(); h.onQuit = f; h.mu.Unlock() }
func (h *Host) InputReader() io.Reader         { return h.inR }
func (h *Host) Output() io.Writer              { return fanout{h} }

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
	fmt.Fprintf(h.log, "attached %d %s %dx%d\n", c.id, c.label, c.cols, c.rows)

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
				h.inW.Write(payload)
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
			if err := WriteFrame(c.conn, FOutput, p); err != nil {
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
		WriteJSON(c.conn, FBye, Bye{Reason: reason})
		c.conn.Close()
		fmt.Fprintf(h.log, "detached %d %s (%s)\n", c.id, c.label, reason)
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
			WriteJSON(c.conn, FSize, Size{Cols: cols, Rows: rows})
		}
		if onSize != nil {
			onSize(cols, rows)
		}
	}
	b, _ := json.Marshal(infos)
	for _, c := range clients {
		WriteFrame(c.conn, FClients, b)
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
```

Add `internal/live/umask_unix.go` (`//go:build !windows`: `func syscallUmask(m int) int { return syscall.Umask(m) }`) and `umask_windows.go` (`func syscallUmask(int) int { return 0 }`).

- [ ] **Step 4: Run** `go test ./internal/live -race -v`; expected PASS.
- [ ] **Step 5: Verify and commit** — `make -f build.mk verify`; commit `live: session host with fan-out, holder election and shared size`.

---

### Task 3: Attach client

**Files:**
- Create: `internal/live/client.go`, `internal/live/chord.go`
- Test: `internal/live/chord_test.go`, `internal/live/client_test.go`

**Interfaces (produced):**

```go
type Chord struct { /* unexported */ }
// Feed splits keystrokes into forwardable bytes and actions. Ctrl+] (0x1d)
// starts a chord; the next key decides: 'd' or 0x1d → ActionDetach, 't' →
// ActionTakeover, anything else → both bytes are forwarded verbatim. The
// chord expires after 1s (the pending 0x1d is forwarded).
type Action int
const ( ActionNone Action = iota; ActionDetach; ActionTakeover )
func (c *Chord) Feed(b []byte, now time.Time) (forward []byte, act Action)

type AttachOptions struct { Label string; View bool; Stdin io.Reader; Stdout io.Writer; Raw func() (restore func(), err error); Size func() (cols, rows int); UTF8 bool }
// Attach connects to a live session and runs until detach, host exit, or
// ctx cancel. Returns the host's bye reason ("" on a local detach).
func Attach(ctx context.Context, rec *Record, opt AttachOptions) (string, error)
func DefaultAttachOptions() AttachOptions   // real terminal: x/term raw mode, os.Stdin/Stdout, size polling, label from env
func ClientLabel() string                  // "vscode" | "ssh from <ip>" | "local", plus pid
```

- [ ] **Step 1: Write the failing tests**

`internal/live/chord_test.go`:

```go
package live

import (
	"testing"
	"time"
)

func TestChordDetachAndTakeover(t *testing.T) {
	now := time.Now()
	var c Chord
	fwd, act := c.Feed([]byte("ab"), now)
	if string(fwd) != "ab" || act != ActionNone {
		t.Fatalf("plain: %q %v", fwd, act)
	}
	fwd, act = c.Feed([]byte{0x1d}, now)
	if len(fwd) != 0 || act != ActionNone {
		t.Fatalf("pending chord leaked: %q %v", fwd, act)
	}
	if _, act = c.Feed([]byte("d"), now); act != ActionDetach {
		t.Fatalf("Ctrl+] d: %v", act)
	}
	c.Feed([]byte{0x1d}, now)
	if _, act = c.Feed([]byte{0x1d}, now); act != ActionDetach {
		t.Fatalf("Ctrl+] Ctrl+]: %v", act)
	}
	c.Feed([]byte{0x1d}, now)
	if _, act = c.Feed([]byte("t"), now); act != ActionTakeover {
		t.Fatalf("Ctrl+] t: %v", act)
	}
	c.Feed([]byte{0x1d}, now)
	fwd, act = c.Feed([]byte("x"), now)
	if string(fwd) != "\x1dx" || act != ActionNone {
		t.Fatalf("unknown chord key must forward both: %q %v", fwd, act)
	}
	c.Feed([]byte{0x1d}, now)
	fwd, _ = c.Feed([]byte("y"), now.Add(2*time.Second))
	if string(fwd) != "\x1dy" {
		t.Fatalf("expired chord must forward: %q", fwd)
	}
}
```

`internal/live/client_test.go` (end-to-end against Task 2's host with in-memory stdin/stdout):

```go
package live

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestAttachRoundTripAndDetach(t *testing.T) {
	h, sock := startHost(t)
	rec := &Record{Code: "T", PID: os.Getpid(), Socket: sock, Token: "tok"}
	stdinR, stdinW := io.Pipe()
	var stdout bytes.Buffer
	done := make(chan string, 1)
	go func() {
		reason, _ := Attach(context.Background(), rec, AttachOptions{
			Label: "test", Stdin: stdinR, Stdout: &stdout,
			Raw: func() (func(), error) { return func() {}, nil },
			Size: func() (int, int) { return 90, 30 }, UTF8: true,
		})
		done <- reason
	}()
	within(t, time.Second, func() bool { return len(h.Clients()) == 1 })
	if c, r := h.Size(); c != 90 || r != 30 {
		t.Fatalf("host size %dx%d", c, r)
	}
	// keystrokes reach the program; program output reaches stdout
	stdinW.Write([]byte("hi"))
	buf := make([]byte, 4)
	n, _ := h.InputReader().Read(buf)
	if string(buf[:n]) != "hi" {
		t.Fatalf("program got %q", buf[:n])
	}
	h.Output().Write([]byte("FRAME"))
	within(t, time.Second, func() bool { return strings.Contains(stdout.String(), "FRAME") })
	// Ctrl+] d detaches locally
	stdinW.Write([]byte{0x1d, 'd'})
	select {
	case reason := <-done:
		if reason != "" {
			t.Fatalf("local detach reason = %q", reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Attach did not return after the detach chord")
	}
	within(t, time.Second, func() bool { return len(h.Clients()) == 0 })
}

func TestAttachReturnsHostBye(t *testing.T) {
	h, sock := startHost(t)
	rec := &Record{Code: "T", PID: os.Getpid(), Socket: sock, Token: "tok"}
	stdinR, _ := io.Pipe()
	done := make(chan string, 1)
	go func() {
		reason, _ := Attach(context.Background(), rec, AttachOptions{Label: "t", Stdin: stdinR, Stdout: io.Discard,
			Raw: func() (func(), error) { return func() {}, nil }, Size: func() (int, int) { return 80, 24 }, UTF8: true})
		done <- reason
	}()
	within(t, time.Second, func() bool { return len(h.Clients()) == 1 })
	h.Close("session ended")
	select {
	case reason := <-done:
		if reason != "session ended" {
			t.Fatalf("reason = %q", reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Attach did not return on host close")
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/live -run 'TestChord|TestAttach'`; expected undefined symbols.

- [ ] **Step 3: Implement**

`internal/live/chord.go`:

```go
package live

import "time"

type Action int

const (
	ActionNone Action = iota
	ActionDetach
	ActionTakeover
)

const chordKey = 0x1d // Ctrl+]
const chordWindow = time.Second

// Chord recognises the Ctrl+] prefix chords in a keystroke stream.
type Chord struct {
	pending bool
	since   time.Time
}

func (c *Chord) Feed(b []byte, now time.Time) ([]byte, Action) {
	var out []byte
	for _, k := range b {
		if c.pending {
			c.pending = false
			if now.Sub(c.since) > chordWindow {
				out = append(out, chordKey)
			} else {
				switch k {
				case 'd', chordKey:
					return out, ActionDetach
				case 't':
					return out, ActionTakeover
				default:
					out = append(out, chordKey)
				}
			}
		}
		if k == chordKey {
			c.pending, c.since = true, now
			continue
		}
		out = append(out, k)
	}
	return out, ActionNone
}
```

`internal/live/client.go`:

```go
package live

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
)

type AttachOptions struct {
	Label  string
	View   bool // attach as a viewer: input is never sent
	Stdin  io.Reader
	Stdout io.Writer
	Raw    func() (restore func(), err error)
	Size   func() (cols, rows int)
	UTF8   bool
}

// ClientLabel describes this terminal for the clients list.
func ClientLabel() string {
	base := "local"
	if os.Getenv("TERM_PROGRAM") == "vscode" {
		base = "vscode"
	} else if sc := os.Getenv("SSH_CONNECTION"); sc != "" {
		if f := strings.Fields(sc); len(f) > 0 {
			base = "ssh from " + f[0]
		}
	}
	return fmt.Sprintf("%s (pid %d)", base, os.Getpid())
}

func DefaultAttachOptions() AttachOptions {
	fd := int(os.Stdin.Fd())
	return AttachOptions{
		Label:  ClientLabel(),
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Raw: func() (func(), error) {
			st, err := term.MakeRaw(uintptr(fd))
			if err != nil {
				return nil, err
			}
			return func() { term.Restore(uintptr(fd), st) }, nil
		},
		Size: func() (int, int) {
			c, r, err := term.GetSize(uintptr(fd))
			if err != nil || c == 0 || r == 0 {
				return 80, 24
			}
			return c, r
		},
		UTF8: strings.Contains(strings.ToLower(os.Getenv("LANG")+os.Getenv("LC_ALL")+os.Getenv("LC_CTYPE")), "utf"),
	}
}

const clearScreen = "\x1b[2J\x1b[H"

// Attach runs a terminal client until detach, host exit, or ctx cancel.
func Attach(ctx context.Context, rec *Record, opt AttachOptions) (string, error) {
	conn, err := net.Dial("unix", rec.Socket)
	if err != nil {
		return "", fmt.Errorf("live: %w", err)
	}
	defer conn.Close()
	cols, rows := opt.Size()
	if err := WriteJSON(conn, FHello, Hello{Token: rec.Token, Cols: cols, Rows: rows, Label: opt.Label, UTF8: opt.UTF8}); err != nil {
		return "", err
	}
	restore, err := opt.Raw()
	if err != nil {
		return "", err
	}
	defer restore()
	io.WriteString(opt.Stdout, clearScreen)

	result := make(chan string, 2)
	// host → stdout
	go func() {
		for {
			typ, p, err := ReadFrame(conn)
			if err != nil {
				result <- "connection closed"
				return
			}
			switch typ {
			case FOutput:
				opt.Stdout.Write(p)
			case FSize:
				io.WriteString(opt.Stdout, clearScreen)
			case FBye:
				var b Bye
				json.Unmarshal(p, &b)
				result <- b.Reason
				return
			}
		}
	}()
	// stdin → host, with chords
	go func() {
		var chord Chord
		buf := make([]byte, 4096)
		for {
			n, err := opt.Stdin.Read(buf)
			if err != nil {
				result <- ""
				return
			}
			fwd, act := chord.Feed(buf[:n], time.Now())
			switch act {
			case ActionDetach:
				WriteFrame(conn, FDetach, nil)
				result <- ""
				return
			case ActionTakeover:
				WriteFrame(conn, FTakeover, nil)
			}
			if len(fwd) > 0 && !opt.View {
				if err := WriteFrame(conn, FInput, fwd); err != nil {
					result <- "connection closed"
					return
				}
			}
		}
	}()
	// size polling (portable; SIGWINCH is unix-only)
	go func() {
		lc, lr := cols, rows
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c, r := opt.Size()
				if c != lc || r != lr {
					lc, lr = c, r
					WriteJSON(conn, FResize, Size{Cols: c, Rows: r})
				}
			}
		}
	}()
	select {
	case reason := <-result:
		io.WriteString(opt.Stdout, clearScreen)
		return reason, nil
	case <-ctx.Done():
		WriteFrame(conn, FDetach, nil)
		io.WriteString(opt.Stdout, clearScreen)
		return "", ctx.Err()
	}
}
```

Make `github.com/charmbracelet/x/term` a direct dependency (`go get github.com/charmbracelet/x/term@v0.2.2 && go mod tidy`). Terminal size is polled every 500 ms (portable; SIGWINCH is unix-only), so no per-OS size files are needed.

- [ ] **Step 4: Run** `go test ./internal/live -race -v`; expected PASS.
- [ ] **Step 5: Verify and commit** — `make -f build.mk verify`; commit `live: attach client with chords and size polling`.

---

### Task 4: TUI served mode and the clients view

**Files:**
- Modify: `internal/tui/tui.go` (`RunServed`, `clientsMsg`, idle limit, bottom line, `/clients`, `/detach`), `internal/tui/palette.go` (menu entries), `internal/tui/clipboard.go` (writer target), `internal/ui/common.go` (commands), `internal/config/config.go` (`LiveIdleLimit`)
- Test: `internal/tui/served_test.go`

**Interfaces:**
- Consumes: `live.Host` (`InputReader`, `Output`, `OnSize`, `OnClients`, `DetachHolder`, `Clients`, `AnyASCII`).
- Produces: `func (m *Model) RunServed(ctx context.Context, h *live.Host) error`; `type clientsMsg []live.ClientInfo`; `type idleTickMsg time.Time`; `Model.host *live.Host`; `Model.ascii bool`; config `LiveIdleLimit int` (minutes, `json:"live_idle_limit"`, default 0 = never).

- [ ] **Step 1: Write the failing tests** (`internal/tui/served_test.go`):

```go
package tui

import (
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/live"
)

// With two clients the bottom line names the holder and the chords.
func TestClientsMsgRendersHolder(t *testing.T) {
	m := newTestModel(t)
	m.Update(clientsMsg{{ID: 1, Label: "vscode (pid 1)"}, {ID: 2, Label: "ssh from 10.0.0.5 (pid 2)", Holder: true}})
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

// With live_idle_limit set, a served session with no clients and no run
// in progress quits once the limit has passed; a client or a run resets it.
func TestIdleLimitQuitsWhenUnattachedAndIdle(t *testing.T) {
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
```

(Add `fmt`, `time` and `tea "github.com/charmbracelet/bubbletea"` to the test imports.)

- [ ] **Step 2: Run to verify failure** — `go test ./internal/tui -run 'TestClients'`; expected undefined `clientsMsg`, `detachHolder`.

- [ ] **Step 3: Implement**

In `internal/tui/tui.go`:

```go
// clientsMsg carries the attached-terminal list from the session host.
type clientsMsg []live.ClientInfo
```

Model fields:

```go
	host         *live.Host
	served       bool
	clients      []live.ClientInfo
	detachHolder func()    // host.DetachHolder when served; nil in-process
	ascii        bool      // some attached client cannot show UTF-8 glyphs
	idleSince    time.Time // last moment the session had a client or a run
```

```go
// idleTickMsg drives the live_idle_limit check in served mode.
type idleTickMsg time.Time

func idleTick() tea.Cmd { return tea.Tick(30*time.Second, func(t time.Time) tea.Msg { return idleTickMsg(t) }) }
```

`RunServed`:

```go
// RunServed runs the program over a session host instead of a terminal:
// keystrokes come from the host's input pipe, frames go to every attached
// client, and sizes arrive as WindowSizeMsg from the host.
func (m *Model) RunServed(ctx context.Context, h *live.Host) error {
	m.rootCtx = ctx
	m.host = h
	m.served = true
	m.idleSince = time.Now()
	m.detachHolder = h.DetachHolder
	m.termWrite = func(s string) { io.WriteString(h.Output(), s) }
	m.clipboardWrite = func(s string) error { io.WriteString(h.Output(), osc52(s)); return writeClipboardTools(s) }
	if m.cfg.ThemeTerminalColors {
		m.termWrite(terminalColorSeq(m.cfg.Theme))
		defer m.termWrite(terminalColorReset())
	}
	p := tea.NewProgram(m, tea.WithInput(h.InputReader()), tea.WithOutput(h.Output()),
		tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithoutSignalHandler())
	m.program = p
	h.OnSize(func(cols, rows int) { p.Send(tea.WindowSizeMsg{Width: cols, Height: rows}) })
	h.OnClients(func(cl []live.ClientInfo) { p.Send(clientsMsg(cl)) })
	h.OnQuit(func() { p.Send(tea.Quit()) })
	if c, r := h.Size(); c > 0 {
		go p.Send(tea.WindowSizeMsg{Width: c, Height: r})
	}
	if m.cfg.LiveIdleLimit > 0 {
		go p.Send(idleTickMsg(time.Now()))
	}
	_, err := p.Run()
	m.histFile.save()
	return err
}
```

In `clipboard.go`, split `writeClipboard` into `writeClipboard` (OSC 52 to stdout + tools, as today) and `writeClipboardTools(s string) error` (the system-tool loop only).

`Update`:

```go
	case clientsMsg:
		prev := m.clients
		m.clients = []live.ClientInfo(msg)
		m.ascii = false
		for _, c := range m.clients {
			if !c.UTF8 {
				m.ascii = true
			}
		}
		for _, c := range m.clients {
			if !hasClient(prev, c.ID) {
				note := "attached: " + c.Label
				if c.Holder {
					note += ", now holding input"
				}
				m.appendLine(stDim.Render(note))
			}
		}
		for _, c := range prev {
			if !hasClient(m.clients, c.ID) {
				m.appendLine(stDim.Render("detached: " + c.Label))
			}
		}
```

with `func hasClient(list []live.ClientInfo, id int) bool`. Then:

```go
	case idleTickMsg:
		if !m.served || m.cfg.LiveIdleLimit <= 0 {
			return m, nil
		}
		if len(m.clients) > 0 || m.running {
			m.idleSince = time.Time(msg)
		} else if time.Time(msg).Sub(m.idleSince) >= time.Duration(m.cfg.LiveIdleLimit)*time.Minute {
			return m, tea.Quit
		}
		return m, idleTick()
```

`config.go`: `LiveIdleLimit int \`json:"live_idle_limit"\`` with the comment "minutes a served session may sit with no clients and no run before it exits (0 = never)"; default 0.

`bottomLine` (after the IDE marker):

```go
	if len(m.clients) > 1 {
		holder := "?"
		for _, c := range m.clients {
			if c.Holder {
				holder = c.Label
			}
		}
		line += stAccent.Render(fmt.Sprintf(" ⧉ %d", len(m.clients))) +
			stDim.Render(" · input: "+holder+" · Ctrl+] d detach · Ctrl+] t take over")
	}
```

`slashCommand`:

```go
	case "/clients":
		if len(m.clients) == 0 {
			m.appendLine(stDim.Render("not served: this session is running in-process (start without --no-host to allow attach)"))
			return m, nil
		}
		for _, c := range m.clients {
			mark := "  "
			if c.Holder {
				mark = "> "
			}
			m.appendLine(stDim.Render(fmt.Sprintf("%s%s  %dx%d", mark, c.Label, c.Cols, c.Rows)))
		}
		return m, nil
	case "/detach":
		if m.detachHolder == nil {
			m.appendLine(stDim.Render("nothing to detach: not served"))
			return m, nil
		}
		m.detachHolder()
		return m, nil
```

`ui.SlashCommandTable` gains `{"/clients", "list terminals attached to this session", false}` and `{"/detach", "detach the terminal that holds input (the session keeps running)", false}`; `menuEntries` gains `{"Sessions", "Attached terminals", "who is viewing this session", cmd("/clients")}` and `{"Sessions", "Detach this terminal", "session keeps running; be-code attach <code> to return", cmd("/detach")}`.

- [ ] **Step 4: Run** `go test ./internal/tui ./internal/ui`; expected PASS.
- [ ] **Step 5: Verify and commit** — `make -f build.mk verify`; commit `tui: served mode over a live session host; /clients and /detach`.

---

### Task 5: Compact layout

**Files:**
- Modify: `internal/config/config.go` (`Layout string`, default `"auto"`), `internal/tui/tui.go` (layout, header, prompt, wheel, bottom line, modals), `internal/tui/palette.go` (popups, menu), `internal/tui/queue.go`
- Test: `internal/tui/compact_test.go`

**Interfaces (produced):** `func (m *Model) compact() bool` (config `layout` and the current size); `Model.ascii` used for glyph fallbacks.

- [ ] **Step 1: Write the failing tests**

```go
package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestCompactEngagesOnSmallSizes(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if m.compact() {
		t.Fatal("compact at 100x30")
	}
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 18})
	if !m.compact() {
		t.Fatal("not compact at 60x18")
	}
	v := m.View()
	if strings.Contains(v, "BE-Code Redux") || strings.Contains(v, "BE AI Research") {
		t.Fatal("header/attribution shown in compact")
	}
	if !strings.Contains(v, "\n>") && !strings.HasPrefix(v, ">") {
		t.Fatalf("prompt not shortened:\n%s", v)
	}
	if strings.Contains(v, "(>):") {
		t.Fatal("full prompt in compact")
	}
	m.cfg.Layout = "full"
	if m.compact() {
		t.Fatal("layout=full must disable compact")
	}
	m.cfg.Layout = "compact"
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if !m.compact() {
		t.Fatal("layout=compact must force compact")
	}
}

func TestCompactBottomLineAndPopups(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 56, Height: 18})
	m.ag.IDEName = "vscode"
	m.mode = modeBusy
	m.running = true
	m.ag.Enqueue("one")
	m.ag.Enqueue("two")
	v := m.View()
	for _, want := range []string{"/menu", "q2", "⌘"} {
		if !strings.Contains(v, want) {
			t.Fatalf("compact bottom line lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "Enter queues") {
		t.Fatal("long hint shown in compact")
	}
	m.mode = modeInput
	m.running = false
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	v = m.View()
	if strings.Contains(v, "command reference") {
		t.Fatal("palette descriptions shown under 60 columns")
	}
}

func TestASCIIFallbacks(t *testing.T) {
	m := newTestModel(t)
	m.ascii = true
	m.ag.IDEName = "vscode"
	v := m.View()
	if strings.Contains(v, "◑") || strings.Contains(v, "⌘") {
		t.Fatalf("non-ASCII glyphs with an ASCII client:\n%s", v)
	}
	if !strings.Contains(v, "IDE") {
		t.Fatal("ASCII IDE marker missing")
	}
}
```

- [ ] **Step 2: Run to verify failure** — expected undefined `compact`, `Layout`.

- [ ] **Step 3: Implement**

`config.go`: `Layout string \`json:"layout"\`` with comment "auto | compact | full"; default `"auto"`.

`tui.go`:

```go
const (
	compactCols = 70
	compactRows = 20
)

// compact reports whether the reduced layout is in force: forced by
// config, or automatic when the (shared) size is small.
func (m *Model) compact() bool {
	switch strings.ToLower(m.cfg.Layout) {
	case "compact":
		return true
	case "full":
		return false
	}
	return m.width < compactCols || m.height < compactRows
}
```

- `showHeader()` returns false when compact.
- Prompt: `SetPromptFunc` returns `"> "` (width 2) when compact — since the prompt function is set once, compute it dynamically: `ta.SetPromptFunc(5, func(i int) string { if m.compact() { if i == 0 { return "> " }; return "  " } ... })` and call `m.input.SetWidth` in `layout()` with `wheelWidth` reduced to 5 in compact (glyph + `NN%`).
- `wheelView`: when `m.ascii`, use `o . o O *` for fill and `| / - \` for spin; format `"%s %d%%"` in compact.
- `bottomLine`: when compact, `" /menu · <shortModel> · <state>"` where state is `ready` / spinner + first word of statusNote; queue count `qN` (the `⧉` glyph is reserved for the clients marker), IDE marker `⌘` (ASCII `IDE`); no hint text. The clients marker from Task 4 stays as `⧉ N` but drops the holder label and chord hints in compact.
- Popups (`paletteBox`, `queueBox`, `contextMenuBox`): when `m.width < 60` omit descriptions; when compact cap rows at 6.
- `viewMenu`: when compact, status block is two lines (`model · profile` and `context N% · session total`), entries label-only (`renderPickList` gets an `omitDesc` flag).
- Approval and plan modals: when compact, use the full height (`modalHeight` returns `m.height-3`), wrap the diff at width, hint line reduced to `y/n/a · ↑↓`.
- Tool-call preview lines (`toolStartMsg` rendering): truncate the args preview to `m.width-12` in compact.

- [ ] **Step 4: Run** `go test ./internal/tui`; expected PASS (existing tests at 80×24 are above both thresholds, so they stay non-compact; if any existing test used a size under the thresholds, adjust that test's size rather than the thresholds).
- [ ] **Step 5: Verify and commit** — `make -f build.mk verify`; commit `tui: compact layout for small shared sizes; ASCII fallbacks`.

---

### Task 6: Launcher, host command, attach, sessions, docs

**Files:**
- Create: `internal/live/spawn_unix.go`, `internal/live/spawn_windows.go`, `cmd/live.go`
- Modify: `cmd/root.go` (flags, `runInteractive`, `finishSession` writer type), `cmd/commands.go` (`sessions` LIVE column, `sessions kill`), `internal/config/config.go` (`HostSessions bool` default true), `build.mk` (VERSION), `README.md`, `CHANGELOG.md`, root `CLAUDE.md`, `docs/live-checklist.md`
- Test: `internal/live/spawn_test.go`

**Interfaces (produced):**

```go
// live
func SpawnHost(exe, arg, logPath string, env []string, extraArgs ...string) (pid int, err error) // detached: Setsid / DETACHED_PROCESS; stdio → logPath
func WaitForSocket(path string, timeout time.Duration) error
func NewToken() string
// cmd
var flagNoHost, flagView bool
func runSessionHost(code string) error          // --session-host <code> (hidden flag)
func attachCmd                                   // be-code attach <code|last> [--view]
```

- [ ] **Step 1: Write the failing test** (`internal/live/spawn_test.go`):

```go
package live

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSpawnHostDetachesAndWaitForSocket(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "x.sock")
	// A stand-in "host": sleep long enough for the check, in its own session.
	pid, err := SpawnHost("/bin/sh", "-c", filepath.Join(dir, "log"), []string{"BE_TEST=1"}, "sleep 2")
	if err != nil {
		t.Skip("cannot spawn detached process here: " + err.Error())
	}
	if !processAlive(pid) {
		t.Fatal("spawned process not alive")
	}
	go func() { time.Sleep(100 * time.Millisecond); os.WriteFile(sock, nil, 0o600) }()
	if err := WaitForSocket(sock, time.Second); err != nil {
		t.Fatal(err)
	}
	if err := WaitForSocket(filepath.Join(dir, "never.sock"), 150*time.Millisecond); err == nil {
		t.Fatal("expected timeout")
	}
}
```

The test spawns `/bin/sh -c "sleep 2"` through the same function production uses with `be-code --session-host <code> …`. (Windows: the test skips when `/bin/sh` is absent.)

- [ ] **Step 2: Run to verify failure** — undefined `SpawnHost`, `WaitForSocket`.

- [ ] **Step 3: Implement**

`internal/live/spawn_unix.go`:

```go
//go:build !windows

package live

import (
	"os"
	"os/exec"
	"syscall"
)

func SpawnHost(exe, arg, logPath string, env []string, extraArgs ...string) (int, error) {
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	cmd := exec.Command(exe, append([]string{arg}, extraArgs...)...)
	cmd.Env = env
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	go cmd.Wait() // reap; the host outlives us
	return pid, nil
}
```

`internal/live/spawn_windows.go`: same with `cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | 0x00000008 /* DETACHED_PROCESS */}`.

Shared (`internal/live/spawn.go`):

```go
func WaitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("live: host did not start (no socket at %s within %s)", path, timeout)
}

func NewToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}
```

`cmd/live.go`:

```go
package cmd

// runSessionHost is the detached process behind a served TUI session.
func runSessionHost(code string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	dir, err := live.Dir()
	if err != nil {
		return err
	}
	rec, err := live.Load(dir, code) // written by the launcher before spawning
	if err != nil {
		return err
	}
	rec.PID = os.Getpid()
	if err := rec.Save(dir); err != nil {
		return err
	}
	p, ag, err := buildAgent(cfg, false) // honours --resume, -C, --provider, --model forwarded by the launcher
	if err != nil {
		return err
	}
	if flagResume == "" {
		ag.Session.Code = rec.Code // the advertised attach code doubles as the resume code
	}
	h := live.NewHost(rec.Token, os.Stderr)
	if err := h.Listen(rec.Socket); err != nil {
		return err
	}
	go h.Serve()
	defer live.Remove(dir, code)
	defer ag.Tools.Close()
	defer ag.Checkpoints.Cleanup()
	defer finishSession(ag, true, h.Output())
	if ideSession != nil {
		ag.Tools.ReviewWrite = ideSession.ReviewWrite
		defer ideSession.Close()
	}
	m := tui.New(cfg, ag, p)
	err = m.RunServed(context.Background(), h)
	h.Close("session ended")
	return err
}
```

Session code: with a fresh session the host overwrites the store session's `Code` with the advertised code, so the resume line printed by `finishSession` is the same code used for `attach`. When the launcher was started with `--resume <x>`, that flag is forwarded and the host resumes `x`; the live record's code is then `x`'s resume code (the launcher reads it with `store.Load(flagResume)` before spawning). Change `finishSession(ag *agent.Agent, withModel bool, out io.Writer)` (it only uses `Fprintf`); it is called with `h.Output()` in the host so every client sees the resume line.

Launcher, in `runInteractive`: decide before building anything, because the launcher process must not own a session it does not host (no `finishSession`, no tool registry, no MCP servers):

```go
func runInteractive(ctx context.Context) error {
	cfg, err := loadOrWizard(ctx)
	if err != nil {
		return err
	}
	if !usePlainUI(cfg) && !flagNoHost && cfg.HostSessions { // plain / non-TTY runs stay in-process
		return launchServed(ctx, cfg)
	}
	p, ag, err := buildAgent(cfg, false)
	… (unchanged in-process path)
}
```

```go
// launchServed starts a detached host for this session and attaches to it.
func launchServed(ctx context.Context, cfg *config.Config) error {
	dir, err := live.Dir()
	if err != nil {
		return err
	}
	workspace, err := filepath.Abs(flagDir)
	if err != nil {
		return err
	}
	// Offer to attach to a live session for this workspace instead.
	if lives, _ := live.List(dir); len(lives) > 0 {
		for _, r := range lives {
			if r.Workspace == workspace {
				fmt.Printf("a live session for this workspace is running (%s, since %s). Attach to it? [Y/n] ", r.Code, r.StartedAt.Format("15:04"))
				var ans string
				fmt.Scanln(&ans)
				if ans == "" || strings.HasPrefix(strings.ToLower(ans), "y") {
					return attachLive(ctx, &r, false)
				}
			}
		}
	}
	code := store.CodeFor(live.NewToken()) // fresh; the host stamps it on its session
	if flagResume != "" {
		s, err := store.Load(flagResume)
		if err != nil {
			return err
		}
		code = s.ResumeCode()
	}
	if _, err := live.Load(dir, code); err == nil {
		return fmt.Errorf("session %s is already live; use: be-code attach %s", code, code)
	}
	model := provider.ResolveModel(cfg, flagProvider, flagModel)
	rec := live.Record{Code: code, Socket: live.SocketPath(dir, code), Workspace: workspace, Model: model, StartedAt: time.Now(), Token: live.NewToken()}
	if err := rec.Save(dir); err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{code, "-C", workspace}
	if flagResume != "" {
		args = append(args, "--resume", flagResume)
	}
	if flagProvider != "" {
		args = append(args, "--provider", flagProvider)
	}
	if flagModel != "" {
		args = append(args, "--model", flagModel)
	}
	if _, err := live.SpawnHost(self, "--session-host", filepath.Join(dir, code+".log"), os.Environ(), args...); err != nil {
		live.Remove(dir, code)
		return fmt.Errorf("could not start the session host: %w (use --no-host to run in-process)", err)
	}
	if err := live.WaitForSocket(rec.Socket, 10*time.Second); err != nil {
		return err
	}
	return attachLive(ctx, &rec, false)
}

func attachLive(ctx context.Context, rec *live.Record, view bool) error {
	opt := live.DefaultAttachOptions()
	opt.View = view
	reason, err := live.Attach(ctx, rec, opt)
	if err != nil {
		return err
	}
	if reason == "" {
		fmt.Printf("detached from %s (still running); be-code attach %s to return\n", rec.Code, rec.Code)
	} else {
		fmt.Printf("%s: %s\n", rec.Code, reason)
	}
	return nil
}
```

(Check the exact spellings of the provider/model flags in `root.go` before forwarding them; `flagProvider` and `flagModel` are the Go names.) A host started this way inherits the launcher's environment, so `OLLAMA_HOST`, API-key env vars and `TERM_PROGRAM` reach it; the host's own stdio is the log file at `~/.be-code/live/<code>.log`.

Flags in `root.go`: `--no-host` (persistent bool), hidden `--session-host <code>` (string; when set, `RunE` calls `runSessionHost`). `attachCmd` (`be-code attach <code|last> [--view]`): resolve `last` to the newest live record; if no live record, fall back to `flagResume = code; return runInteractive(ctx)` with a printed note `no live session <code>; resuming the saved one`. `sessionsCmd`: add a `LIVE` column showing `live` or `-` from `live.List` (client counts are not advertised in the record; `/clients` inside the session shows them); `sessions kill <code>`: dial, send `hello` + `quit`, wait up to 10 s for the record to disappear, else `syscall.Kill(pid, SIGTERM)` (unix) / `os.FindProcess(pid).Kill()` (windows) and `live.Remove`. Both `attach` and `kill` reject unknown codes with `no live session <code>`.

Config: `HostSessions bool \`json:"host_sessions"\`` default true (`LiveIdleLimit` was added in Task 4).

Docs: README section "Live sessions and handoff" (start, `⧉` marker, `be-code attach <code>`, `ssh host be-code attach <code>`, chords, `--view`, `sessions kill`, `--no-host`, `host_sessions`, `live_idle_limit`, `layout`, the phone/compact note and the "everyone runs at the smallest size" trade-off); CHANGELOG `## v0.5.0 — <date> — live sessions & handoff`; `build.mk` VERSION 0.5.0; root `CLAUDE.md`: a "Live sessions" paragraph (`internal/live`, host/attach, served TUI, compact layout, deviation about identical frames). `docs/live-checklist.md`:

```
1. In VS Code's terminal: be-code. Expect the TUI as before; /clients shows one client.
2. Second terminal: be-code sessions → LIVE column shows the code; be-code attach <code>. Expect the same screen, "attached: … now holding input" in both, ⧉ 2 in the bottom line.
3. Type in the second terminal: it drives the session; the first is a viewer. Ctrl+] t in the first takes input back.
4. Resize the smaller terminal: both views relayout to the smaller size.
5. Ctrl+] d in the second: "detached … still running". Close the VS Code window entirely; from another terminal: be-code attach <code> — the session is still there.
6. ssh localhost be-code attach <code> — same as 2 over SSH.
7. From a phone SSH app: attach; expect the compact layout (no header, short bottom line, > prompt).
8. /quit from any client: every client prints the resume line and returns to its shell; be-code sessions no longer lists it live.
9. be-code --no-host: runs in-process; /clients says not served.
```

- [ ] **Step 4: Run** `make -f build.mk verify`, `sh test/e2e/run_e2e.sh` (headless is unchanged), then a manual smoke on this machine: `be-code --session-host` path via `be-code` in one terminal and `be-code attach <code>` in another (steps 1-3 and 8 of the checklist).
- [ ] **Step 5: Commit** — `launcher, --session-host, attach, sessions kill, docs (0.5.0)`.
