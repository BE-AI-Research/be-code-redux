package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/live"
)

// Two views of one session see the same entries, each rendered at its own
// size and with its own styles; a resize of one leaves the other's buffer
// untouched.
func TestTwoViewsShareEntriesButRenderSeparately(t *testing.T) {
	s := newTestSession(t)
	a := s.NewView(1, "desk (pid 1)")
	b := s.NewView(2, "tablet (pid 2)")
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	b.Update(tea.WindowSizeMsg{Width: 40, Height: 15})
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "desk (pid 1)", UTF8: true}, {ID: 2, Label: "tablet (pid 2)", UTF8: true}})
	s.appendEntry(entry{Kind: entryTool, Label: "read_file", Text: strings.Repeat("a", 200)})
	flush(a, b)
	if len(a.entries) != len(b.entries) || len(a.entries) < 1 {
		t.Fatalf("entries diverged: %d vs %d", len(a.entries), len(b.entries))
	}
	if !strings.Contains(a.rendered.String(), "read_file") || !strings.Contains(b.rendered.String(), "read_file") {
		t.Fatal("entry not rendered on both views")
	}
	if len(b.rendered.String()) >= len(a.rendered.String()) {
		t.Fatal("the narrow view did not truncate tool args")
	}
	if !b.compact() || a.compact() {
		t.Fatalf("layouts: a compact=%v b compact=%v", a.compact(), b.compact())
	}
	before := a.rendered.String()
	b.Update(tea.WindowSizeMsg{Width: 60, Height: 15})
	if a.rendered.String() != before {
		t.Fatal("resizing view b changed view a's rendered buffer")
	}
	// Every view got the roster; the bottom line shows both labels.
	if !strings.Contains(a.View(), "⧉ 2") {
		t.Fatalf("view a bottom line: %q", a.View())
	}
}

// Broadcast must never block: a view whose mailbox is full drops messages
// instead of stalling the core. What is dropped is only the *rendering* of
// an entry — the entry itself is on the shared transcript, so a rebuild
// (any resize or theme change) puts the view back in step.
func TestBroadcastNeverBlocksOnAFullMailbox(t *testing.T) {
	const entries = 10000 // mailboxDepth is 256: the rest must be dropped, not queued
	s := newTestSession(t)
	v := s.NewView(1, "x")
	v.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	before := len(s.entries)
	done := make(chan struct{})
	go func() {
		for i := 0; i < entries; i++ {
			// appendEntry is the broadcast path: it records the entry on the
			// session and hands an entryMsg to every view's mailbox.
			s.appendEntry(entry{Kind: entryDim, Text: fmt.Sprintf("line %d", i)})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("broadcast blocked on a full mailbox")
	}
	if got := len(s.Entries()); got != before+entries {
		t.Fatalf("session kept %d entries, want %d: a dropped message must not lose the entry", got, before+entries)
	}
	flush(v)
	if v.renderedN >= len(s.entries) {
		t.Fatalf("renderedN = %d of %d: the test never actually overflowed the mailbox", v.renderedN, len(s.entries))
	}
	v.rebuild()
	if v.renderedN != len(s.entries) {
		t.Fatalf("rebuild left the view at %d of %d entries", v.renderedN, len(s.entries))
	}
	if !strings.Contains(v.rendered.String(), fmt.Sprintf("line %d", entries-1)) {
		t.Fatal("rebuild did not resync the view to the newest entry")
	}
}

// The interleaving a bare render counter cannot survive: a rebuild overtakes
// the entryMsgs still queued for the entries it just painted, and a fresh
// entry is appended before the drain runs. Each entry must be rendered
// exactly once — the stale messages dropped, the new one drawn in its own
// place rather than in a stale one's.
func TestRebuildOvertakingQueuedEntriesRendersEachOnce(t *testing.T) {
	s := newTestSession(t)
	v := s.NewView(1, "x")
	v.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	for _, text := range []string{"alpha", "bravo", "charlie"} {
		s.appendEntry(entry{Kind: entryPlain, Text: text}) // queued, not yet delivered
	}
	v.rebuild() // what a WindowSizeMsg does, before Update drains the mailbox
	s.appendEntry(entry{Kind: entryPlain, Text: "delta"})
	flush(v)
	got := v.rendered.String()
	for _, text := range []string{"alpha", "bravo", "charlie", "delta"} {
		if n := strings.Count(got, text); n != 1 {
			t.Fatalf("%q rendered %d times, want once:\n%s", text, n, got)
		}
	}
	if v.renderedN != len(s.entries) {
		t.Fatalf("render cursor at %d of %d entries", v.renderedN, len(s.entries))
	}
}

// A broadcast the delivery goroutine is holding must not be overtaken by the
// drain View.Update runs at the end of its own body. The mailbox has two
// would-be consumers — that goroutine, parked inside p.Send, and that drain
// — and if both took messages off the queue, whatever the goroutine held
// would be applied last: an entryMsg overtaken that way is dropped outright,
// because its index is already behind View.renderedN by the time it lands.
// So the goroutine takes nothing and delivers a bare poke instead.
func TestABroadcastIsNotOvertakenByTheUpdateDrain(t *testing.T) {
	s := newTestSession(t)
	v := s.NewView(1, "desk")
	v.Update(tea.WindowSizeMsg{Width: 100, Height: 30})

	// Stands in for tea.Program.Send, which blocks until the program's update
	// loop takes the message: the delivery goroutine parks here holding
	// whatever it had to hand over.
	held := make(chan tea.Msg, 1)
	release := make(chan struct{})
	go v.mb.run(func(msg tea.Msg) {
		held <- msg
		<-release
	})

	s.appendEntry(entry{Kind: entryPlain, Text: "five"})
	s.appendEntry(entry{Kind: entryPlain, Text: "six"})
	first := <-held // the goroutine has taken something and is parked

	// Meanwhile the program is already inside Update for an earlier message,
	// and that Update's own drain empties the queue behind the parked
	// goroutine.
	v.Update(statusMsg("busy"))
	close(release)
	v.Update(first) // and now what was held finally lands

	got := v.rendered.String()
	i, j := strings.Index(got, "five"), strings.Index(got, "six")
	if i < 0 || j < 0 {
		t.Fatalf("an entry was lost (five at %d, six at %d):\n%s", i, j, got)
	}
	if i > j {
		t.Fatalf("entries rendered out of order:\n%s", got)
	}
	if n := strings.Count(got, "five"); n != 1 {
		t.Fatalf("five rendered %d times, want once:\n%s", n, got)
	}
}
