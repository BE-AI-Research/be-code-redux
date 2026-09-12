package live

import (
	"context"
	"encoding/json"
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

// ReasonEnded is Host.Close's reason when the served program has finished
// (see cmd/live.go). It is the one bye reason that means the program already
// left the alt screen and printed its closing lines — the resume code — on
// the normal screen, so Attach must not clear on its way out.
const ReasonEnded = "session ended"

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

// Attach connects to a live session and runs until detach, host exit, or
// ctx cancel. Returns the host's bye reason ("" on a local detach).
func Attach(ctx context.Context, rec *Record, opt AttachOptions) (string, error) {
	conn, err := net.Dial("unix", rec.Socket)
	if err != nil {
		return "", fmt.Errorf("live: %w", err)
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
	io.WriteString(opt.Stdout, clearScreen)

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
			case FOutput:
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
	go func() {
		defer close(stdinDone)
		var chord Chord
		buf := make([]byte, 4096)
		for {
			n, err := opt.Stdin.Read(buf)
			// A Reader may return n > 0 alongside a non-nil err (the last
			// read of a stream, per the io.Reader contract); process those
			// bytes before acting on the error so a detach chord or the
			// final keystrokes right before EOF are never dropped.
			if n > 0 {
				fwd, act := chord.Feed(buf[:n], time.Now())
				// Forward any plain bytes the same Read delivered ahead of
				// the chord (e.g. pasted text ending in Ctrl+] d) before
				// acting on act, so they reach the program instead of
				// being dropped by an early return below.
				if len(fwd) > 0 && !opt.View {
					if werr := writeFrame(FInput, fwd); werr != nil {
						select {
						case result <- "connection closed":
						default:
						}
						return
					}
				}
				switch act {
				case ActionDetach:
					localDetach.Store(true)
					writeFrame(FDetach, nil)
					select {
					case result <- "":
					default:
					}
					return
				case ActionTakeover:
					writeFrame(FTakeover, nil)
				}
			}
			if err != nil {
				select {
				case result <- "":
				default:
				}
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
	// terminal is left holding a frozen alt-screen rendering: clear it so the
	// shell prompt lands on a clean screen. A session that ended normally is
	// the exception — its program restored the normal screen itself and then
	// printed the resume line, which a clear here would wipe.
	if reason != ReasonEnded {
		io.WriteString(opt.Stdout, clearScreen)
	}
	return reason, retErr
}
