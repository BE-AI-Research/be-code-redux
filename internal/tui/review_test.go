package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/review"
)

// A y/n answer resolves the shared ask normally with the coordinator wired.
func TestApprovalStillAnswerable(t *testing.T) {
	tempHome(t)
	s := newTestSession(t)
	m := s.NewView(0, "local")
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	ok := make(chan bool, 1)
	go func() { ok <- s.ReviewTerminal().Ask(context.Background(), "a.go") }()
	waitFor(t, func() bool { flush(m); return m.mode == modeAsk })
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	if !<-ok {
		t.Fatal("approval not granted")
	}
	if m.mode == modeAsk {
		t.Fatal("the answering view must close its own modal")
	}
}

func TestReviewCommand(t *testing.T) {
	m := newTestModel(t)
	m.SetReview(review.New(review.ModeBoth, nil, m.ReviewTerminal(), nil))
	m.slashCommand("/review")
	flush(m)
	if !strings.Contains(m.rendered.String(), "review: both (resolves to both)") {
		t.Fatalf("mode line missing:\n%s", m.rendered.String())
	}
	m.slashCommand("/review tui")
	flush(m)
	if !strings.Contains(m.rendered.String(), "review: tui (resolves to tui)") {
		t.Fatalf("mode not set:\n%s", m.rendered.String())
	}
	if m.review.Mode() != review.ModeTUI {
		t.Fatalf("coordinator mode = %q", m.review.Mode())
	}
	m.slashCommand("/review nonsense")
	flush(m)
	if !strings.Contains(m.rendered.String(), "nonsense") {
		t.Fatalf("invalid mode not reported:\n%s", m.rendered.String())
	}
	if m.review.Mode() != review.ModeTUI {
		t.Fatalf("invalid mode changed the setting: %q", m.review.Mode())
	}
	// auto with no roster (in-process) resolves to editor.
	m.slashCommand("/review auto")
	flush(m)
	if !strings.Contains(m.rendered.String(), "review: auto (resolves to editor)") {
		t.Fatalf("auto resolution missing:\n%s", m.rendered.String())
	}
}
