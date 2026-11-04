package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/inbox"
	"github.com/brown-enterprises/be-code/internal/store"
)

// A name lands on the same colour everywhere, and names spread over the set.
func TestUserColorIsStableAndSpread(t *testing.T) {
	if userColorIndex("alice", 6) != userColorIndex("alice", 6) {
		t.Fatal("not stable")
	}
	seen := map[int]bool{}
	for _, n := range []string{"alice", "bob", "carol", "dave", "erin", "frank", "grace", "heidi"} {
		seen[userColorIndex(n, 6)] = true
	}
	if len(seen) < 3 {
		t.Fatalf("eight names used %d colours", len(seen))
	}
	if userColorIndex("x", 0) != 0 {
		t.Fatal("an empty set must not divide by zero")
	}
}

// The room's frame: a title row with the buttons, the rule under it, chips
// for people, the bar on a line that names you, and @names coloured — and
// none of it on a compact terminal, which keeps the plain footer.
func TestRoomFrameHasButtonsChipsAndTheMentionBar(t *testing.T) {
	s, va, vb := pingPair(t)
	va.slashCommand("/chat")
	s.Post("bob", "@alice look at this", "")
	s.Post("bob", "hello everyone", "")
	drainAll(t, va, vb)
	out := va.View()
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[0], "▌ ROOM") || !strings.Contains(lines[0], "Room") || !strings.Contains(lines[0], "Inbox") || !strings.Contains(lines[0], "← Back") {
		t.Fatalf("title row: %q", lines[0])
	}
	if !strings.Contains(lines[1], "━━━") {
		t.Fatalf("no rule under the title: %q", lines[1])
	}
	if !strings.Contains(out, "▐") {
		t.Fatalf("the line naming alice has no bar:\n%s", out)
	}
	if !strings.Contains(out, " bob  @alice look at this") {
		t.Fatalf("chip and text:\n%s", out)
	}
	bar := 0
	for _, l := range lines {
		if strings.Contains(l, "▐") {
			bar++
		}
	}
	if bar != 1 {
		t.Fatalf("%d lines carry the bar, want 1", bar)
	}
	if len(va.btnZones) != 5 {
		t.Fatalf("%d button zones", len(va.btnZones))
	}
	// Compact: no title row, no rules, the plain footer.
	va.Update(tea.WindowSizeMsg{Width: 60, Height: 18})
	out = va.View()
	if strings.Contains(out, "▌ ROOM") || strings.Contains(out, "━━━") || !strings.Contains(out, "chat · Esc") {
		t.Fatalf("compact room:\n%s", out)
	}
}

// Clicking a button on the title row does what the command does.
func TestTitleRowButtonsAnswerClicks(t *testing.T) {
	_, va, _ := pingPair(t)
	va.slashCommand("/chat")
	va.View()
	zone := func(action string) chatButton {
		for _, z := range va.btnZones {
			if z.action == action {
				return z
			}
		}
		t.Fatalf("no %s button", action)
		return chatButton{}
	}
	z := zone("inbox")
	va.Update(mouse(z.x0+1, 0, tea.MouseButtonLeft, tea.MouseActionPress))
	if va.mode != modeInbox {
		t.Fatalf("mode after clicking Inbox: %v", va.mode)
	}
	va.View()
	z = zone("pings")
	va.Update(mouse(z.x0+1, 0, tea.MouseButtonLeft, tea.MouseActionPress))
	if va.cfg.Chat.PingsOn("alice") {
		t.Fatal("clicking the pings button did not switch them off")
	}
	va.View()
	z = zone("dm")
	va.Update(mouse(z.x0+1, 0, tea.MouseButtonLeft, tea.MouseActionPress))
	if va.input.Value() != "/dm " {
		t.Fatalf("DM button filled %q", va.input.Value())
	}
	va.input.Reset()
	va.View()
	z = zone("back")
	va.Update(mouse(z.x0+1, 0, tea.MouseButtonLeft, tea.MouseActionPress))
	if va.mode == modeInbox || va.mode == modeChat {
		t.Fatalf("mode after Back: %v", va.mode)
	}
	// A click off the row does nothing in these modes.
	va.slashCommand("/chat")
	va.View()
	va.Update(mouse(2, 5, tea.MouseButtonLeft, tea.MouseActionPress))
	if va.mode != modeChat {
		t.Fatal("a click in the room changed mode")
	}
}

// A day change and a quiet stretch earn a divider; a short gap does not.
func TestRoomDividers(t *testing.T) {
	_, va, _ := pingPair(t)
	now := time.Now()
	va.room = []store.ChatLine{
		{TS: now.Add(-26 * time.Hour), User: "bob", Text: "yesterday"},
		{TS: now.Add(-30 * time.Minute), User: "bob", Text: "earlier"},
		{TS: now.Add(-29 * time.Minute), User: "bob", Text: "right after"},
		{TS: now, User: "bob", Text: "now"},
	}
	out := va.renderRoom()
	if !strings.Contains(out, "─ today ─") {
		t.Fatalf("no day divider:\n%s", out)
	}
	if !strings.Contains(out, "─ "+now.Format("15:04")+" ─") {
		t.Fatalf("no time divider:\n%s", out)
	}
	dividers := 0
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "─") {
			dividers++
		}
	}
	if dividers != 2 {
		t.Fatalf("%d dividers, want 2 (a minute's gap earns none):\n%s", dividers, out)
	}
}

// The inbox: a row per thread with the unread mark, the chip, presence,
// the time, how many are new and the last line; the open thread carries
// the bar on the lines that were unread when it was opened.
func TestInboxRowsAndUnreadBarInTheThread(t *testing.T) {
	s, va, _ := pingPair(t)
	inbox.Send(s.inboxDir, "carol", "alice", "the review is ready")
	va.slashCommand("/inbox")
	out := va.View()
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[0], "▌ INBOX") || !strings.Contains(lines[0], "Inbox ●1") {
		t.Fatalf("title row: %q", lines[0])
	}
	if !strings.Contains(out, "●  carol   ○ away") || !strings.Contains(out, "1 new  the review is ready") {
		t.Fatalf("row:\n%s", out)
	}
	va.Update(tea.KeyMsg{Type: tea.KeyEnter})
	out = va.View()
	lines = strings.Split(out, "\n")
	if !strings.Contains(lines[0], "▌ DM  carol  ○ away") {
		t.Fatalf("DM title: %q", lines[0])
	}
	if !strings.Contains(out, "▐") {
		t.Fatalf("the unread line has no bar:\n%s", out)
	}
	// A reply of your own re-opens the thread: nothing is unread now.
	va.input.SetValue("looking")
	va.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if strings.Contains(va.View(), "▐") {
		t.Fatalf("the bar stayed after the thread was re-read:\n%s", va.View())
	}
}

// The bottom line counts room lines that named you since you last looked,
// and the room clears it.
func TestBottomLineCountsMentions(t *testing.T) {
	s, va, vb := pingPair(t)
	s.Post("bob", "@alice one", "")
	s.Post("bob", "plain", "")
	s.Post("bob", "@alice two", "")
	drainAll(t, va, vb)
	if !strings.Contains(va.bottomLine(), "@ (2)") || !strings.Contains(va.bottomLine(), "chat (3 new)") {
		t.Fatalf("alice's bottom line: %q", va.bottomLine())
	}
	if strings.Contains(vb.bottomLine(), "@ (") {
		t.Fatalf("bob's bottom line counts alice's mentions: %q", vb.bottomLine())
	}
	va.slashCommand("/chat")
	va.slashCommand("/back")
	if strings.Contains(va.bottomLine(), "@ (") {
		t.Fatalf("opening the room did not clear the count: %q", va.bottomLine())
	}
}
