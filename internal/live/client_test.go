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
