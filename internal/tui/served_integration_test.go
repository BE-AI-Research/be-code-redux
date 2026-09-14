package tui

import (
	"context"
	"strings"
	"testing"
	"time"
)

// settle is how long a terminal is given to have done something, in the one
// assertion whose subject is that it did nothing.
const settle = 300 * time.Millisecond

// The whole served stack, end to end: a real live.Host on a real socket, a
// real runner, and two real Bubble Tea programs rendering into two fake
// terminals of different sizes.
//
// What it pins down is the point of the per-terminal rendering wave. One
// transcript reaches both terminals, but each one lays it out for itself —
// the 40x15 phone in the compact layout, the 120x40 desk in the full one
// with its header — one terminal's resize relays out that terminal and no
// other, a detach takes down exactly one program, and Quit returns
// RunServed with every program stopped.
func TestServedSessionRendersEachClientAtItsOwnSize(t *testing.T) {
	tempHome(t)
	s := newTestSession(t)
	h, sock, token := newTestHostFor(t)
	done := make(chan error, 1)
	go func() { done <- s.RunServed(context.Background(), h) }()

	small := dialFake(t, sock, token, "phone (pid 1)", 40, 15)
	large := dialFake(t, sock, token, "desk (pid 2)", 120, 40)
	waitFor(t, func() bool { return len(h.Clients()) == 2 })
	smallID, largeID := idFor(t, h, "phone (pid 1)"), idFor(t, h, "desk (pid 2)")

	s.appendEntry(entry{Kind: entryDim, Text: "shared line"})
	smallOut := small.collect(t, "shared line", 3*time.Second)
	largeOut := large.collect(t, "shared line", 3*time.Second)

	if !strings.Contains(smallOut, "> ") || strings.Contains(smallOut, "(>): ") {
		t.Fatalf("small client not in compact layout:\n%s", smallOut)
	}
	if strings.Contains(smallOut, "BE-Code Redux") {
		t.Fatalf("small client must not draw the header:\n%s", smallOut)
	}
	if !strings.Contains(largeOut, "(>): ") || !strings.Contains(largeOut, "BE-Code Redux") {
		t.Fatalf("large client not in full layout with header:\n%s", largeOut)
	}

	// One terminal's resize is that terminal's own business: it relays out
	// for its new size and the other keeps the size it reported. There is
	// no shared minimum any more, which is the regression this guards.
	beforeLarge := large.bytesSoFar()
	small.resize(t, 60, 20)
	waitFor(t, func() bool { w, h := viewSize(t, s, smallID); return w == 60 && h == 20 })
	// Then a settle window, which is the one thing a condition wait cannot
	// replace: what is being checked is that nothing happened on the large
	// terminal, and "nothing" has no edge to wait for. The small terminal
	// having finished relaying out is the cue that the resize has been
	// delivered at all.
	time.Sleep(settle)
	if w, hgt := viewSize(t, s, largeID); w != 120 || hgt != 40 {
		t.Fatalf("the large terminal was relaid out at %dx%d when the small one resized", w, hgt)
	}
	// Cost, not correctness, and deliberately only logged: runner.onClients
	// still re-sends every running program its own WindowSizeMsg on a roster
	// change, and a resize is one — which makes Bubble Tea's renderer drop
	// its frame cache and rewrite the large terminal in full even though
	// nothing on it changed. The clear-on-FSize that used to justify it is
	// gone (live.FSize is unused since 0.8.0 and only an FSize makes a client
	// clear), so this number should be 0; see the task 9 report.
	if n := large.bytesSoFar() - beforeLarge; n > 0 {
		t.Logf("note: the small terminal's resize rewrote %d bytes on the large one (runner.onClients repaint)", n)
	}

	// A detach takes down exactly one program — that terminal's view is let
	// go of — while the session and the other terminal carry on.
	small.detach(t)
	waitFor(t, func() bool { return len(h.Clients()) == 1 })
	waitFor(t, func() bool { return s.viewByID(smallID) == nil })
	if s.viewByID(largeID) == nil {
		t.Fatal("the remaining terminal's view was retired too")
	}
	s.appendEntry(entry{Kind: entryDim, Text: "after the detach"})
	large.collect(t, "after the detach", 3*time.Second)

	// And Quit returns RunServed with every program stopped — the invariant
	// the closing lines in cmd/live.go depend on.
	s.Quit()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunServed did not return after Quit")
	}
}

// A view that dies mid-frame takes its own terminal down and nothing else:
// the runner's Run goroutine recovers, logs, and drops that client with
// "view error", while the session and every other program keep going.
func TestPanickingViewIsDroppedAndTheSessionContinues(t *testing.T) {
	tempHome(t)
	s := newTestSession(t)
	h, sock, token := newTestHostFor(t)
	done := make(chan error, 1)
	go func() { done <- s.RunServed(context.Background(), h) }()

	a := dialFake(t, sock, token, "a (pid 1)", 80, 24)
	b := dialFake(t, sock, token, "b (pid 2)", 80, 24)
	waitFor(t, func() bool { return len(h.Clients()) == 2 })
	// Both programs have to be up before the hook is armed, or the entry
	// below would render into a view that does not exist yet.
	b.collect(t, "attached: b (pid 2)", 3*time.Second)

	id := idFor(t, h, "a (pid 1)")
	v := s.viewByID(id)
	if v == nil {
		t.Fatalf("no view for client %d", id)
	}
	s.mu.Lock() // update runs under the session lock; so does arming its hook
	v.panicOnNextUpdate = true
	s.mu.Unlock()

	s.appendEntry(entry{Kind: entryDim, Text: "boom"})
	if reason := a.readBye(t); reason != "view error" {
		t.Fatalf("bye reason %q, want %q", reason, "view error")
	}

	// The other terminal still gets that entry, and the ones after it.
	b.collect(t, "boom", 3*time.Second)
	waitFor(t, func() bool { return len(h.Clients()) == 1 })
	s.appendEntry(entry{Kind: entryDim, Text: "still here"})
	b.collect(t, "still here", 3*time.Second)

	// The dead view is detached and its mailbox closed (the runner stops it
	// on a goroutine of its own once the roster has dropped the client, so
	// this is waited for, not read once), which is what lets the session be
	// quit cleanly below rather than waiting on a program that is never
	// coming back.
	waitFor(t, func() bool { return s.viewByID(id) == nil })
	s.Quit()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunServed did not return after Quit")
	}
}
