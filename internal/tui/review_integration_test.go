package tui

import (
	"context"
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
// coordinator sends once the terminal has decided.
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
// the Bubble Tea event loop, delivering the messages the coordinator sends and
// the keys the clients press.
//
// What it proves: auto resolves to both from the roster labels; the terminal
// side raises the ordinary approval modal, so it is shared by every attached
// terminal; the SSH terminal's `y` decides the write even though the editor
// still has the diff open; and the editor is then told to withdraw it.
func TestSharedReviewAnsweredFromTheSecondTerminal(t *testing.T) {
	m := twoClients(t)
	// The roster the host would report: VS Code's own terminal plus one that
	// is not it, which is exactly what makes auto resolve to both.
	roster := []live.ClientInfo{
		{ID: 1, Label: "vscode (pid 1)", UTF8: true},
		{ID: 2, Label: "ssh from 10.0.0.5 (pid 2)", UTF8: true},
	}
	m.Update(clientsMsg(roster))

	// send has no tea.Program to deliver to in a test, so route what the
	// coordinator's goroutine sends into a channel this goroutine drains.
	msgs := make(chan tea.Msg, 8)
	m.sendHook = func(msg tea.Msg) { msgs <- msg }

	editor := &stuckEditor{cancels: make(chan string, 1)}
	// labels is read from the coordinator's goroutine; roster is written
	// before it starts and never again.
	coord := review.New(review.ModeAuto, editor, m.ReviewTerminal(), func() []string {
		out := make([]string, 0, len(roster))
		for _, c := range roster {
			out = append(out, c.Label)
		}
		return out
	})
	m.SetReview(coord)
	if got := coord.Resolve(); got != review.ModeBoth {
		t.Fatalf("auto resolved to %q with an SSH terminal attached, want both", got)
	}

	m.mode, m.running = modeBusy, true // a run is in progress, as it would be
	decided := make(chan tools.ReviewDecision, 1)
	go func() { // the tool goroutine
		decided <- coord.Decide(context.Background(), "internal/core/core.go", "old\n", "new\n")
	}()

	// The terminal side's approval reaches the event loop as the same message
	// a tool approval uses.
	var raised bool
	select {
	case msg := <-msgs:
		am, ok := msg.(approvalMsg)
		if !ok {
			t.Fatalf("coordinator sent %T, want an approvalMsg", msg)
		}
		if am.action != "file_write" {
			t.Fatalf("approval action = %q", am.action)
		}
		if am.detail == "" {
			t.Fatal("approval carries no diff preview")
		}
		m.Update(msg)
		raised = true
	case <-time.After(2 * time.Second):
		t.Fatal("the shared review never raised the terminal prompt")
	}
	if !raised || m.mode != modeApproval {
		t.Fatalf("mode = %v, want modeApproval", m.mode)
	}
	editor.mu.Lock()
	shown := len(editor.reviewed)
	editor.mu.Unlock()
	if shown == 0 {
		t.Fatal("the editor was never asked to show the diff")
	}

	// The person on the SSH terminal answers.
	m.Update(live.ClientKeyMsg{Client: 2, Key: runes("y")})
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
	if m.mode != modeBusy {
		t.Fatalf("mode after the answer = %v, want modeBusy", m.mode)
	}
}
