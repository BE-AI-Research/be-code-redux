package tui

import (
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
	if !strings.Contains(m.transcript.String(), "answered in VS Code") {
		t.Fatalf("withdrawal note missing:\n%s", m.transcript.String())
	}
	if len(resp) != 0 {
		t.Fatal("a withdrawn prompt must not answer the coordinator")
	}
	// A late second withdrawal (the losing goroutine waking up) is a no-op.
	m.Update(approvalCancelMsg{note: "answered in VS Code"})
	if got := strings.Count(m.transcript.String(), "answered in VS Code"); got != 1 {
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
	if !strings.Contains(m.transcript.String(), "review: both (resolves to both)") {
		t.Fatalf("mode line missing:\n%s", m.transcript.String())
	}
	m.slashCommand("/review tui", 0)
	if !strings.Contains(m.transcript.String(), "review: tui (resolves to tui)") {
		t.Fatalf("mode not set:\n%s", m.transcript.String())
	}
	if m.review.Mode() != review.ModeTUI {
		t.Fatalf("coordinator mode = %q", m.review.Mode())
	}
	m.slashCommand("/review nonsense", 0)
	if !strings.Contains(m.transcript.String(), "nonsense") {
		t.Fatalf("invalid mode not reported:\n%s", m.transcript.String())
	}
	if m.review.Mode() != review.ModeTUI {
		t.Fatalf("invalid mode changed the setting: %q", m.review.Mode())
	}
	// auto with no roster (in-process) resolves to editor.
	m.slashCommand("/review auto", 0)
	if !strings.Contains(m.transcript.String(), "review: auto (resolves to editor)") {
		t.Fatalf("auto resolution missing:\n%s", m.transcript.String())
	}
}
