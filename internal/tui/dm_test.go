package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/inbox"
	"github.com/brown-enterprises/be-code/internal/live"
)

func dmSession(t *testing.T, id string) (*Session, *View) {
	s := newTestSession(t)
	s.inboxDir = t.TempDir()
	s.onlineFn = func(string) bool { return false }
	s.resolveFn = func(inbox.Terminal) (inbox.Resolution, error) { return inbox.Resolution{ID: id, How: "config"}, nil }
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "l", IP: "1.1.1.1", User: id}})
	v := s.NewView(1, "l")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return s, v
}

func TestInboxListsThreadsAndEnterOpensOne(t *testing.T) {
	s, v := dmSession(t, "alice")
	inbox.Send(s.inboxDir, "bob", "alice", "can you look at the picking tests when you have a moment")
	time.Sleep(2 * time.Millisecond)
	inbox.Send(s.inboxDir, "carol", "alice", "lunch?")
	v.slashCommand("/inbox")
	out := v.View()
	if v.mode != modeInbox || !strings.Contains(out, "● carol") || !strings.Contains(out, "lunch?") || strings.Index(out, "carol") > strings.Index(out, "bob") {
		t.Fatalf("mode %v:\n%s", v.mode, out)
	}
	if v.unreadDMs() != 2 || !strings.Contains(v.bottomLine(), "inbox (2)") {
		t.Fatalf("unread %d bottom %q", v.unreadDMs(), v.bottomLine())
	}
	v.Update(tea.KeyMsg{Type: tea.KeyEnter}) // carol
	out = v.View()
	if v.mode != modeDM || !strings.Contains(out, "dm carol") || !strings.Contains(out, "(not online)") || !strings.Contains(out, "lunch?") {
		t.Fatalf("mode %v:\n%s", v.mode, out)
	}
	if v.unreadDMs() != 1 { // opening marks carol's thread read; bob's stays
		t.Fatalf("unread after open: %d", v.unreadDMs())
	}
}

func TestDMSendsAFileAndAgentMentionIsPlainText(t *testing.T) {
	s, v := dmSession(t, "alice")
	started := false
	s.startTurnHook = func(string) { started = true }
	v.slashCommand("/dm bob")
	v.input.SetValue("@agent this must not reach the model")
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	th, _ := inbox.Thread(s.inboxDir, "alice", "bob")
	if len(th) != 1 || th[0].From != "alice" || !strings.Contains(th[0].Text, "@agent") {
		t.Fatalf("thread %+v", th)
	}
	if started || s.ag.Pending() != 0 {
		t.Fatal("a DM reached the agent")
	}
	if !strings.Contains(v.View(), "alice: @agent") {
		t.Fatalf("own message not shown:\n%s", v.View())
	}
}

func TestAnArrivingDMPingsTheOwnerOnly(t *testing.T) {
	s := newTestSession(t)
	s.inboxDir = t.TempDir()
	s.resolveFn = func(tm inbox.Terminal) (inbox.Resolution, error) {
		return inbox.Resolution{ID: tm.User, How: "config"}, nil
	}
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "a", User: "alice"}, {ID: 2, Label: "b", User: "bob"}})
	va, vb := s.NewView(1, "a"), s.NewView(2, "b")
	for _, v := range []*View{va, vb} {
		v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	}
	m, _ := inbox.Send(s.inboxDir, "carol", "alice", "hey")
	s.deliverDM(m)
	drainAll(t, va, vb)
	if va.unreadDMs() != 1 || vb.unreadDMs() != 0 {
		t.Fatalf("unread a=%d b=%d", va.unreadDMs(), vb.unreadDMs())
	}
	if !strings.Contains(va.shownToast(), "DM from carol") {
		t.Fatalf("toast %q", va.shownToast())
	}
	if vb.shownToast() != "" {
		t.Fatalf("bob's terminal saw alice's DM: %q", vb.shownToast())
	}
	// With alice's thread open, the line appears at once.
	va.slashCommand("/dm carol")
	m2, _ := inbox.Send(s.inboxDir, "carol", "alice", "second")
	s.deliverDM(m2)
	drainAll(t, va)
	if !strings.Contains(va.View(), "second") {
		t.Fatalf("open thread missed the line:\n%s", va.View())
	}
}

func TestNarrowDMHidesTheContactColumn(t *testing.T) {
	s, v := dmSession(t, "alice")
	inbox.Send(s.inboxDir, "bob", "alice", "x")
	v.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	v.slashCommand("/dm bob")
	if out := v.View(); strings.Contains(out, "│") || !strings.Contains(out, "Tab") {
		t.Fatalf("narrow view:\n%s", out)
	}
}

// The "(not online)" check reads every live record; the DM view renders on
// the cursor blink, so the answer is cached for a few seconds.
func TestOnlineCheckIsCachedAcrossRenders(t *testing.T) {
	s, v := dmSession(t, "alice")
	calls := 0
	s.onlineFn = func(string) bool { calls++; return false }
	inbox.Send(s.inboxDir, "bob", "alice", "x")
	v.slashCommand("/dm bob")
	for i := 0; i < 20; i++ {
		v.View()
	}
	if calls != 1 {
		t.Fatalf("online checked %d times over 20 renders", calls)
	}
	v.slashCommand("/dm carol") // a different thread asks afresh
	v.View()
	if calls != 2 {
		t.Fatalf("a new thread did not re-check: %d", calls)
	}
}
