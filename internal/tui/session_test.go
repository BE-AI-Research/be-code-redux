package tui

import (
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
// instead of stalling the core.
func TestBroadcastNeverBlocksOnAFullMailbox(t *testing.T) {
	s := newTestSession(t)
	v := s.NewView(1, "x")
	_ = v
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10000; i++ {
			s.broadcast(entryMsg{entry{Kind: entryDim, Text: "x"}})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("broadcast blocked on a full mailbox")
	}
}
