package live

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/x/term"
)

// AttachOptions configures Attach. Raw puts the local terminal in raw mode
// and returns a restore func; Size reports the current terminal size; Stdin
// and Stdout let tests substitute in-memory pipes for the real terminal.
type AttachOptions struct {
	Label  string
	View   bool // attach as a viewer: input is never sent
	Stdin  io.Reader
	Stdout io.Writer
	Raw    func() (restore func(), err error)
	Size   func() (cols, rows int)
	UTF8   bool
	// Joined, when non-nil, is set the moment the host sends this terminal
	// its first frame of rendered output — the proof that it actually
	// joined a running session rather than arriving at one already on its
	// way out. A host in its shutdown window still advertises its record
	// and still answers on its socket, so a launcher joining at that
	// instant can be accepted and told "session ended" without ever seeing
	// the session; cmd's join paths read this to tell that apart from an
	// ordinary end and start a fresh host instead (see cmd.joinMissed).
	Joined *atomic.Bool
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

// DefaultAttachOptions wires Attach to the real controlling terminal: raw
// mode via x/term, os.Stdin/os.Stdout, and polled size.
func DefaultAttachOptions() AttachOptions {
	fd := int(os.Stdin.Fd())
	return AttachOptions{
		Label:  ClientLabel(),
		Stdin:  stdinPump,
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

// The terminal modes a client owns for the duration of an attach. Bubble Tea
// emits its own mode sequences once, at program start, into the host's output
// — before any client exists — so nothing the program does can put *this*
// terminal into the alt screen, turn mouse reporting on, or enable bracketed
// paste. The client has to do it, and undo it on the way out, or the session
// renders into the main buffer with a visible cursor, no mouse selection and
// no multi-line paste (each pasted line submitting as its own message).
//
// enterModes: alt screen (1049h), hide cursor (25l), mouse cell-motion
// reporting with SGR extended coordinates (1002h/1006h — what the served
// program is run with, see tui.RunServed), bracketed paste (2004h).
// exitModes is the exact reverse, innermost last.
const (
	enterModes = "\x1b[?1049h\x1b[?25l\x1b[?1002h\x1b[?1006h\x1b[?2004h"
	exitModes  = "\x1b[?2004l\x1b[?1006l\x1b[?1002l\x1b[?25h\x1b[?1049l"
	// exitModesEnded is exitModes without the alt-screen exit, for the one
	// case where the host has already taken this terminal back to the normal
	// screen and printed the resume line there (see ReasonEnded): a second
	// 1049l would restore the cursor to its attach-time position and let the
	// shell prompt overwrite exactly the line the user needs to read.
	exitModesEnded = "\x1b[?2004l\x1b[?1006l\x1b[?1002l\x1b[?25h"
)

// ExitAltScreen takes every attached terminal back to the normal screen with
// a visible cursor. The host writes it to its fan-out output before the
// session's closing lines (see cmd/live.go), so those land on the normal
// screen and survive the clients' own restore — the resume code is the one
// thing that must still be on screen after every terminal has let go.
const ExitAltScreen = "\x1b[?1049l\x1b[?25h"

// ReasonDetached is the host's bye reason when it detached a client rather
// than the client detaching itself: `/detach` typed in that terminal, or
// `Detach` called for it from inside the session. The terminal is going
// back to its shell with the session still running, so the caller reports
// it the same way as a local Ctrl+] d rather than as a session that stopped
// (see cmd.attachLive).
const ReasonDetached = "detached"

// ReasonEnded is Host.Close's reason when the served program has finished
// (see cmd/live.go). It is the one bye reason that means the program already
// left the alt screen and printed its closing lines — the resume code — on
// the normal screen, so Attach must not clear (or re-exit the alt screen) on
// its way out.
const ReasonEnded = "session ended"

// ErrDial wraps every failure to reach a host's socket at all, so a caller
// can tell a host that is simply not there from a session that refused,
// ended or broke mid-attach.
var ErrDial = errors.New("cannot reach the session host")

// SwitchTarget extracts the session code from a "switch:CODE" bye reason.
func SwitchTarget(reason string) (string, bool) {
	if strings.HasPrefix(reason, ReasonSwitchPrefix) && len(reason) > len(ReasonSwitchPrefix) {
		return reason[len(ReasonSwitchPrefix):], true
	}
	return "", false
}

// deadlineReader is implemented by *os.File (a real terminal, since Go
// 1.23) but not by the in-memory pipes the tests use. When available,
// Attach uses it to interrupt a stdin goroutine parked in Read once the
// session ends, so that goroutine does not outlive Attach and steal a
// keystroke from whatever reads stdin next. The deadline is always reset
// to the zero value before Attach returns: a non-zero deadline persists
// across calls, so leaving it set would make every subsequent read of the
// same file (a reattach, a fallback prompt) fail instantly with an i/o
// timeout.
type deadlineReader interface {
	SetReadDeadline(t time.Time) error
}

// ChunkReader is an input source that hands out whole reads on a channel
// and can be abandoned without a blocked Read outliving the caller. Attach
// prefers it over plain Read: a blocking terminal read cannot be
// interrupted (an inherited os.Stdin is not in Go's poller, so its
// SetReadDeadline is a no-op), which means the stdin goroutine of a
// previous Attach — a picker switch reattaches in the same terminal —
// would otherwise stay parked in Read, swallow the first keystrokes typed
// after the switch, and die on its already-closed connection.
type ChunkReader interface {
	Chunks() <-chan []byte
}

// StdinPump reads one terminal for the life of the process and serves its
// bytes to successive Attach calls through Chunks. The channel is closed
// when the underlying reader ends.
type StdinPump struct {
	src     io.Reader
	ch      chan []byte
	once    sync.Once
	mu      sync.Mutex // guards pending (Read path only)
	pending []byte
}

func NewStdinPump(r io.Reader) *StdinPump {
	return &StdinPump{src: r, ch: make(chan []byte, 64)}
}

func (p *StdinPump) Chunks() <-chan []byte {
	p.once.Do(func() {
		go func() {
			defer close(p.ch)
			buf := make([]byte, 4096)
			for {
				n, err := p.src.Read(buf)
				if n > 0 {
					p.ch <- append([]byte(nil), buf[:n]...)
				}
				if err != nil {
					return
				}
			}
		}()
	})
	return p.ch
}

// Read satisfies io.Reader for callers that do not use Chunks: it drains
// the channel one chunk at a time, keeping any remainder for the next call.
func (p *StdinPump) Read(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pending) == 0 {
		chunk, ok := <-p.Chunks()
		if !ok {
			return 0, io.EOF
		}
		p.pending = chunk
	}
	n := copy(b, p.pending)
	p.pending = p.pending[n:]
	return n, nil
}

// stdinPump is the process-wide reader DefaultAttachOptions hands out, so
// every Attach in this process shares one terminal reader.
var stdinPump = NewStdinPump(os.Stdin)

// Attach connects to a live session and runs until detach, host exit, or
// ctx cancel. Returns the host's bye reason ("" on a local detach).
func Attach(ctx context.Context, rec *Record, opt AttachOptions) (string, error) {
	conn, err := net.Dial("unix", rec.Socket)
	if err != nil {
		// ErrDial as well as the underlying error: a caller that is joining
		// rather than attaching on purpose needs to tell "the host's socket
		// is gone" from every other failure (see AttachOptions.Joined).
		return "", fmt.Errorf("live: %w: %w", ErrDial, err)
	}
	defer conn.Close()

	// conn is written to by two independent goroutines below (stdin
	// forwarding and size polling); a net.Conn's Write is safe to call
	// concurrently, but WriteFrame issues two separate Write calls (header,
	// then payload) and an interleaving of those from two goroutines would
	// corrupt the frame stream. Serialize every write through this mutex.
	var connMu sync.Mutex
	// localDetach is set (before FDetach is written) when the user's own
	// Ctrl+] d/Ctrl+] chord asks to detach. Sending FDetach makes the host
	// detach this client and reply with its own FBye (reason "detached"),
	// which can race the stdin goroutine's own "" result: whichever gets
	// pushed into result first would otherwise decide Attach's return
	// value. A user-initiated detach must deterministically return "" (the
	// documented contract), so the flag overrides whatever result carries.
	var localDetach atomic.Bool
	writeFrame := func(t FrameType, payload []byte) error {
		connMu.Lock()
		defer connMu.Unlock()
		return WriteFrame(conn, t, payload)
	}
	writeJSON := func(t FrameType, v any) error {
		connMu.Lock()
		defer connMu.Unlock()
		return WriteJSON(conn, t, v)
	}

	cols, rows := opt.Size()
	if err := writeJSON(FHello, Hello{Token: rec.Token, Cols: cols, Rows: rows, Label: opt.Label, UTF8: opt.UTF8}); err != nil {
		return "", err
	}
	restore, err := opt.Raw()
	if err != nil {
		return "", err
	}
	defer restore()
	// Deferred after restore() (so it runs before it, defers being LIFO) and
	// driven by a variable rather than written inline at the end, so that
	// every exit path — including one added later — leaves this terminal's
	// modes as it found them.
	ended := false
	defer func() {
		if ended {
			io.WriteString(opt.Stdout, exitModesEnded)
			return
		}
		io.WriteString(opt.Stdout, exitModes)
	}()
	io.WriteString(opt.Stdout, enterModes+clearScreen)

	result := make(chan string, 2)
	stdinDone := make(chan struct{})
	readerDone := make(chan struct{})
	// done is closed the moment Attach is about to return, for any reason
	// (host bye, local detach, or ctx cancel). The size-polling goroutine
	// below only has a ticker to drive it, so without this it would run
	// forever past Attach's return whenever the exit came from the result
	// channel rather than ctx cancellation - a plain goroutine leak.
	done := make(chan struct{})
	defer close(done)

	// host → stdout
	go func() {
		defer close(readerDone)
		for {
			typ, p, err := ReadFrame(conn)
			if err != nil {
				select {
				case result <- "connection closed":
				default:
				}
				return
			}
			switch typ {
			case FOutput, FOverlay:
				if typ == FOutput && opt.Joined != nil {
					opt.Joined.Store(true)
				}
				opt.Stdout.Write(p)
			case FSize:
				io.WriteString(opt.Stdout, clearScreen)
			case FBye:
				var b Bye
				json.Unmarshal(p, &b)
				select {
				case result <- b.Reason:
				default:
				}
				return
			}
		}
	}()

	// stdin → host, with chords
	stopStdin := make(chan struct{})
	go func() {
		defer close(stdinDone)
		var chord Chord
		// handle forwards one read's bytes and reports whether the goroutine
		// must stop (detach chord, or the connection is gone).
		handle := func(b []byte) bool {
			fwd, act := chord.Feed(b, time.Now())
			// Forward any plain bytes the same read delivered ahead of the
			// chord (e.g. pasted text ending in Ctrl+] d) before acting on
			// act, so they reach the program instead of being dropped.
			if len(fwd) > 0 && !opt.View {
				if werr := writeFrame(FInput, fwd); werr != nil {
					select {
					case result <- "connection closed":
					default:
					}
					return true
				}
			}
			if act == ActionDetach {
				localDetach.Store(true)
				writeFrame(FDetach, nil)
				select {
				case result <- "":
				default:
				}
				return true
			}
			return false
		}
		eof := func() {
			select {
			case result <- "":
			default:
			}
		}
		if cr, ok := opt.Stdin.(ChunkReader); ok {
			// A shared reader: consume until told to stop, so no goroutine
			// from this Attach can outlive it and steal keys from the next.
			chunks := cr.Chunks()
			for {
				select {
				case <-stopStdin:
					return
				case b, ok := <-chunks:
					if !ok {
						eof()
						return
					}
					if handle(b) {
						return
					}
				}
			}
		}
		buf := make([]byte, 4096)
		for {
			n, err := opt.Stdin.Read(buf)
			// A Reader may return n > 0 alongside a non-nil err (the last
			// read of a stream, per the io.Reader contract); process those
			// bytes before acting on the error so a detach chord or the
			// final keystrokes right before EOF are never dropped.
			if n > 0 && handle(buf[:n]) {
				return
			}
			if err != nil {
				eof()
				return
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
			case <-done:
				return
			case <-t.C:
				c, r := opt.Size()
				if c != lc || r != lr {
					lc, lr = c, r
					writeJSON(FResize, Size{Cols: c, Rows: r})
				}
			}
		}
	}()

	var reason string
	var retErr error
	select {
	case reason = <-result:
	case <-ctx.Done():
		writeFrame(FDetach, nil)
		retErr = ctx.Err()
	}
	// A user-initiated detach must return "" even if the host's own bye
	// (sent in reply to our FDetach, reason "detached") won the race into
	// result ahead of the stdin goroutine's own "" — see localDetach above.
	if localDetach.Load() {
		reason = ""
	}

	// Force both background goroutines off the connection and stdin before
	// writing the final clear, so neither can still be mid-write when we
	// do (or race each other): closing conn makes a blocked ReadFrame in
	// the host→stdout goroutine error out deterministically instead of
	// possibly still writing to opt.Stdout when we return, and setting a
	// read deadline interrupts a stdin goroutine parked in Read (real
	// terminals only - see deadlineReader). Both waits are bounded: an
	// io.Reader without deadline support (e.g. an io.PipeReader with no
	// pending write, as in tests) can't be interrupted and is left to exit
	// on its own later.
	conn.Close()
	close(stopStdin)
	if d, ok := opt.Stdin.(deadlineReader); ok {
		d.SetReadDeadline(time.Now())
	}
	wait := func(ch <-chan struct{}) {
		select {
		case <-ch:
		case <-time.After(200 * time.Millisecond):
		}
	}
	wait(readerDone)
	wait(stdinDone)
	// Reset the deadline: leaving it set would make every subsequent read
	// of this same file (a reattach, a fallback prompt) fail instantly
	// with an i/o timeout, long after this Attach call has returned.
	if d, ok := opt.Stdin.(deadlineReader); ok {
		d.SetReadDeadline(time.Time{})
	}

	// The session is still running (a detach) or gone unexpectedly, so this
	// terminal is left holding a frozen alt-screen rendering: clear it so a
	// terminal without alt-screen support does not keep it, before the
	// deferred exitModes leaves the alt screen on one that does. A session
	// that ended normally is the exception — the host already took this
	// terminal back to the normal screen and printed the resume line there,
	// which a clear (or a second alt-screen exit) would wipe.
	// A session that ended without ever rendering a frame here never sent
	// this terminal the alt-screen exit that makes ReasonEnded special (it
	// wrote it before this attach existed), so this is an ordinary exit: the
	// alt screen has to be left the normal way or the shell comes back
	// inside it.
	ended = reason == ReasonEnded && (opt.Joined == nil || opt.Joined.Load())
	if !ended {
		io.WriteString(opt.Stdout, clearScreen)
	}
	return reason, retErr
}
