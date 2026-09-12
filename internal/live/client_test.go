package live

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer wraps bytes.Buffer with a mutex: Attach's "host → stdout"
// goroutine writes to the client's Stdout concurrently with this test's
// polling reads of it, and a plain bytes.Buffer is not safe for that (the
// race detector catches it even though the two never actually corrupt each
// other's output in practice, thanks to the polling interval).
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func TestAttachRoundTripAndDetach(t *testing.T) {
	h, sock := startHost(t)
	rec := &Record{Code: "T", PID: os.Getpid(), Socket: sock, Token: "tok"}
	stdinR, stdinW := io.Pipe()
	var stdout syncBuffer
	done := make(chan string, 1)
	go func() {
		reason, _ := Attach(context.Background(), rec, AttachOptions{
			Label: "test", Stdin: stdinR, Stdout: &stdout,
			Raw:  func() (func(), error) { return func() {}, nil },
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

// TestAttachForwardsBytesBeforeDetachInSameRead covers a single Read that
// delivers ordinary bytes and a full detach chord together (e.g. pasted
// text ending in Ctrl+] d): the bytes preceding the chord must still reach
// the program before Attach detaches.
func TestAttachForwardsBytesBeforeDetachInSameRead(t *testing.T) {
	h, sock := startHost(t)
	rec := &Record{Code: "T", PID: os.Getpid(), Socket: sock, Token: "tok"}
	stdinR, stdinW := io.Pipe()
	done := make(chan string, 1)
	go func() {
		reason, _ := Attach(context.Background(), rec, AttachOptions{Label: "t", Stdin: stdinR, Stdout: io.Discard,
			Raw: func() (func(), error) { return func() {}, nil }, Size: func() (int, int) { return 80, 24 }, UTF8: true})
		done <- reason
	}()
	within(t, time.Second, func() bool { return len(h.Clients()) == 1 })

	// A single Write matched by the client's single 4096-byte Read call
	// delivers "ab\x1dd" as one Read, exercising Chord.Feed's single-call
	// path rather than the chord split across two Feed calls.
	stdinW.Write([]byte("ab\x1dd"))
	buf := make([]byte, 4)
	n, _ := h.InputReader().Read(buf)
	if string(buf[:n]) != "ab" {
		t.Fatalf("program got %q, want \"ab\"", buf[:n])
	}
	select {
	case reason := <-done:
		if reason != "" {
			t.Fatalf("reason = %q", reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Attach did not return after the detach chord")
	}
}

// TestAttachResetsStdinDeadlineOnReturn covers a real *os.File as Stdin
// (os.Pipe, which - unlike the io.Pipe the other tests use - supports
// SetReadDeadline just like a real terminal does). Attach interrupts a
// blocked stdin read with a deadline on the way out; if it failed to reset
// that deadline afterward, every later read of the same file would fail
// instantly with an i/o timeout.
func TestAttachResetsStdinDeadlineOnReturn(t *testing.T) {
	h, sock := startHost(t)
	rec := &Record{Code: "T", PID: os.Getpid(), Socket: sock, Token: "tok"}
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdinR.Close()
	defer stdinW.Close()
	done := make(chan string, 1)
	go func() {
		reason, _ := Attach(context.Background(), rec, AttachOptions{Label: "t", Stdin: stdinR, Stdout: io.Discard,
			Raw: func() (func(), error) { return func() {}, nil }, Size: func() (int, int) { return 80, 24 }, UTF8: true})
		done <- reason
	}()
	within(t, time.Second, func() bool { return len(h.Clients()) == 1 })
	h.Close("session ended")
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Attach did not return on host close")
	}

	// Attach's own stdin goroutine is no longer reading stdinR (Attach has
	// returned); a leftover deadline would make this read fail instantly.
	if _, err := stdinW.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	stdinR.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4)
	n, err := stdinR.Read(buf)
	if err != nil {
		t.Fatalf("read after Attach returned: %v", err)
	}
	if string(buf[:n]) != "ok" {
		t.Fatalf("got %q", buf[:n])
	}
}
