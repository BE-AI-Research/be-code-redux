package tools

import (
	"context"
	"os"
	"path/filepath"
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
