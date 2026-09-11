package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/provider"
)

func TestReviewWriteDecisions(t *testing.T) {
	dir := t.TempDir()
	prompted := 0
	reg, _ := NewRegistry(dir, func(a, d string) bool { prompted++; return true })
	reg.ApproveWrites = true
	var statuses []string
	reg.OnStatus = func(s string) { statuses = append(statuses, s) }
	write := func() Result {
		return reg.Dispatch(context.Background(), provider.ToolCall{Name: "write_file", Arguments: `{"path":"a.txt","content":"v"}`})
	}
	reg.ReviewWrite = func(_ context.Context, rel, oldC, newC string) ReviewDecision { return ReviewReject }
	if res := write(); !res.IsError {
		t.Fatal("reject not honoured")
	}
	reg.ReviewWrite = func(_ context.Context, rel, oldC, newC string) ReviewDecision { return ReviewAccept }
	if res := write(); res.IsError {
		t.Fatal(res.Content)
	}
	if prompted != 0 {
		t.Fatalf("TUI approver called %d times although the editor decided", prompted)
	}
	reg.ReviewWrite = func(_ context.Context, rel, oldC, newC string) ReviewDecision { return ReviewUnavailable }
	write()
	if prompted != 1 {
		t.Fatal("fallback to Approve did not happen")
	}
	reg.ReviewWrite = func(_ context.Context, rel, oldC, newC string) ReviewDecision { return ReviewAcceptAll }
	write()
	if reg.ApproveWrites {
		t.Fatal("accept-all must stop asking for the session")
	}
	if len(statuses) == 0 || statuses[0] != "reviewing change in VS Code…" {
		t.Fatalf("status not reported: %v", statuses)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(b) != "v" {
		t.Fatal("file not written after accept")
	}
}

// Esc during an editor review must abort the run: the dispatch context is
// cancelled, so a ReviewWrite still waiting on the editor has to return
// instead of holding the tool call open for its own (10-minute) deadline.
func TestReviewWriteHonoursDispatchContext(t *testing.T) {
	dir := t.TempDir()
	reg, _ := NewRegistry(dir, func(a, d string) bool { return true })
	reg.ApproveWrites = true
	entered := make(chan struct{})
	reg.ReviewWrite = func(ctx context.Context, rel, oldC, newC string) ReviewDecision {
		close(entered)
		<-ctx.Done() // the editor never answers; only cancellation frees us
		return ReviewUnavailable
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan Result, 1)
	go func() {
		done <- reg.Dispatch(ctx, provider.ToolCall{Name: "write_file", Arguments: `{"path":"a.txt","content":"v"}`})
	}()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("write_file did not return after the dispatch context was cancelled")
	}
}

// A review cancelled by the run's context (Esc during the editor diff) must
// reject the write outright, never fall through to the terminal prompt.
func TestCancelledReviewRejectsWithoutTerminalPrompt(t *testing.T) {
	dir := t.TempDir()
	prompted := 0
	reg, _ := NewRegistry(dir, func(a, d string) bool { prompted++; return true })
	reg.ApproveWrites = true
	reg.ReviewWrite = func(ctx context.Context, rel, oldC, newC string) ReviewDecision {
		<-ctx.Done()
		return ReviewUnavailable
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	res := reg.Dispatch(ctx, provider.ToolCall{Name: "write_file", Arguments: `{"path":"a.txt","content":"v"}`})
	if !res.IsError {
		t.Fatal("cancelled review must reject the write")
	}
	if prompted != 0 {
		t.Fatalf("terminal prompt shown %d times after a cancelled review", prompted)
	}
	if _, err := os.Stat(filepath.Join(dir, "a.txt")); err == nil {
		t.Fatal("file written after a cancelled review")
	}
}
