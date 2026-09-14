package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/review"
)

// The other place answered first: the still-open modal closes with a dimmed
// note and the run goes back to busy. Nothing is sent on resp — the
// coordinator already has its answer.
func TestApprovalCancelClosesModalWithNote(t *testing.T) {
	m := newTestModel(t)
	m.running = true
	resp := make(chan bool, 1)
	m.Update(approvalMsg{action: "file_write", detail: "--- a.go\n+x", resp: resp})
	if m.mode != modeApproval {
		t.Fatalf("modal did not open: mode %v", m.mode)
	}
	m.Update(approvalCancelMsg{note: "answered in VS Code"})
	if m.mode != modeBusy {
		t.Fatalf("mode after withdrawal = %v, want modeBusy", m.mode)
	}
	if m.approval != nil {
		t.Fatal("approval still pending after withdrawal")
	}
	if !strings.Contains(m.rendered.String(), "answered in VS Code") {
		t.Fatalf("withdrawal note missing:\n%s", m.rendered.String())
	}
	if len(resp) != 0 {
		t.Fatal("a withdrawn prompt must not answer the coordinator")
	}
	// A late second withdrawal (the losing goroutine waking up) is a no-op.
	m.Update(approvalCancelMsg{note: "answered in VS Code"})
	if got := strings.Count(m.rendered.String(), "answered in VS Code"); got != 1 {
		t.Fatalf("note repeated %d times", got)
	}
}

// A withdrawal with no note closes the modal silently.
func TestApprovalCancelWithoutNote(t *testing.T) {
	m := newTestModel(t)
	m.Update(approvalMsg{action: "file_write", detail: "a.go", resp: make(chan bool, 1)})
	m.Update(approvalCancelMsg{})
	if m.mode == modeApproval || m.approval != nil {
		t.Fatal("modal still open")
	}
}

// A y/n answer still resolves the prompt normally with the coordinator wired.
func TestApprovalStillAnswerable(t *testing.T) {
	m := newTestModel(t)
	resp := make(chan bool, 1)
	m.Update(approvalMsg{action: "file_write", detail: "a.go", resp: resp})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if !<-resp {
		t.Fatal("approval not granted")
	}
}

func TestReviewCommand(t *testing.T) {
	m := newTestModel(t)
	m.SetReview(review.New(review.ModeBoth, nil, m.ReviewTerminal(), nil))
	m.slashCommand("/review", 0)
	flush(m)
	if !strings.Contains(m.rendered.String(), "review: both (resolves to both)") {
		t.Fatalf("mode line missing:\n%s", m.rendered.String())
	}
	m.slashCommand("/review tui", 0)
	flush(m)
	if !strings.Contains(m.rendered.String(), "review: tui (resolves to tui)") {
		t.Fatalf("mode not set:\n%s", m.rendered.String())
	}
	if m.review.Mode() != review.ModeTUI {
		t.Fatalf("coordinator mode = %q", m.review.Mode())
	}
	m.slashCommand("/review nonsense", 0)
	flush(m)
	if !strings.Contains(m.rendered.String(), "nonsense") {
		t.Fatalf("invalid mode not reported:\n%s", m.rendered.String())
	}
	if m.review.Mode() != review.ModeTUI {
		t.Fatalf("invalid mode changed the setting: %q", m.review.Mode())
	}
	// auto with no roster (in-process) resolves to editor.
	m.slashCommand("/review auto", 0)
	flush(m)
	if !strings.Contains(m.rendered.String(), "review: auto (resolves to editor)") {
		t.Fatalf("auto resolution missing:\n%s", m.rendered.String())
	}
}

// A shared review answered in the editor the instant it was raised delivers
// its withdrawal *before* the prompt it withdraws (both cross to the Update
// goroutine through p.Send). The prompt must then never open: a modal whose
// answer nobody is waiting for cannot be dismissed by the person looking at
// it, and the reply channel has to be answered so the asker is not stuck.
func TestApprovalCancelBeforeItsPromptDropsThePrompt(t *testing.T) {
	m := newTestModel(t)
	m.running = true
	m.Update(approvalCancelMsg{note: "answered in VS Code", gen: 1})
	resp := make(chan bool, 1)
	m.Update(approvalMsg{action: "file_write", detail: "--- a.go\n+x", resp: resp, gen: 1})
	if m.mode == modeApproval || m.approval != nil {
		t.Fatalf("phantom modal opened: mode %v", m.mode)
	}
	select {
	case ok := <-resp:
		if ok {
			t.Fatal("a dropped prompt must not approve the write")
		}
	default:
		t.Fatal("a dropped prompt must answer its channel")
	}
}

// The other order: a stale withdrawal (the previous write's losing
// goroutine waking up late) must not close the prompt of the write that
// came after it, which someone still has to answer.
func TestStaleApprovalCancelLeavesTheNewerPromptOpen(t *testing.T) {
	m := newTestModel(t)
	m.running = true
	resp := make(chan bool, 1)
	m.Update(approvalMsg{action: "file_write", detail: "--- b.go\n+y", resp: resp, gen: 7})
	if m.mode != modeApproval {
		t.Fatalf("modal did not open: mode %v", m.mode)
	}
	m.Update(approvalCancelMsg{note: "answered in VS Code", gen: 6})
	if m.mode != modeApproval || m.approval == nil {
		t.Fatal("a stale withdrawal closed the newer prompt")
	}
	if strings.Contains(m.rendered.String(), "answered in VS Code") {
		t.Fatalf("stale note printed:\n%s", m.rendered.String())
	}
	// Its own withdrawal still closes it.
	m.Update(approvalCancelMsg{note: "answered in VS Code", gen: 7})
	if m.mode == modeApproval || m.approval != nil {
		t.Fatal("the matching withdrawal did not close the prompt")
	}
}

// The generation comes from the adapter the coordinator calls, so a
// withdrawal always carries the number of the ask it is withdrawing, and a
// later ask gets a fresh one.
func TestReviewTerminalNumbersEachAsk(t *testing.T) {
	m := newTestModel(t)
	msgs := make(chan tea.Msg, 8)
	m.sendOverride = func(msg tea.Msg) { msgs <- msg }
	term := m.ReviewTerminal()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Ask sends its prompt, then returns on the cancelled ctx
	if term.Ask(ctx, "preview") {
		t.Fatal("a cancelled Ask must not approve")
	}
	term.Withdraw("cancelled")
	first, ok := (<-msgs).(approvalMsg)
	if !ok || first.gen != 1 {
		t.Fatalf("first ask = %+v, want gen 1", first)
	}
	wd, ok := (<-msgs).(approvalCancelMsg)
	if !ok || wd.gen != first.gen {
		t.Fatalf("withdrawal %+v does not match ask gen %d", wd, first.gen)
	}
	if term.Ask(ctx, "preview 2") {
		t.Fatal("a cancelled Ask must not approve")
	}
	second, ok := (<-msgs).(approvalMsg)
	if !ok || second.gen != 2 {
		t.Fatalf("second ask = %+v, want gen 2", second)
	}
}
