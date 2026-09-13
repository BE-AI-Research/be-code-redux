package review

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/tools"
)

type fakeEditor struct {
	mu        sync.Mutex
	decide    chan tools.ReviewDecision
	cancelled []string
	shared    bool
	calls     int
}

func (e *fakeEditor) Review(ctx context.Context, rel, old, new string, shared bool) tools.ReviewDecision {
	e.mu.Lock()
	e.calls++
	e.shared = shared
	e.mu.Unlock()
	select {
	case d := <-e.decide:
		return d
	case <-ctx.Done():
		return tools.ReviewCancelled
	}
}
func (e *fakeEditor) Cancel(rel string) {
	e.mu.Lock()
	e.cancelled = append(e.cancelled, rel)
	e.mu.Unlock()
}

type fakeTerm struct {
	mu        sync.Mutex
	answer    chan bool
	withdrawn []string
	calls     int
}

func (t *fakeTerm) Ask(ctx context.Context, preview string) bool {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	select {
	case a := <-t.answer:
		return a
	case <-ctx.Done():
		return false
	}
}
func (t *fakeTerm) Withdraw(note string) {
	t.mu.Lock()
	t.withdrawn = append(t.withdrawn, note)
	t.mu.Unlock()
}

func setup(mode Mode, labels []string) (*Coordinator, *fakeEditor, *fakeTerm) {
	e := &fakeEditor{decide: make(chan tools.ReviewDecision, 1)}
	tm := &fakeTerm{answer: make(chan bool, 1)}
	var clients func() []string
	if labels != nil {
		clients = func() []string { return labels }
	}
	return New(mode, e, tm, clients), e, tm
}

func TestResolveAuto(t *testing.T) {
	c, _, _ := setup("auto", nil)
	if c.Resolve() != "editor" {
		t.Fatal("unserved must resolve to editor")
	}
	c, _, _ = setup("auto", []string{"vscode (pid 1)"})
	if c.Resolve() != "editor" {
		t.Fatal("solo vscode terminal must resolve to editor")
	}
	c, _, _ = setup("auto", []string{"vscode (pid 1)", "ssh from 10.0.0.5 (pid 2)"})
	if c.Resolve() != "both" {
		t.Fatal("a remote client must resolve to both")
	}
	if err := c.SetMode("nonsense"); err == nil {
		t.Fatal("invalid mode accepted")
	}
}

func TestEditorAnswersFirstWithdrawsTerminal(t *testing.T) {
	c, e, tm := setup("both", nil)
	e.decide <- tools.ReviewAccept
	if d := c.Decide(context.Background(), "a.go", "", "x"); d != tools.ReviewAccept {
		t.Fatalf("decision %v", d)
	}
	waitFor(t, func() bool {
		tm.mu.Lock()
		defer tm.mu.Unlock()
		return len(tm.withdrawn) == 1 && tm.withdrawn[0] == "answered in VS Code"
	})
	if !e.shared {
		t.Fatal("a shared review must be requested as shared")
	}
}

func TestTerminalAnswersFirstCancelsEditor(t *testing.T) {
	c, e, tm := setup("both", nil)
	tm.answer <- false
	if d := c.Decide(context.Background(), "a.go", "", "x"); d != tools.ReviewReject {
		t.Fatalf("decision %v", d)
	}
	waitFor(t, func() bool {
		e.mu.Lock()
		defer e.mu.Unlock()
		return len(e.cancelled) == 1 && e.cancelled[0] == "a.go"
	})
}

func TestEditorUnavailableLeavesTerminalAlone(t *testing.T) {
	c, e, tm := setup("both", nil)
	e.decide <- tools.ReviewUnavailable
	go func() { time.Sleep(50 * time.Millisecond); tm.answer <- true }()
	if d := c.Decide(context.Background(), "a.go", "", "x"); d != tools.ReviewAccept {
		t.Fatalf("decision %v", d)
	}
	if len(e.cancelled) != 0 {
		t.Fatal("nothing to cancel on an unavailable editor")
	}
}

func TestModesEditorAndTui(t *testing.T) {
	c, e, tm := setup("tui", nil)
	if d := c.Decide(context.Background(), "a.go", "", "x"); d != tools.ReviewUnavailable || e.calls != 0 || tm.calls != 0 {
		t.Fatalf("tui mode: %v editor=%d term=%d (fs.go asks Approve itself)", d, e.calls, tm.calls)
	}
	c, e, tm = setup("editor", []string{"vscode (pid 1)", "ssh (pid 2)"})
	e.decide <- tools.ReviewReject
	if d := c.Decide(context.Background(), "a.go", "", "x"); d != tools.ReviewReject || tm.calls != 0 {
		t.Fatalf("editor mode must never raise the terminal prompt: %v term=%d", d, tm.calls)
	}
}

func TestCancelledContextRejects(t *testing.T) {
	c, _, _ := setup("both", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if d := c.Decide(ctx, "a.go", "", "x"); d != tools.ReviewReject {
		t.Fatalf("decision %v", d)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met")
}
