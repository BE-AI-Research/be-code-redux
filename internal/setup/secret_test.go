package setup

import (
	"errors"
	"io"
	"os"
	"testing"
	"time"

	"github.com/charmbracelet/x/term"
)

// I6a (final review): an interrupt during the no-echo read restores the
// terminal's saved state before the process exits; a read that finishes
// normally leaves the handler unregistered and never exits.
func TestReadSecretRestoresTheTerminalOnInterrupt(t *testing.T) {
	saved := &term.State{}
	var notified chan<- os.Signal
	restored := make(chan *term.State, 1)
	exited := make(chan struct{})
	release := make(chan struct{})
	prev := []any{secretGetState, secretRestore, secretReadPassword, secretNotify, secretStopNotify, secretExit, secretOut}
	t.Cleanup(func() {
		secretGetState = prev[0].(func(uintptr) (*term.State, error))
		secretRestore = prev[1].(func(uintptr, *term.State) error)
		secretReadPassword = prev[2].(func(uintptr) ([]byte, error))
		secretNotify = prev[3].(func(chan<- os.Signal))
		secretStopNotify = prev[4].(func(chan<- os.Signal))
		secretExit = prev[5].(func())
		secretOut = prev[6].(io.Writer)
	})
	secretOut = io.Discard
	secretGetState = func(uintptr) (*term.State, error) { return saved, nil }
	secretRestore = func(_ uintptr, s *term.State) error { restored <- s; return nil }
	secretNotify = func(c chan<- os.Signal) { notified = c }
	secretStopNotify = func(chan<- os.Signal) {}
	// os.Exit is not survivable in a test: record it and let the parked
	// read return, as the process ending would have ended it.
	secretExit = func() { close(exited); close(release) }
	secretReadPassword = func(uintptr) ([]byte, error) {
		notified <- os.Interrupt // Ctrl-C while echo is off
		<-release
		return nil, errors.New("interrupted")
	}
	if _, err := ReadSecret(0); err == nil {
		t.Fatal("an interrupted read returned a key")
	}
	select {
	case s := <-restored:
		if s != saved {
			t.Fatal("restored some other state")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the terminal state was not restored on interrupt")
	}
	select {
	case <-exited:
	default:
		t.Fatal("no exit after the interrupt")
	}

	// A normal read: no restore, no exit.
	secretExit = func() { t.Fatal("exited after a normal read") }
	secretRestore = func(uintptr, *term.State) error { t.Fatal("restored after a normal read"); return nil }
	secretReadPassword = func(uintptr) ([]byte, error) { return []byte("k"), nil }
	if b, err := ReadSecret(0); err != nil || string(b) != "k" {
		t.Fatalf("normal read failed (%d bytes, %v)", len(b), err)
	}
}
