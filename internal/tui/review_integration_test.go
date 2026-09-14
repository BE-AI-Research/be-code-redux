package tui

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/live"
	"github.com/brown-enterprises/be-code/internal/review"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// stuckEditor is the editor half of a shared review that never answers — the
// person is at the phone, not the desk. It records the withdrawal the
// coordinator sends once a terminal has decided.
type stuckEditor struct {
	mu       sync.Mutex
	reviewed []string
	cancels  chan string
}

func (e *stuckEditor) Review(ctx context.Context, rel, old, new string, shared bool) tools.ReviewDecision {
	e.mu.Lock()
	e.reviewed = append(e.reviewed, rel)
	e.mu.Unlock()
	<-ctx.Done() // answered elsewhere, or the run was cancelled
	return tools.ReviewCancelled
}

func (e *stuckEditor) Cancel(rel string) {
	select {
	case e.cancels <- rel:
	default:
	}
}

// The whole shared-review path through the real TUI: a served session with
// VS Code's terminal and an SSH terminal attached, ide.review at its default
// auto, an editor that shows the diff but never answers. The coordinator runs
// on the agent goroutine (as the write tool does) while this goroutine plays
// the Bubble Tea event loop, draining each view's mailbox and pressing the
// keys the clients press.
//
// What it proves: auto resolves to both from the roster labels; the terminal
// side raises the shared ask, so *both* attached terminals show it; the
// second terminal's `y` decides the write even though the editor still has
// the diff open; the editor is then told to withdraw it; and the terminal
// that answered is not told that someone answered.
func TestSharedReviewAnsweredFromTheSecondTerminal(t *testing.T) {
	s, a, b := twoViews(t)
	// The roster the host would report: VS Code's own terminal plus one that
	// is not it, which is exactly what makes auto resolve to both.
	roster := []live.ClientInfo{
		{ID: 1, Label: "vscode (pid 1)", UTF8: true},
		{ID: 2, Label: "ssh from 10.0.0.5 (pid 2)", UTF8: true},
	}
	s.SetClients(roster)
	flush(a, b)
	// labels is read from the coordinator's goroutine; roster is written
	// before it starts and never again.
	labels := make([]string, 0, len(roster))
	for _, c := range roster {
		labels = append(labels, c.Label)
	}

	editor := &stuckEditor{cancels: make(chan string, 1)}
	coord := review.New(review.ModeAuto, editor, s.ReviewTerminal(), func() []string { return labels })
	s.SetReview(coord)
	if got := coord.Resolve(); got != review.ModeBoth {
		t.Fatalf("auto resolved to %q with an SSH terminal attached, want both", got)
	}

	s.setRunState(true, "thinking") // a run is in progress, as it would be
	flush(a, b)
	decided := make(chan tools.ReviewDecision, 1)
	go func() { // the tool goroutine
		decided <- coord.Decide(context.Background(), "internal/core/core.go", "old\n", "new\n")
	}()

	// The terminal side of the race raises the shared ask on every terminal.
	waitFor(t, func() bool { flush(a, b); return a.mode == modeAsk && b.mode == modeAsk })
	if !strings.Contains(a.View(), "approval required") || !strings.Contains(b.View(), "approval required") {
		t.Fatal("the shared review must show on both terminals")
	}
	if open := s.current(); open == nil || open.Action != "file_write" || open.Detail == "" {
		t.Fatalf("the shared ask is not a file-write approval with a diff: %+v", open)
	}
	// Both sides of the race start on their own goroutines, so the editor may
	// be a few microseconds behind the terminal prompt: wait for it.
	waitFor(t, func() bool {
		editor.mu.Lock()
		defer editor.mu.Unlock()
		return len(editor.reviewed) > 0
	})

	// The person on the SSH terminal answers.
	b.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	select {
	case d := <-decided:
		if d != tools.ReviewAccept {
			t.Fatalf("decision = %v, want ReviewAccept", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the write was never decided")
	}
	select {
	case rel := <-editor.cancels:
		if rel != "internal/core/core.go" {
			t.Fatalf("editor withdrawal for %q", rel)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the editor diff was never withdrawn")
	}

	flush(a, b)
	if a.mode != modeBusy || b.mode != modeBusy {
		t.Fatalf("modes after the answer: a=%v b=%v, want modeBusy", a.mode, b.mode)
	}
	// The terminal that answered is not told that someone answered; the one
	// that did not is told who.
	if strings.Contains(b.wrapped, "answered by") {
		t.Fatalf("the answering terminal was told it answered:\n%s", b.wrapped)
	}
	if !strings.Contains(a.wrapped, "answered by ssh from 10.0.0.5 (pid 2)") {
		t.Fatalf("the watching terminal was not told who answered:\n%s", a.wrapped)
	}
	if !strings.Contains(a.wrapped, "approved") || !strings.Contains(b.wrapped, "approved") {
		t.Fatal("the verdict is a shared entry and must be on both")
	}
}
