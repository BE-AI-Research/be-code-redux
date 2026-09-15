package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/tools"
)

func withEngine(t *testing.T, ag *Agent) *engine.Store {
	t.Helper()
	st, err := engine.OpenAt(filepath.Join(t.TempDir(), "eng"), ag.Tools.Root, "s1", false, 4096)
	if err != nil {
		t.Fatal(err)
	}
	ag.SetEngine(st)
	return st
}

func TestReadsAreDigestedAndTheBlockReachesTheSystemPrompt(t *testing.T) {
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		switch calls {
		case 1:
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "1", Name: "read_file", Arguments: `{"path":"a.go"}`}}}, nil
		case 2:
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "2", Name: "read_file", Arguments: `{"path":"a.go"}`}}}, nil
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}}
	ag, dir := newTestAgent(t, p, nil)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o644)
	st := withEngine(t, ag)
	if _, err := ag.Run(context.Background(), "look at a.go"); err != nil {
		t.Fatal(err)
	}
	if len(st.Digests()) != 1 || st.Digests()[0].Path != "a.go" {
		t.Fatalf("digests %+v", st.Digests())
	}
	// The second read got the footer; the third request's system prompt has the block.
	second := p.reqs[2].Messages
	var toolMsg string
	for _, m := range second {
		if m.Role == provider.RoleTool && m.ToolCallID == "2" {
			toolMsg = m.Content
		}
	}
	if !strings.Contains(toolMsg, "already read at turn 1 (unchanged)") {
		t.Fatalf("no footer:\n%s", toolMsg)
	}
	sys := p.reqs[2].Messages[0].Content
	// read_file numbers the trailing empty line after the final newline, so
	// a three-line file reads as lines 1–4; the digest records what the
	// model was actually shown.
	if !strings.Contains(sys, "Working memory:") || !strings.Contains(sys, "a.go (lines 1–4)") {
		t.Fatalf("system prompt lacks the block:\n%s", sys)
	}
	if !strings.Contains(sys, "Task: look at a.go") {
		t.Fatalf("RunFull/Run did not seed the task line:\n%s", sys)
	}
	// Flushed at turn end.
	if _, err := os.Stat(filepath.Join(st.Dir(), "digests.json")); err != nil {
		t.Fatal("store not flushed at turn end")
	}
}

func TestRepeatedSearchIsServedFromTheCache(t *testing.T) {
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls <= 2 {
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "s", Name: "search", Arguments: `{"pattern":"func A"}`}}}, nil
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}}
	ag, dir := newTestAgent(t, p, nil)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o644)
	withEngine(t, ag)
	var seen []string
	ag.Events.OnToolEnd = func(name string, res tools.Result) { seen = append(seen, res.Content) }
	ag.Run(context.Background(), "find A")
	if len(seen) != 2 || strings.Contains(seen[0], "(cached") || !strings.HasSuffix(seen[1], "(cached; files unchanged)") {
		t.Fatalf("tool results: %q", seen)
	}
}

func TestRunFullRecordsBaselineAndExecutePlanSeedsSteps(t *testing.T) {
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok"}, nil
	}}
	ag, _ := newTestAgent(t, p, nil)
	st := withEngine(t, ag)
	if _, _, err := ag.ExecutePlan(context.Background(), "add a flag", "Plan\n1. parse\n2. wire\n"); err != nil {
		t.Fatal(err)
	}
	l := st.Ledger()
	if l.Task != "add a flag" || len(l.Steps) != 2 || l.Steps[1].Text != "wire" {
		t.Fatalf("ledger %+v", l)
	}
	// Not a git repo: baseline stays empty rather than erroring.
	if l.Baseline.Head != "" {
		t.Fatalf("baseline %+v", l.Baseline)
	}
}

func TestResumeRebuildsTheBlockAndHandoffCarriesStoppedAt(t *testing.T) {
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok"}, nil
	}}
	ag, _ := newTestAgent(t, p, nil)
	st := withEngine(t, ag)
	st.SetPlan("t", []string{"one", "two"})
	st.SetStep(2, "doing")
	ag.SetSession(store.NewSession("p", "m", ag.Tools.Root))
	ag.Run(context.Background(), "x")
	h, err := ag.WriteHandoff(context.Background(), false)
	if err != nil || !strings.Contains(h, "Stopped at: two") {
		t.Fatalf("handoff %v:\n%s", err, h)
	}
	ag.Resume(ag.Session)
	if !strings.Contains(ag.History.System.Content, "doing: 2. two") {
		t.Fatal("resume did not rebuild the block")
	}
}
