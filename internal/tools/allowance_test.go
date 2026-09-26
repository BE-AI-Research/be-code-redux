package tools

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/schedule"
)

type allowLog struct {
	mu      sync.Mutex
	actions []string
	answer  bool
}

func (l *allowLog) approve(action, detail string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.actions = append(l.actions, action)
	return l.answer
}

func (l *allowLog) asked() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.actions...)
}

func allowReg(t *testing.T, answer bool) (*Registry, *allowLog) {
	t.Helper()
	log := &allowLog{answer: answer}
	r, err := NewRegistry(t.TempDir(), log.approve)
	if err != nil {
		t.Fatal(err)
	}
	r.ApproveWrites = true
	return r, log
}

func dispatchCall(r *Registry, name, args string) Result {
	return r.Dispatch(context.Background(), provider.ToolCall{ID: "1", Name: name, Arguments: args})
}

func grants(t *testing.T, g ...string) schedule.Allowance {
	a, err := schedule.ParseAllowance(g)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAllowanceShellSkipsPrompt(t *testing.T) {
	r, log := allowReg(t, false)
	r.SetAllowance(grants(t, "shell: echo *"), time.Minute)
	res := dispatchCall(r, "shell", `{"command":"echo hi"}`)
	if res.IsError || !strings.Contains(res.Content, "hi") || len(log.asked()) != 0 {
		t.Fatalf("covered command ran unasked: %+v asked=%v", res, log.asked())
	}
	res = dispatchCall(r, "shell", `{"command":"ls"}`)
	if !res.IsError || strings.Join(log.asked(), ",") != "shell" {
		t.Fatalf("uncovered command asks: %+v asked=%v", res, log.asked())
	}
	refused, timedOut := r.ClearAllowance()
	if refused != "shell" || timedOut {
		t.Fatalf("first refusal recorded: %q %v", refused, timedOut)
	}
	// Cleared: the grant no longer applies.
	dispatchCall(r, "shell", `{"command":"echo hi"}`)
	if len(log.asked()) != 2 {
		t.Fatalf("after clear it asks again: %v", log.asked())
	}
}

func TestAllowanceCompoundStillAsks(t *testing.T) {
	r, log := allowReg(t, false)
	r.SetAllowance(grants(t, "shell: echo *"), time.Minute)
	dispatchCall(r, "shell", `{"command":"echo hi && rm -rf x"}`)
	if len(log.asked()) != 1 {
		t.Fatal("a compound command is never covered by a glob")
	}
}

func TestAllowanceDenyWins(t *testing.T) {
	r, log := allowReg(t, true)
	r.ShellDeny = []string{"rm *"}
	r.SetAllowance(grants(t, "shell: rm *"), time.Minute)
	res := dispatchCall(r, "shell", `{"command":"rm x"}`)
	if !res.IsError || len(log.asked()) != 0 {
		t.Fatalf("deny list refuses outright: %+v %v", res, log.asked())
	}
}

func TestAllowanceUntrustedWebStillAsks(t *testing.T) {
	r, log := allowReg(t, true)
	r.SetAllowance(grants(t, "shell: echo *"), time.Minute)
	r.MarkUntrustedWeb()
	dispatchCall(r, "shell", `{"command":"echo hi"}`)
	if strings.Join(log.asked(), ",") != "shell_after_web" {
		t.Fatalf("asked %v", log.asked())
	}
}

func TestAllowanceWrites(t *testing.T) {
	r, log := allowReg(t, false)
	r.SetAllowance(grants(t, "write: docs"), time.Minute)
	res := dispatchCall(r, "write_file", `{"path":"docs/a.md","content":"x"}`)
	if res.IsError || len(log.asked()) != 0 {
		t.Fatalf("covered write: %+v %v", res, log.asked())
	}
	if _, err := os.Stat(filepath.Join(r.Root, "docs", "a.md")); err != nil {
		t.Fatal(err)
	}
	res = dispatchCall(r, "write_file", `{"path":"src/b.go","content":"x"}`)
	if !res.IsError || strings.Join(log.asked(), ",") != "file_write" {
		t.Fatalf("uncovered write asks: %+v %v", res, log.asked())
	}
}

func TestAllowanceNotInherited(t *testing.T) {
	r, log := allowReg(t, false)
	r.SetAllowance(grants(t, "shell: echo *", "write: ."), time.Minute)
	sub := r.Subset("shell", "write_file")
	sub.ApproveWrites = true
	dispatchCall(sub, "shell", `{"command":"echo hi"}`)
	scoped := r.Scoped([]string{"docs"}, []string{"go vet ./..."}, "s1")
	dispatchCall(scoped, "write_file", `{"path":"docs/a.md","content":"x"}`)
	if got := strings.Join(log.asked(), ","); got != "shell,file_write" {
		t.Fatalf("Subset and Scoped must ask: %q", got)
	}
}

func TestAllowanceAskTimeoutWithdraws(t *testing.T) {
	r, _ := allowReg(t, true)
	r.ApproveCtx = func(ctx context.Context, action, detail string) bool {
		<-ctx.Done()
		return false
	}
	r.SetAllowance(nil, 20*time.Millisecond)
	start := time.Now()
	res := dispatchCall(r, "shell", `{"command":"ls"}`)
	if !res.IsError || time.Since(start) > 2*time.Second {
		t.Fatalf("refused after the deadline: %+v", res)
	}
	if refused, timedOut := r.ClearAllowance(); refused != "shell" || !timedOut {
		t.Fatalf("%q %v", refused, timedOut)
	}
}

// TestAllowanceSkipsReviewWrite is fix round 1, item 1: ReviewWrite has no
// deadline of its own, and a rejection there was never recorded against the
// allowance's ask_timeout. During a fired turn an uncovered write must skip
// straight to r.ask, which does have one.
func TestAllowanceSkipsReviewWrite(t *testing.T) {
	r, log := allowReg(t, false)
	reviewCalled := false
	r.ReviewWrite = func(ctx context.Context, rel, oldC, newC string) ReviewDecision {
		reviewCalled = true
		return ReviewAccept
	}
	r.SetAllowance(nil, time.Minute)
	res := dispatchCall(r, "write_file", `{"path":"src/b.go","content":"x"}`)
	if reviewCalled {
		t.Fatal("ReviewWrite was called for an uncovered write during a fired turn")
	}
	if !res.IsError || strings.Join(log.asked(), ",") != "file_write" {
		t.Fatalf("expected a file_write ask through Approve: %+v asked=%v", res, log.asked())
	}
	if refused, _ := r.ClearAllowance(); refused != "file_write" {
		t.Fatalf("refusal recorded: %q", refused)
	}
}

// TestAllowanceReviewWriteUsedWithoutAllowance pins the "outside a fired turn
// nothing changes" half of the same ruling.
func TestAllowanceReviewWriteUsedWithoutAllowance(t *testing.T) {
	r, _ := allowReg(t, true)
	reviewCalled := false
	r.ReviewWrite = func(ctx context.Context, rel, oldC, newC string) ReviewDecision {
		reviewCalled = true
		return ReviewAccept
	}
	res := dispatchCall(r, "write_file", `{"path":"src/b.go","content":"x"}`)
	if !reviewCalled || res.IsError {
		t.Fatalf("ReviewWrite must still run without an allowance: %+v reviewCalled=%v", res, reviewCalled)
	}
}

// TestSubsetSeesParentsUntrustedWeb is fix round 1, item 2: the browser tool
// that sets untrustedWeb is never itself exposed through Subset, so a
// Subset("shell") could otherwise never learn the parent request read an
// untrusted page and would skip shell_after_web (browser spec §3.6).
func TestSubsetSeesParentsUntrustedWeb(t *testing.T) {
	r, log := allowReg(t, true)
	r.MarkUntrustedWeb()
	sub := r.Subset("shell")
	dispatchCall(sub, "shell", `{"command":"echo hi"}`)
	if strings.Join(log.asked(), ",") != "shell_after_web" {
		t.Fatalf("asked %v", log.asked())
	}
}

// TestAllowanceWriteThroughSymlinkEscapeStillAsks is fix round 1, item 4:
// WriteAllowed matched the textual path only, so a symlink inside a granted
// directory could point outside the workspace and land an unattended,
// never-diffed write there.
func TestAllowanceWriteThroughSymlinkEscapeStillAsks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevated privileges on windows")
	}
	r, log := allowReg(t, true)
	r.SetAllowance(grants(t, "write: docs"), time.Minute)
	if err := os.MkdirAll(filepath.Join(r.Root, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(r.Root, "docs", "out")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	res := dispatchCall(r, "write_file", `{"path":"docs/out/x.txt","content":"x"}`)
	if res.IsError {
		t.Fatalf("the approver answers yes, so the write should still land: %+v", res)
	}
	if strings.Join(log.asked(), ",") != "file_write" {
		t.Fatalf("a write through a symlink escaping the grant must still ask: asked=%v", log.asked())
	}
	if _, err := os.Stat(filepath.Join(outside, "x.txt")); err != nil {
		t.Fatalf("write did not land at the resolved (outside) target: %v", err)
	}
}

func TestNoAllowanceBehaviourUnchanged(t *testing.T) {
	r, log := allowReg(t, true)
	dispatchCall(r, "shell", `{"command":"echo hi"}`)
	if strings.Join(log.asked(), ",") != "shell" {
		t.Fatalf("asked %v", log.asked())
	}
	if refused, _ := r.ClearAllowance(); refused != "" {
		t.Fatal("nothing to clear")
	}
}
