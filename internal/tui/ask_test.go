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
	// current and Answer are documented as "caller holds s.mu"; a test that
	// calls them by hand honours that too.
	s.mu.Lock()
	gen := s.current().Gen
	s.mu.Unlock()
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	s.mu.Lock()
	_, ok := s.Answer(gen, askAnswer{OK: true}, 2, b)
	s.mu.Unlock()
	if ok {
		t.Fatal("an answer for a resolved generation must be ignored")
	}
}

// A picked row acts through the view that rendered the ask — the terminal
// whose cursor chose it — not through a view looked up from the answering
// client id. onPick's effects are view-local (a session switch hands *that*
// terminal over), so being handed the wrong view, or none, would silently
// pick nothing at all.
func TestSharedPickerOnPickGetsTheAnsweringView(t *testing.T) {
	tempHome(t)
	s := newTestSession(t)
	s.served = true
	m := s.NewView(1, "desk (pid 1)")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "desk (pid 1)", UTF8: true}})
	flush(m)

	picked := make(chan string, 1)
	go s.Ask(context.Background(), &ask{Kind: askPicker, Title: "Select model",
		Items: []pickItem{{id: "one", label: "one"}, {id: "two", label: "two"}},
		onPick: func(v *View, id string, from int) tea.Cmd {
			if v != m {
				t.Error("onPick must be given the view that rendered the ask")
			}
			if from != 1 {
				t.Errorf("onPick got client %d, want the one that answered", from)
			}
			picked <- id
			return nil
		}})
	waitFor(t, func() bool { flush(m); return m.mode == modeAsk })
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	select {
	case id := <-picked:
		if id != "one" {
			t.Fatalf("picked %q", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onPick never ran")
	}
	flush(m)
	if m.mode == modeAsk {
		t.Fatal("the picker is still open")
	}
	// The person who pressed Enter is not told that somebody answered.
	if strings.Contains(m.wrapped, "answered by") {
		t.Fatalf("the answering terminal was told it answered:\n%s", m.wrapped)
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

// TestShowAskDoesNotColorizeSubAgentResume is item 5's second test gap:
// showAsk's undiffed branch (consult/model_reload/sub_agent_resume all skip
// ui.ColorizeDiff) was untested for the new action. A detail line starting
// with "-" would be recoloured as a diff deletion (wrapped in the ANSI
// sequence \x1b[31m...\x1b[0m) if it reached the diff-colouring branch
// instead — this is a synthetic detail built to make that distinction
// visible; resumeAskDetail's own real format never starts a line with "-",
// which is exactly why this needs its own test rather than trusting the
// agent-level detail tests to exercise the branch.
func TestShowAskDoesNotColorizeSubAgentResume(t *testing.T) {
	v := newTestModel(t)
	a := &ask{Kind: askApproval, Action: "sub_agent_resume",
		Detail: "resume sub-agent work from the previous session?\n- 3.2 port internal/scan\nnothing has been sent to any model yet"}
	v.showAsk(a)
	out := v.modalVP.View()
	if strings.Contains(out, "\x1b[31m") || strings.Contains(out, "\x1b[32m") {
		t.Fatalf("sub_agent_resume's detail was run through the diff colouriser:\n%q", out)
	}
	if !strings.Contains(out, "- 3.2 port internal/scan") {
		t.Fatalf("modal body missing the detail text unmodified:\n%s", out)
	}
}

// TestSubAgentResumeAskPressingAlwaysLeavesTheModalOpen is item 5's first
// test gap: no test pressed "a" on the new modal. handleAskKey's "a" branch
// has an explicit guard for sub_agent_resume (decided = false) because this
// action has no standing grant to offer — falling through to the final
// else would flip cfg.ApproveFileWrites and Tools.ApproveWrites off for the
// whole session, disabling every future file-write review because someone
// pressed "a" on an unrelated startup question.
func TestSubAgentResumeAskPressingAlwaysLeavesTheModalOpen(t *testing.T) {
	s, a, _ := twoViews(t)
	beforeCfg := s.cfg.ApproveFileWrites
	beforeReg := s.ag.Tools.ApproveWrites
	decided := make(chan bool, 1)
	go func() {
		decided <- s.approveFromAgent("sub_agent_resume",
			"resume sub-agent work from the previous session?\n  3.2 port internal/scan — big (ollama-lan/qwen3:32b), scope: internal/scan\nnothing has been sent to any model yet")
	}()
	waitFor(t, func() bool { flush(a); return a.mode == modeAsk })

	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	flush(a)
	if a.mode != modeAsk {
		t.Fatal(`"a" closed the sub_agent_resume modal, but this action has no "always" to grant`)
	}
	if s.cfg.ApproveFileWrites != beforeCfg || s.ag.Tools.ApproveWrites != beforeReg {
		t.Fatal(`"a" on sub_agent_resume fell through to the file-write branch and changed ApproveFileWrites/ApproveWrites`)
	}
	select {
	case <-decided:
		t.Fatal(`"a" answered the question, but this action has no "always"`)
	default:
	}

	// Clean up the still-open ask so the waiting goroutine (and the test)
	// do not leak: "n" is an ordinary decline.
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	select {
	case ok := <-decided:
		if ok {
			t.Fatal("n must decline")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("n never answered after a's no-op")
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
