package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/live"
)

func twoViews(t *testing.T) (*Session, *View, *View) {
	t.Helper()
	tempHome(t)
	s := newTestSession(t)
	s.served = true
	a := s.NewView(1, "desk (pid 1)")
	b := s.NewView(2, "tablet (pid 2)")
	a.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	b.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "desk (pid 1)", UTF8: true}, {ID: 2, Label: "tablet (pid 2)", UTF8: true}})
	flush(a, b)
	return s, a, b
}

func TestSharedApprovalFirstAnswerWinsAndTheOtherViewIsToldWho(t *testing.T) {
	s, a, b := twoViews(t)
	decided := make(chan bool, 1)
	go func() { decided <- s.approveFromAgent("file_write", "--- a\n+++ b\n") }()
	waitFor(t, func() bool { flush(a, b); return a.mode == modeAsk && b.mode == modeAsk })
	if !strings.Contains(a.View(), "approval required") || !strings.Contains(b.View(), "approval required") {
		t.Fatal("both views must show the modal")
	}
	b.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	select {
	case ok := <-decided:
		if !ok {
			t.Fatal("y must approve")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the agent goroutine was never answered")
	}
	flush(a, b)
	if a.mode == modeAsk || b.mode == modeAsk {
		t.Fatalf("modals still open: a=%v b=%v", a.mode, b.mode)
	}
	if !strings.Contains(a.wrapped, "answered by tablet (pid 2)") {
		t.Fatalf("view a was not told who answered:\n%s", a.wrapped)
	}
	if strings.Contains(b.wrapped, "answered by") {
		t.Fatal("the answering view must not be told it answered")
	}
	if !strings.Contains(a.wrapped, "file_write") || !strings.Contains(b.wrapped, "approved") {
		t.Fatal("the verdict line is a shared entry and must be on both")
	}
}

func TestStaleAnswerIsIgnored(t *testing.T) {
	s, a, b := twoViews(t)
	go s.approveFromAgent("shell", "ls")
	waitFor(t, func() bool { flush(a, b); return a.mode == modeAsk && b.mode == modeAsk })
	gen := s.current().Gen
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if _, ok := s.Answer(gen, askAnswer{OK: true}, 2); ok {
		t.Fatal("an answer for a resolved generation must be ignored")
	}
}

func TestEditorWithdrawalClosesEveryView(t *testing.T) {
	s, a, b := twoViews(t)
	term := s.ReviewTerminal()
	res := make(chan bool, 1)
	go func() { res <- term.Ask(context.Background(), "diff") }()
	waitFor(t, func() bool { flush(a, b); return a.mode == modeAsk })
	term.Withdraw("answered in VS Code")
	flush(a, b)
	if a.mode == modeAsk || b.mode == modeAsk {
		t.Fatal("withdrawal left a modal open")
	}
	if !strings.Contains(a.wrapped, "answered in VS Code") || !strings.Contains(b.wrapped, "answered in VS Code") {
		t.Fatal("withdrawal note missing")
	}
	select {
	case ok := <-res:
		if ok {
			t.Fatal("a withdrawn ask must return false")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Ask did not return after Withdraw")
	}
}

func TestSharedPickerCursorIsLocalButTheAnswerIsShared(t *testing.T) {
	s, a, b := twoViews(t)
	picked := make(chan string, 1)
	go s.Ask(context.Background(), &ask{Kind: askPicker, Title: "Select model", Items: []pickItem{{id: "one", label: "one"}, {id: "two", label: "two"}},
		onPick: func(v *View, id string, from int) tea.Cmd { picked <- id; return nil }})
	waitFor(t, func() bool { flush(a, b); return a.mode == modeAsk && b.mode == modeAsk })
	b.Update(tea.KeyMsg{Type: tea.KeyDown})
	if a.picker.cursor != 0 || b.picker.cursor != 1 {
		t.Fatalf("cursors: a=%d b=%d", a.picker.cursor, b.picker.cursor)
	}
	b.Update(tea.KeyMsg{Type: tea.KeyEnter})
	select {
	case id := <-picked:
		if id != "two" {
			t.Fatalf("picked %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onPick never ran")
	}
	flush(a, b)
	if a.mode == modeAsk {
		t.Fatal("view a still shows the picker")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in 2s")
}
