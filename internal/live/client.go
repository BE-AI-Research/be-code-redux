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

// deadlineReader is implemented by *os.File (a real terminal, since Go
// 1.23) but not by the in-memory pipes the tests use. When available,
// Attach uses it to interrupt a stdin goroutine parked in Read once the
// session ends, so that goroutine does not outlive Attach and steal a
// keystroke from whatever reads stdin next.
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
	// done is closed the moment Attach is about to return, for any reason
	// (host bye, local detach, or ctx cancel). The size-polling goroutine
	// below only has a ticker to drive it, so without this it would run
	// forever past Attach's return whenever the exit came from the result
	// channel rather than ctx cancellation - a plain goroutine leak.
	done := make(chan struct{})
	defer close(done)

	// host → stdout
	go func() {
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
				switch act {
				case ActionDetach:
					writeFrame(FDetach, nil)
					select {
					case result <- "":
					default:
					}
					return
				case ActionTakeover:
					writeFrame(FTakeover, nil)
				}
				if len(fwd) > 0 && !opt.View {
					if werr := writeFrame(FInput, fwd); werr != nil {
						select {
						case result <- "connection closed":
						default:
						}
						return
					}
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

	// Best-effort: interrupt a stdin goroutine still parked in Read so it
	// does not outlive this call. Only works when opt.Stdin supports read
	// deadlines (a real terminal); an io.Reader that doesn't (e.g. an
	// io.PipeReader with no pending write, as in tests) can't be
	// interrupted this way and is left to exit on its own next input.
	if d, ok := opt.Stdin.(deadlineReader); ok {
		d.SetReadDeadline(time.Now())
	}
	select {
	case <-stdinDone:
	case <-time.After(50 * time.Millisecond):
	}

	io.WriteString(opt.Stdout, clearScreen)
	return reason, retErr
}
