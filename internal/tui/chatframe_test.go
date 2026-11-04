package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
	// A fixed noon: at real midnight the -30m lines would fall on the
	// previous day and earn a day divider instead of a time one.
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.Local)
	va.now = func() time.Time { return now }
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

// The selected inbox row is marked by a cursor (a background would end at
// the first styled piece inside the row).
func TestInboxCursorMarksTheSelectedRow(t *testing.T) {
	s, va, _ := pingPair(t)
	inbox.Send(s.inboxDir, "bob", "alice", "one")
	time.Sleep(2 * time.Millisecond)
	inbox.Send(s.inboxDir, "carol", "alice", "two")
	va.slashCommand("/inbox")
	rows := func() (sel, other string) {
		for _, l := range strings.Split(va.View(), "\n") {
			switch {
			case strings.Contains(l, " carol "):
				sel = l
			case strings.Contains(l, " bob "):
				other = l
			}
		}
		return
	}
	sel, other := rows()
	if !strings.HasPrefix(sel, "▸") || strings.HasPrefix(other, "▸") {
		t.Fatalf("cursor: %q / %q", sel, other)
	}
	va.Update(tea.KeyMsg{Type: tea.KeyDown})
	sel, other = rows()
	if strings.HasPrefix(sel, "▸") || !strings.HasPrefix(other, "▸") {
		t.Fatalf("cursor after Down: %q / %q", sel, other)
	}
}

// An @name keeps its spelling and its punctuation; @agent takes the
// model's colour; an e-mail address is left alone.
func TestColorMentionsKeepsTheSpelling(t *testing.T) {
	_, va, _ := pingPair(t)
	for _, c := range []struct{ in, want string }{
		{"ping @Alice.", "ping @Alice."},
		{"ask @agent, not a@b.c", "ask @agent, not a@b.c"},
		{"@bob-2's turn", "@bob-2's turn"},
	} {
		if got := va.colorMentions(c.in); got != c.want {
			t.Fatalf("%q -> %q, want %q", c.in, got, c.want)
		}
	}
	if va.nameStyle("agent").GetBold() != va.st.NameAgent.GetBold() {
		t.Fatal("agent's name style is not the model's")
	}
}

// The inbox answers every row's presence from one read of the live records
// per onlineTTL, however many threads there are.
func TestInboxPresenceIsOneScanPerTTL(t *testing.T) {
	s, va, _ := pingPair(t)
	scans := 0
	s.liveUsersFn = func() map[string]bool { scans++; return map[string]bool{"user03": true} }
	for i := 0; i < 8; i++ {
		inbox.Send(s.inboxDir, fmt.Sprintf("user%02d", i), "alice", "hello")
	}
	va.slashCommand("/inbox")
	for i := 0; i < 10; i++ {
		va.View()
	}
	if scans != 1 {
		t.Fatalf("%d scans for 8 rows over 10 renders", scans)
	}
	if out := va.View(); !strings.Contains(out, "user03   ● online") || strings.Count(out, "● online") != 1 {
		t.Fatalf("presence:\n%s", out)
	}
}

// /clear empties the room and the mention count with it.
func TestRoomResetClearsTheMentionCount(t *testing.T) {
	s, va, vb := pingPair(t)
	s.Post("bob", "@alice one", "")
	drainAll(t, va, vb)
	if !strings.Contains(va.bottomLine(), "@ (1)") {
		t.Fatalf("bottom line: %q", va.bottomLine())
	}
	va.Update(roomResetMsg{})
	if strings.Contains(va.bottomLine(), "@ (") {
		t.Fatalf("the count survived a room reset: %q", va.bottomLine())
	}
}

// A draft never follows a click into another view: a half-typed DM is
// parked, the room gets nothing, and the draft is back when that thread is
// reopened. The active button does nothing; DM ▸ fills only an empty input.
func TestButtonClicksParkTheDraft(t *testing.T) {
	s, va, _ := pingPair(t)
	inbox.Send(s.inboxDir, "carol", "alice", "hey")
	zone := func(action string) chatButton {
		va.View()
		for _, z := range va.btnZones {
			if z.action == action {
				return z
			}
		}
		t.Fatalf("no %s button", action)
		return chatButton{}
	}
	va.slashCommand("/dm carol")
	va.input.SetValue("private to carol")
	// DM ▸ inside a thread leaves a draft alone, and fills an empty input.
	z := zone("dm")
	va.Update(mouse(z.x0+1, 0, tea.MouseButtonLeft, tea.MouseActionPress))
	if va.input.Value() != "private to carol" {
		t.Fatalf("DM overwrote the draft: %q", va.input.Value())
	}
	z = zone("room")
	va.Update(mouse(z.x0+1, 0, tea.MouseButtonLeft, tea.MouseActionPress))
	if va.mode != modeChat || va.input.Value() != "" {
		t.Fatalf("the DM draft followed the click into the room: mode %v input %q", va.mode, va.input.Value())
	}
	va.Update(tea.KeyMsg{Type: tea.KeyEnter})
	for _, l := range s.Room() {
		if strings.Contains(l.Text, "private") {
			t.Fatalf("a DM draft was posted to the room: %q", l.Text)
		}
	}
	// The active button is a no-op and keeps the room's own draft.
	va.input.SetValue("for the room")
	z = zone("room")
	va.Update(mouse(z.x0+1, 0, tea.MouseButtonLeft, tea.MouseActionPress))
	if va.mode != modeChat || va.input.Value() != "for the room" {
		t.Fatalf("after clicking the active button: mode %v draft %q", va.mode, va.input.Value())
	}
	// Into the inbox: the list keeps its keys (no draft in its input), and
	// Enter on carol's row brings the DM draft back.
	z = zone("inbox")
	va.Update(mouse(z.x0+1, 0, tea.MouseButtonLeft, tea.MouseActionPress))
	if va.mode != modeInbox || va.input.Value() != "" {
		t.Fatalf("inbox: mode %v input %q", va.mode, va.input.Value())
	}
	va.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if va.mode != modeDM || va.dm.with != "carol" || va.input.Value() != "private to carol" {
		t.Fatalf("the DM draft did not come back: mode %v with %q input %q", va.mode, va.dm.with, va.input.Value())
	}
	// And the room's draft is back when the room is reopened.
	va.slashCommand("/chat")
	if va.input.Value() != "for the room" {
		t.Fatalf("the room's draft did not come back: %q", va.input.Value())
	}
	z = zone("dm")
	va.input.Reset()
	va.Update(mouse(z.x0+1, 0, tea.MouseButtonLeft, tea.MouseActionPress))
	if va.input.Value() != "/dm " {
		t.Fatalf("DM ▸ on an empty input filled %q", va.input.Value())
	}
}

// The full layout's frame fits the terminal at the sizes just above
// compact, with a name as long as an ID can be.
func TestFramedViewsFitTheirTerminal(t *testing.T) {
	s, va, _ := pingPair(t)
	long := strings.Repeat("n", 32)
	inbox.Send(s.inboxDir, long, "alice", strings.Repeat("a long first line ", 10))
	inbox.Send(s.inboxDir, "bob", "alice", "hi")
	s.Post(long, "@alice "+strings.Repeat("words ", 30), "")
	for _, size := range [][2]int{{70, 20}, {100, 20}, {120, 40}} {
		va.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, c := range []struct{ name, cmd string }{{"chat", "/chat"}, {"inbox", "/inbox"}, {"dm", "/dm " + long}} {
			va.slashCommand(c.cmd)
			out := va.View()
			lines := strings.Split(out, "\n")
			if len(lines) > size[1] {
				t.Fatalf("%dx%d %s: %d lines:\n%s", size[0], size[1], c.name, len(lines), out)
			}
			for i, l := range lines {
				if w := lipgloss.Width(l); w > size[0] {
					t.Fatalf("%dx%d %s: line %d is %d columns: %q", size[0], size[1], c.name, i, w, l)
				}
			}
			if !strings.Contains(lines[0], "▌ ") || !strings.Contains(lines[1], "━") {
				t.Fatalf("%dx%d %s: no frame:\n%s", size[0], size[1], c.name, out)
			}
		}
	}
}

// Tab to the next thread parks the draft with the thread it was typed for.
func TestTabBetweenThreadsParksTheDraft(t *testing.T) {
	s, va, _ := pingPair(t)
	inbox.Send(s.inboxDir, "bob", "alice", "one")
	time.Sleep(2 * time.Millisecond)
	inbox.Send(s.inboxDir, "carol", "alice", "two")
	va.slashCommand("/dm carol")
	va.input.SetValue("private to carol")
	va.Update(tea.KeyMsg{Type: tea.KeyTab})
	if va.dm.with != "bob" || va.input.Value() != "" || !strings.Contains(va.input.Placeholder, "message bob") {
		t.Fatalf("after Tab: with %q input %q placeholder %q", va.dm.with, va.input.Value(), va.input.Placeholder)
	}
	va.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if th, _ := inbox.Thread(s.inboxDir, "alice", "bob"); len(th) != 1 {
		t.Fatalf("something was sent to bob: %+v", th)
	}
	va.Update(tea.KeyMsg{Type: tea.KeyTab})
	if va.dm.with != "carol" || va.input.Value() != "private to carol" {
		t.Fatalf("back in carol's thread: with %q input %q", va.dm.with, va.input.Value())
	}
}
