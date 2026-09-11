package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
	reg.ReviewWrite = func(rel, oldC, newC string) ReviewDecision { return ReviewReject }
	if res := write(); !res.IsError {
		t.Fatal("reject not honoured")
	}
	reg.ReviewWrite = func(rel, oldC, newC string) ReviewDecision { return ReviewAccept }
	if res := write(); res.IsError {
		t.Fatal(res.Content)
	}
	if prompted != 0 {
		t.Fatalf("TUI approver called %d times although the editor decided", prompted)
	}
	reg.ReviewWrite = func(rel, oldC, newC string) ReviewDecision { return ReviewUnavailable }
	write()
	if prompted != 1 {
		t.Fatal("fallback to Approve did not happen")
	}
	reg.ReviewWrite = func(rel, oldC, newC string) ReviewDecision { return ReviewAcceptAll }
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
