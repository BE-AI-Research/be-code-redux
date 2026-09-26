package browsertest

import (
	"fmt"
	"sync"
	"testing"
)

// recordingTB wraps a real testing.TB, delegating everything (Helper,
// Cleanup, Fatal, ...) to it except Errorf, which it records instead of
// failing the outer test — so a test can assert that a fake Browser
// reported a misuse (Errorf) without that failing the test doing the
// asserting.
type recordingTB struct {
	testing.TB
	mu   sync.Mutex
	msgs []string
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.mu.Lock()
	r.msgs = append(r.msgs, fmt.Sprintf(format, args...))
	r.mu.Unlock()
}

func (r *recordingTB) errors() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.msgs...)
}

// TestBrowserEmitWithNoConnectionErrors: Emit right after New, before any
// client has ever dialed in, must not silently do nothing — it must wait
// for a connection (up to a bound) and report the misuse if none ever
// arrives, rather than pretending the event was delivered.
func TestBrowserEmitWithNoConnectionErrors(t *testing.T) {
	rt := &recordingTB{TB: t}
	fb := New(rt)
	fb.Emit("", "Target.targetCreated", map[string]any{})
	if len(rt.errors()) == 0 {
		t.Fatal("Emit with no connection ever, and none arriving, did not report an error")
	}
}

// TestBrowserDropWithNoConnectionIsQuiet: unlike Emit, Drop with nothing to
// drop is a legitimate no-op and must not report anything.
func TestBrowserDropWithNoConnectionIsQuiet(t *testing.T) {
	rt := &recordingTB{TB: t}
	fb := New(rt)
	fb.Drop()
	if errs := rt.errors(); len(errs) != 0 {
		t.Fatalf("Drop with no connection reported: %v", errs)
	}
}

// TestBrowserEmitAfterClosingIsQuiet: once the fake has begun tearing down
// (closing set, as New's own t.Cleanup does before it drops the last
// connection), a late Emit — such as one a background goroutine schedules
// with EmitSoon, arriving after the test it belongs to has already
// returned — must return quietly. Calling Errorf at that point would land
// on a testing.T whose test function may already have finished, which the
// testing package turns into a process-wide panic instead of a normal
// failure of just that one test.
func TestBrowserEmitAfterClosingIsQuiet(t *testing.T) {
	rt := &recordingTB{TB: t}
	fb := New(rt)
	fb.mu.Lock()
	fb.closing = true
	fb.mu.Unlock()
	fb.Emit("", "Page.javascriptDialogClosed", map[string]any{})
	if errs := rt.errors(); len(errs) != 0 {
		t.Fatalf("Emit after closing reported: %v", errs)
	}
}
