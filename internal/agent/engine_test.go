package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
	// A second exit replaces the line rather than stacking another copy.
	st.SetStep(2, "done")
	st.SetStep(1, "doing")
	h2, err := ag.WriteHandoff(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(h2, "Stopped at: ") != 1 || !strings.Contains(h2, "Stopped at: one") {
		t.Fatalf("stopped-at lines accumulated:\n%s", h2)
	}
	ag.Resume(ag.Session)
	if !strings.Contains(ag.History.System.Content, "doing: 1. one") {
		t.Fatal("resume did not rebuild the block")
	}
}

// A small model that double-encodes its arguments must still be observed:
// the engine has to see the arguments the tool actually ran with.
func TestDoubleEncodedArgumentsAreObserved(t *testing.T) {
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls <= 2 {
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{
				{ID: fmt.Sprint(calls), Name: "read_file", Arguments: `"{\"path\":\"a.go\"}"`}}}, nil
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}}
	ag, dir := newTestAgent(t, p, nil)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o644)
	st := withEngine(t, ag)
	var seen []string
	ag.Events.OnToolEnd = func(name string, res tools.Result) { seen = append(seen, res.Content) }
	if _, err := ag.Run(context.Background(), "look at a.go"); err != nil {
		t.Fatal(err)
	}
	if ds := st.Digests(); len(ds) != 1 || ds[0].Path != "a.go" {
		t.Fatalf("digests %+v", st.Digests())
	}
	if len(seen) != 2 || !strings.Contains(seen[1], "already read at turn 1 (unchanged)") {
		t.Fatalf("tool results: %q", seen)
	}
}

// Arguments no tool could run are not observed, and the call still reaches
// the registry so the model sees the real error.
func TestMalformedArgumentsSkipObservation(t *testing.T) {
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls == 1 {
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{
				{ID: "1", Name: "read_file", Arguments: `{not json`}}}, nil
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}}
	ag, dir := newTestAgent(t, p, nil)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n"), 0o644)
	st := withEngine(t, ag)
	var errored bool
	ag.Events.OnToolEnd = func(name string, res tools.Result) { errored = res.IsError }
	if _, err := ag.Run(context.Background(), "look at a.go"); err != nil {
		t.Fatal(err)
	}
	if !errored {
		t.Fatal("malformed arguments did not reach the registry")
	}
	if len(st.Digests()) != 0 {
		t.Fatalf("digested a call that never ran: %+v", st.Digests())
	}
}

func TestRunFullRefreshesTheTaskLineBetweenRequests(t *testing.T) {
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok"}, nil
	}}
	ag, _ := newTestAgent(t, p, nil)
	st := withEngine(t, ag)
	if _, _, err := ag.RunFull(context.Background(), "first thing"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ag.RunFull(context.Background(), "second thing"); err != nil {
		t.Fatal(err)
	}
	if st.Ledger().Task != "second thing" {
		t.Fatalf("task %q", st.Ledger().Task)
	}
	if !strings.Contains(ag.History.System.Content, "Task: second thing") {
		t.Fatalf("block did not follow the new request:\n%s", ag.History.System.Content)
	}
	// A plan in flight keeps its own task line.
	st.SetPlan("the plan", []string{"one", "two"})
	st.SetStep(1, "doing")
	if _, _, err := ag.RunFull(context.Background(), "a side question"); err != nil {
		t.Fatal(err)
	}
	if st.Ledger().Task != "the plan" {
		t.Fatalf("plan task line was overwritten: %q", st.Ledger().Task)
	}
}

func TestCompactUsesDigestsAndFeedsFileNotesBack(t *testing.T) {
	var summaryReq provider.ChatRequest
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		if strings.HasPrefix(req.Messages[0].Content, "Summarize this coding-agent") {
			summaryReq = req
			return &provider.ChatResponse{Content: "The task is x.\n\nfiles:\n- a.go — defines A\n"}, nil
		}
		return &provider.ChatResponse{Content: "ok"}, nil
	}}
	ag, dir := newTestAgent(t, p, nil)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\nfunc A() {}\n"), 0o644)
	st := withEngine(t, ag)
	st.NextTurn()
	st.Observe(engine.Event{Tool: "read_file", Args: map[string]any{"path": "a.go"}, Content: "    1\tpackage a\n    2\tfunc A() {}\n"})
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "read a.go"})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "1", Name: "read_file", Arguments: `{"path":"a.go"}`}}})
	ag.History.Add(provider.Message{Role: provider.RoleTool, ToolCallID: "1", Name: "read_file", Content: "    1\tpackage a\n    2\tfunc A() {}\n"})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "A is defined."})
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "next"})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "ok"})
	if err := ag.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	u := summaryReq.Messages[1].Content
	if !strings.Contains(u, "(read a.go lines 1–2; digested)") || strings.Contains(u, "package a") {
		t.Fatalf("summary transcript still carries the read:\n%s", u)
	}
	if !strings.Contains(u, "Working memory:") || !strings.Contains(summaryReq.Messages[0].Content, "Do not restate anything already in Working memory.") {
		t.Fatalf("summary request lacks the block or the instruction:\n%s\n%s", summaryReq.Messages[0].Content, u)
	}
	if st.Digests()[0].Note != "defines A" {
		t.Fatalf("file note not applied: %+v", st.Digests())
	}
	if strings.Contains(ag.History.Messages[0].Content, "files:") {
		t.Fatalf("files block not stripped from the stored summary:\n%s", ag.History.Messages[0].Content)
	}
}

// A backend that omits tool-call ids falls back to call_<index> per
// response, so the same id can name a read_file call in one turn and an
// unrelated tool in the next. The second call's result must not be
// mis-stubbed as a digested read of the first call's file.
func TestCompactDoesNotStubAReusedCallID(t *testing.T) {
	var summaryReq provider.ChatRequest
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		if strings.HasPrefix(req.Messages[0].Content, "Summarize this coding-agent") {
			summaryReq = req
			return &provider.ChatResponse{Content: "The task is x."}, nil
		}
		return &provider.ChatResponse{Content: "ok"}, nil
	}}
	ag, dir := newTestAgent(t, p, nil)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\nfunc A() {}\n"), 0o644)
	st := withEngine(t, ag)
	st.NextTurn()
	st.Observe(engine.Event{Tool: "read_file", Args: map[string]any{"path": "a.go"}, Content: "    1\tpackage a\n    2\tfunc A() {}\n"})
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "read a.go"})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "call_0", Name: "read_file", Arguments: `{"path":"a.go"}`}}})
	ag.History.Add(provider.Message{Role: provider.RoleTool, ToolCallID: "call_0", Name: "read_file", Content: "    1\tpackage a\n    2\tfunc A() {}\n"})
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "now run ls"})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "call_0", Name: "shell", Arguments: `{"cmd":"ls"}`}}})
	ag.History.Add(provider.Message{Role: provider.RoleTool, ToolCallID: "call_0", Name: "shell", Content: "a.go\n"})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "done"})
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "next"})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "ok"})
	if err := ag.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	u := summaryReq.Messages[1].Content
	if !strings.Contains(u, "(read a.go lines 1–2; digested)") {
		t.Fatalf("read_file result should still be stubbed as digested:\n%s", u)
	}
	if !strings.Contains(u, "[tool] a.go\n") {
		t.Fatalf("shell result reusing the read's call id must keep its full %%.600s stub, not the digest stub:\n%s", u)
	}
}

// A summary that is nothing but a files: block leaves no body to keep, so
// Compact must report it empty (the caller falls back to trimming) while
// still applying the file notes and leaving history untouched.
func TestCompactErrorsOnFilesOnlySummaryButStillAppliesNotes(t *testing.T) {
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		if strings.HasPrefix(req.Messages[0].Content, "Summarize this coding-agent") {
			return &provider.ChatResponse{Content: "files:\n- a.go — defines A\n"}, nil
		}
		return &provider.ChatResponse{Content: "ok"}, nil
	}}
	ag, dir := newTestAgent(t, p, nil)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\nfunc A() {}\n"), 0o644)
	st := withEngine(t, ag)
	st.NextTurn()
	st.Observe(engine.Event{Tool: "read_file", Args: map[string]any{"path": "a.go"}, Content: "    1\tpackage a\n    2\tfunc A() {}\n"})
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "read a.go"})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "A is defined."})
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "next"})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "ok"})
	before := append([]provider.Message(nil), ag.History.Messages...)
	err := ag.Compact(context.Background())
	if err == nil || !strings.Contains(err.Error(), "empty summary") {
		t.Fatalf("expected empty summary error, got %v", err)
	}
	if !reflect.DeepEqual(ag.History.Messages, before) {
		t.Fatalf("history changed on an errored compaction:\nbefore: %+v\nafter:  %+v", before, ag.History.Messages)
	}
	if st.Digests()[0].Note != "defines A" {
		t.Fatalf("file note not applied: %+v", st.Digests())
	}
}

func TestRunFullRecordsTheGitBaseline(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok"}, nil
	}}
	ag, dir := newTestAgent(t, p, nil)
	for _, c := range []string{
		"git init -q -b main", "git config user.email t@t.local", "git config user.name t",
		"sh -c 'echo hello > a.txt'", "git add -A", "git commit -qm initial",
		// Dirty before the task starts: this is the file the changes tool
		// must later report as already modified.
		"sh -c 'echo more >> a.txt'",
	} {
		if out, err := tools.RunShell(context.Background(), dir, c, 30*time.Second); err != nil {
			t.Fatalf("%s: %v %s", c, err, out)
		}
	}
	st := withEngine(t, ag)
	if _, _, err := ag.RunFull(context.Background(), "do something"); err != nil {
		t.Fatal(err)
	}
	b := st.Ledger().Baseline
	// Dirty is the porcelain text itself, not a hash of it.
	if len(b.Head) < 40 || !strings.Contains(b.Dirty, "a.txt") {
		t.Fatalf("baseline %+v", b)
	}
}

// stubTool is a no-op tool with a name, for prompt tests that key on names.
type stubTool string

func (s stubTool) Name() string                                     { return string(s) }
func (s stubTool) Description() string                              { return "stub" }
func (s stubTool) Schema() json.RawMessage                          { return json.RawMessage(`{"type":"object","properties":{}}`) }
func (s stubTool) Run(context.Context, map[string]any) tools.Result { return tools.Result{Content: "stub"} }

func TestPromptCarriesTaskGuidanceWhenTheToolExists(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	if strings.Contains(ag.History.System.Content, "record a plan with the task tool") {
		t.Fatal("guidance without the tool")
	}
	st := withEngine(t, ag)
	ag.Tools.AddTool(tools.NewTask(st))
	ag.RefreshSystem()
	if !strings.Contains(ag.History.System.Content, "record a plan with the task tool") || !strings.Contains(ag.History.System.Content, "Context is limited and does not survive compaction") {
		t.Fatal("guidance missing")
	}
	if strings.Contains(ag.History.System.Content, "call lookup") {
		t.Fatal("git guidance without the git tools")
	}
	ag.Tools.AddTool(stubTool("lookup")) // Task 6 adds the real one; the prompt keys on the name
	ag.RefreshSystem()
	sys := ag.History.System.Content
	if !strings.Contains(sys, "call lookup") || strings.Contains(sys, "call history") || strings.Contains(sys, "call changes") {
		t.Fatalf("minimal git guidance wrong:\n%s", sys)
	}
}
