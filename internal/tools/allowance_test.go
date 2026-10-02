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

// TestFiredTurnIgnoresSessionWriteShortcut is final review C1: "accept all"
// (ApproveWrites=false) is a session-level shortcut a person gave while
// watching. It must not let an unattended fired turn write outside its
// allowance: the uncovered write asks through the deadline prompt.
func TestFiredTurnIgnoresSessionWriteShortcut(t *testing.T) {
	r, log := allowReg(t, false)
	r.ApproveWrites = false
	r.SetAllowance(nil, time.Minute)
	res := dispatchCall(r, "write_file", `{"path":"src/b.go","content":"x"}`)
	if !res.IsError || strings.Join(log.asked(), ",") != "file_write" {
		t.Fatalf("an uncovered write during a fired turn asks: %+v asked=%v", res, log.asked())
	}
	if _, err := os.Stat(filepath.Join(r.Root, "src", "b.go")); err == nil {
		t.Fatal("the refused write landed")
	}
	r.ClearAllowance()
	res = dispatchCall(r, "write_file", `{"path":"src/b.go","content":"x"}`)
	if res.IsError || len(log.asked()) != 1 {
		t.Fatalf("outside a fired turn accept-all still applies: %+v asked=%v", res, log.asked())
	}
}

// TestFiredAskIsMarked: the UIs' approvers skip their session-level
// shortcuts (AutoApproveShell, !ApproveFileWrites, AutoApproveBrowser) for a
// question a fired turn raised, which they learn from the context (C1).
func TestFiredAskIsMarked(t *testing.T) {
	r, _ := allowReg(t, false)
	var marked []bool
	r.ApproveCtx = func(ctx context.Context, action, detail string) bool {
		marked = append(marked, FiredAsk(ctx))
		return false
	}
	r.SetAllowance(nil, 0) // no deadline: still through ApproveCtx, still marked
	dispatchCall(r, "shell", `{"command":"ls"}`)
	r.ClearAllowance()
	if len(marked) != 1 || !marked[0] {
		t.Fatalf("the fired ask goes through ApproveCtx marked: %v", marked)
	}
	if FiredAsk(context.Background()) {
		t.Fatal("an ordinary context is not marked")
	}
}

// TestFiredTurnWithNoApproverRefuses: with nobody to ask, an uncovered
// action of a fired turn is refused, never run.
func TestFiredTurnWithNoApproverRefuses(t *testing.T) {
	r, err := NewRegistry(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.SetAllowance(nil, time.Minute)
	if res := dispatchCall(r, "shell", `{"command":"echo hi"}`); !res.IsError {
		t.Fatalf("refused: %+v", res)
	}
	if res := dispatchCall(r, "write_file", `{"path":"a.txt","content":"x"}`); !res.IsError {
		t.Fatalf("refused: %+v", res)
	}
}

// TestAllowanceNeverCoversSchedulesOrDotdir is final review I3(c): no grant
// covers .be-code/schedules.md (a fired turn could otherwise re-activate a
// paused schedule, or widen its own allowance) nor anything under the
// BE-Code dotdir.
func TestAllowanceNeverCoversSchedulesOrDotdir(t *testing.T) {
	r, log := allowReg(t, false)
	home := filepath.Join(r.Root, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	r.SetAllowance(grants(t, "write: ."), time.Minute)
	for _, p := range []string{".be-code/schedules.md", "home/.be-code/config.json", "home/.be-code/engine/x/schedules.json"} {
		res := dispatchCall(r, "write_file", `{"path":"`+p+`","content":"x"}`)
		if !res.IsError {
			t.Fatalf("%s: a write: . grant must not cover it: %+v", p, res)
		}
	}
	if got := strings.Join(log.asked(), ","); got != "file_write,file_write,file_write" {
		t.Fatalf("each asked: %q", got)
	}
	if res := dispatchCall(r, "write_file", `{"path":".be-code/notes.md","content":"x"}`); res.IsError {
		t.Fatalf("the rest of the project .be-code is still under the grant: %+v", res)
	}
}

// --- schedule settings (2026-10-01) -----------------------------------------

// TestInheritSessionApprovalsLiftsAcceptAll: with
// schedules.inherit_session_approvals the fired turn takes the session's
// accept-all again; without it (TestFiredTurnIgnoresSessionWriteShortcut)
// it asks.
func TestInheritSessionApprovalsLiftsAcceptAll(t *testing.T) {
	r, log := allowReg(t, false)
	r.ApproveWrites = false
	r.SetFiredPolicy(nil, time.Minute, true)
	res := dispatchCall(r, "write_file", `{"path":"src/b.go","content":"x"}`)
	if res.IsError || len(log.asked()) != 0 {
		t.Fatalf("accept-all applies to a fired turn that inherits: %+v asked=%v", res, log.asked())
	}
	if refused, _ := r.ClearAllowance(); refused != "" {
		t.Fatalf("nothing refused: %q", refused)
	}
	// Off again: the same write asks.
	r.SetFiredPolicy(nil, time.Minute, false)
	if res := dispatchCall(r, "write_file", `{"path":"src/c.go","content":"x"}`); !res.IsError || strings.Join(log.asked(), ",") != "file_write" {
		t.Fatalf("without inherit it asks: %+v asked=%v", res, log.asked())
	}
	r.ClearAllowance()
}

// TestInheritedAskIsStillMarked: inheriting never drops the fired marker or
// the deadline — it adds a second mark the UIs read through SessionShortcuts.
func TestInheritedAskIsStillMarked(t *testing.T) {
	for _, inherit := range []bool{false, true} {
		r, _ := allowReg(t, false)
		var fired, shortcuts []bool
		var deadline []bool
		r.ApproveCtx = func(ctx context.Context, action, detail string) bool {
			fired = append(fired, FiredAsk(ctx))
			shortcuts = append(shortcuts, SessionShortcuts(ctx))
			_, ok := ctx.Deadline()
			deadline = append(deadline, ok)
			return false
		}
		r.SetFiredPolicy(nil, time.Minute, inherit)
		dispatchCall(r, "shell", `{"command":"ls"}`)
		r.ClearAllowance()
		if len(fired) != 1 || !fired[0] || shortcuts[0] != inherit || !deadline[0] {
			t.Fatalf("inherit=%v: fired=%v shortcuts=%v deadline=%v", inherit, fired, shortcuts, deadline)
		}
	}
	if !SessionShortcuts(context.Background()) {
		t.Fatal("an ordinary question takes the session's shortcuts")
	}
}

// TestInheritWithNoApproverStillRefuses: inheriting the session's approvals
// is not "no approver means yes".
func TestInheritWithNoApproverStillRefuses(t *testing.T) {
	r, err := NewRegistry(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.SetFiredPolicy(nil, time.Minute, true)
	defer r.ClearAllowance()
	if res := dispatchCall(r, "shell", `{"command":"echo hi"}`); !res.IsError {
		t.Fatalf("refused: %+v", res)
	}
	if res := dispatchCall(r, "write_file", `{"path":"a.txt","content":"x"}`); !res.IsError {
		t.Fatalf("refused: %+v", res)
	}
}

// TestInheritKeepsDenyAndProtectedPaths: a denied command never runs, and
// a covered write grant never reaches schedules.md, with inherit on.
func TestInheritKeepsDenyAndProtectedPaths(t *testing.T) {
	r, log := allowReg(t, true)
	home := filepath.Join(r.Root, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	r.ShellDeny = []string{"rm *"}
	r.ApproveWrites = true
	r.SetFiredPolicy(grants(t, "write: .", "shell: rm *"), time.Minute, true)
	defer r.ClearAllowance()
	if res := dispatchCall(r, "shell", `{"command":"rm -rf x"}`); !res.IsError || len(log.asked()) != 0 {
		t.Fatalf("deny wins without asking: %+v asked=%v", res, log.asked())
	}
	dispatchCall(r, "write_file", `{"path":".be-code/schedules.md","content":"x"}`)
	if strings.Join(log.asked(), ",") != "file_write" {
		t.Fatalf("schedules.md is never covered by a grant: asked=%v", log.asked())
	}
}

func TestGrantsCovered(t *testing.T) {
	standing := grants(t, "shell: go test*", "shell: make lint", "write: docs", "browser: *.example.com", "browser: localhost")
	cases := []struct {
		req  []string
		want bool
	}{
		{nil, true},
		{[]string{"shell: go test*"}, true},
		{[]string{"shell: go test ./..."}, true},
		{[]string{"shell: go test ./... -run X*"}, true},
		{[]string{"shell: make lint"}, true},
		{[]string{"shell: make *"}, false},
		{[]string{"shell: go test ./... && rm -rf x"}, false},
		{[]string{"shell: go vet ./..."}, false},
		{[]string{"write: docs"}, true},
		{[]string{"write: docs/api"}, true},
		{[]string{"write: ."}, false},
		{[]string{"write: docsx"}, false},
		{[]string{"write: src"}, false},
		{[]string{"browser: *.example.com"}, true},
		{[]string{"browser: api.example.com"}, true},
		{[]string{"browser: *.api.example.com"}, true},
		{[]string{"browser: example.com"}, false},
		{[]string{"browser: localhost"}, true},
		{[]string{"browser: *host"}, false},
		{[]string{"shell: go test ./...", "write: src"}, false},
		{[]string{"shell: go test ./...", "write: docs/x", "browser: a.example.com"}, true},
	}
	for _, c := range cases {
		if got := GrantsCovered(grants(t, c.req...), standing); got != c.want {
			t.Errorf("%v: covered=%v, want %v", c.req, got, c.want)
		}
	}
	// A standing host glob with '?' or a class only covers a plain host: a
	// requested glob could otherwise be wider than it (`?x.com` matches the
	// text `*x.com` but not abcx.com).
	odd := grants(t, "browser: ?x.com")
	if GrantsCovered(grants(t, "browser: *x.com"), odd) || !GrantsCovered(grants(t, "browser: ax.com"), odd) {
		t.Fatal("a '?' standing pattern covers plain hosts only")
	}
	if GrantsCovered(grants(t, "write: docs"), nil) {
		t.Fatal("nothing standing covers no grant")
	}
}

// TestInheritedAcceptAllNeverCoversProtectedPaths: what no grant covers,
// an inheriting fired turn's accept-all does not cover either — the write
// asks, and the question withholds the session's shortcuts.
func TestInheritedAcceptAllNeverCoversProtectedPaths(t *testing.T) {
	r, _ := allowReg(t, false)
	home := filepath.Join(r.Root, "home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	r.ApproveWrites = false
	var shortcuts []bool
	r.ApproveCtx = func(ctx context.Context, action, detail string) bool {
		shortcuts = append(shortcuts, SessionShortcuts(ctx))
		return false
	}
	r.SetFiredPolicy(nil, time.Minute, true)
	defer r.ClearAllowance()
	for _, p := range []string{".be-code/schedules.md", "home/.be-code/config.json"} {
		if res := dispatchCall(r, "write_file", `{"path":"`+p+`","content":"x"}`); !res.IsError {
			t.Fatalf("%s: written unasked: %+v", p, res)
		}
	}
	if len(shortcuts) != 2 || shortcuts[0] || shortcuts[1] {
		t.Fatalf("both asked with the shortcuts withheld: %v", shortcuts)
	}
	if res := dispatchCall(r, "write_file", `{"path":"src/ok.go","content":"x"}`); res.IsError || len(shortcuts) != 2 {
		t.Fatalf("an ordinary path takes accept-all: %+v %v", res, shortcuts)
	}
}
