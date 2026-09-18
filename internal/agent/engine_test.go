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

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/tools"
)

func withEngine(t *testing.T, ag *Agent) *engine.Store {
	t.Helper()
	st, err := engine.OpenAt(filepath.Join(t.TempDir(), "eng"), ag.Tools.Root, "s1", false,
		engine.Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768})
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
	if fs := filesOf(st); len(fs) != 1 || fs[0].Path != "a.go" {
		t.Fatalf("files %+v", fs)
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
	if !strings.Contains(sys, "look at a.go — todo") {
		t.Fatalf("RunFull/Run did not seed the task line:\n%s", sys)
	}
	// Flushed at turn end.
	if _, err := os.Stat(filepath.Join(st.Dir(), "state.json")); err != nil {
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
	tr := st.Tree()
	if len(tr.Roots) != 1 || tr.Roots[0].Text != "add a flag" ||
		len(tr.Roots[0].Children) != 2 || tr.Roots[0].Children[1].Text != "wire" {
		t.Fatalf("tree %s", st.TreeText())
	}
	// Not a git repo: baseline stays empty rather than erroring.
	if b := st.Baseline(); b.Head != "" {
		t.Fatalf("baseline %+v", b)
	}
}

func TestResumeRebuildsTheBlockAndHandoffCarriesStoppedAt(t *testing.T) {
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok"}, nil
	}}
	ag, _ := newTestAgent(t, p, nil)
	st := withEngine(t, ag)
	id := st.Plan("t", []string{"one", "two"})
	st.SetStatusText(id+".2", "doing", "")
	ag.SetSession(store.NewSession("p", "m", ag.Tools.Root))
	ag.Run(context.Background(), "x")
	h, err := ag.WriteHandoff(context.Background(), false)
	if err != nil || !strings.Contains(h, "Stopped at: two") {
		t.Fatalf("handoff %v:\n%s", err, h)
	}
	// A second exit replaces the line rather than stacking another copy.
	st.SetStatusText(id+".2", "done", "")
	st.SetStatusText(id+".1", "doing", "")
	h2, err := ag.WriteHandoff(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(h2, "Stopped at: ") != 1 || !strings.Contains(h2, "Stopped at: one") {
		t.Fatalf("stopped-at lines accumulated:\n%s", h2)
	}
	ag.Resume(ag.Session)
	if !strings.Contains(ag.History.System.Content, "one — doing") {
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
	if fs := filesOf(st); len(fs) != 1 || fs[0].Path != "a.go" {
		t.Fatalf("files %+v", fs)
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
	if fs := filesOf(st); len(fs) != 0 {
		t.Fatalf("digested a call that never ran: %+v", fs)
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
	if got := activeTask(st); got != "second thing" {
		t.Fatalf("task %q", got)
	}
	if !strings.Contains(ag.History.System.Content, "second thing — todo") {
		t.Fatalf("block did not follow the new request:\n%s", ag.History.System.Content)
	}
	// A plan in flight keeps its own task line.
	planID := st.Plan("the plan", []string{"one", "two"})
	st.SetStatusText(planID+".1", "doing", "")
	if _, _, err := ag.RunFull(context.Background(), "a side question"); err != nil {
		t.Fatal(err)
	}
	if got := activeTask(st); got != "the plan" {
		t.Fatalf("plan task line was overwritten: %q", got)
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
	// The working-memory block folded into this same request carries the
	// doing node's raw buffer verbatim, on purpose (that block is never
	// summarised or condensed — it is the lossless part). "package a"
	// legitimately appears there. What must still be true is that the
	// *transcript* section — the part compaction actually rewrites — has
	// the read collapsed rather than repeating the file's content a second
	// time.
	transcript := u
	if i := strings.Index(u, "Transcript (most recent last):"); i >= 0 {
		transcript = u[i:]
	}
	if !strings.Contains(transcript, "(read a.go lines 1–2; digested)") || strings.Contains(transcript, "package a") {
		t.Fatalf("summary transcript still carries the read:\n%s", transcript)
	}
	if !strings.Contains(u, "Working memory:") || !strings.Contains(summaryReq.Messages[0].Content, "Do not restate anything already in Working memory.") {
		t.Fatalf("summary request lacks the block or the instruction:\n%s\n%s", summaryReq.Messages[0].Content, u)
	}
	if filesOf(st)[0].Note != "defines A" {
		t.Fatalf("file note not applied: %+v", filesOf(st))
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

// A summary that is nothing but a files: block leaves no body to keep. It
// used to be reported as an error so the caller fell back to trimming;
// since the tree became the thing that carries state across a compaction,
// it is instead the "continue from the task record" path — with the file
// notes still applied and the newest exchange still in place.
func TestCompactContinuesFromTheRecordOnAFilesOnlySummary(t *testing.T) {
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
	st.EnsureRoot("read a.go")
	st.Observe(engine.Event{Tool: "read_file", Args: map[string]any{"path": "a.go"}, Content: "    1\tpackage a\n    2\tfunc A() {}\n"})
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "read a.go"})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "A is defined."})
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "next"})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "ok"})
	tail := append([]provider.Message(nil), ag.History.Messages[len(ag.History.Messages)-2:]...)
	var notices []string
	ag.Events.OnNotice = func(m string) { notices = append(notices, m) }
	if err := ag.Compact(context.Background()); err != nil {
		t.Fatalf("a files-only summary must not fail compaction: %v", err)
	}
	if !containsAny(notices, "continuing from the task record") {
		t.Fatalf("no notice explaining the missing summary: %v", notices)
	}
	if n := len(ag.History.Messages); n != len(tail)+1 {
		t.Fatalf("history should be the note plus the newest exchange: %+v", ag.History.Messages)
	}
	if !strings.HasPrefix(ag.History.Messages[0].Content, SummaryPrefix) ||
		!strings.Contains(ag.History.Messages[0].Content, "Working memory") {
		t.Fatalf("the stand-in note does not point at the record: %q", ag.History.Messages[0].Content)
	}
	if !reflect.DeepEqual(ag.History.Messages[1:], tail) {
		t.Fatalf("the newest exchange was not kept: %+v", ag.History.Messages)
	}
	if !strings.Contains(ag.History.System.Content, "read a.go") {
		t.Fatalf("the tree did not survive compaction:\n%s", ag.History.System.Content)
	}
	if filesOf(st)[0].Note != "defines A" {
		t.Fatalf("file note not applied: %+v", filesOf(st))
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
	b := st.Baseline()
	// Dirty is the porcelain text itself, not a hash of it.
	if len(b.Head) < 40 || !strings.Contains(b.Dirty, "a.txt") {
		t.Fatalf("baseline %+v", b)
	}
}

// stubTool is a no-op tool with a name, for prompt tests that key on names.
type stubTool string

func (s stubTool) Name() string        { return string(s) }
func (s stubTool) Description() string { return "stub" }
func (s stubTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (s stubTool) Run(context.Context, map[string]any) tools.Result {
	return tools.Result{Content: "stub"}
}

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

// Plan mode carries no Working memory block, so its prompt must not tell
// the model about one: the compat tail spliced out of BuildSystemPrompt
// used to drag the engine guidance along with the tool catalog.
func TestPlanPromptDoesNotPromiseWorkingMemory(t *testing.T) {
	for _, mode := range []string{"auto", "always"} {
		ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) {
			c.CompatToolCalls = mode
		})
		st := withEngine(t, ag)
		ag.Tools.AddTool(tools.NewTask(st))
		for _, gt := range tools.NewGitTools(ag.Tools, nil, false) {
			ag.Tools.AddTool(gt)
		}
		sys := ag.planAgent().systemOverride
		if !strings.Contains(sys, "Tool calling format") {
			t.Fatalf("%s: compat tail missing, test proves nothing:\n%.300s", mode, sys)
		}
		if strings.Contains(sys, "Context is limited and does not survive compaction") {
			t.Fatalf("%s: plan prompt describes a Working memory block it does not carry", mode)
		}
	}
}

func TestCompactEmptySummaryErrorCarriesTheBackendDiagnostics(t *testing.T) {
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		if strings.HasPrefix(req.Messages[0].Content, "Summarize this coding-agent") {
			return &provider.ChatResponse{Content: "", Reasoning: strings.Repeat("r", 12), FinishReason: "length",
				Usage: provider.Usage{PromptTokens: 5000, CompletionTokens: 0}}, nil
		}
		return &provider.ChatResponse{Content: "ok"}, nil
	}}
	ag, _ := newTestAgent(t, p, nil)
	for i := 0; i < 3; i++ {
		ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "q"})
		ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "a"})
	}
	err := ag.Compact(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"empty summary", "finish=length", "5000 prompt tokens", "0 completion tokens", "reasoning 12 chars", "raw reply 0 chars"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err.Error(), want)
		}
	}
}

// Reasoning that exhausts the window is retried once with less of it: the
// retry carries reasoning_effort=low whatever the config asked for, and the
// configured effort is what every ordinary call sends.
func TestReasoningExhaustionRetriesWithLowEffort(t *testing.T) {
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls == 1 {
			return &provider.ChatResponse{Content: "", Reasoning: strings.Repeat("think ", 2000), FinishReason: "length"}, nil
		}
		return &provider.ChatResponse{Content: "done", FinishReason: "stop"}, nil
	}}
	ag, _ := newTestAgent(t, p, func(c *config.Config) { c.ReasoningEffort = "medium"; c.CompactWithModel = false })
	var notices []string
	ag.Events.OnNotice = func(s string) { notices = append(notices, s) }
	out, err := ag.Run(context.Background(), "do the thing")
	if err != nil || out != "done" {
		t.Fatalf("run: %q %v", out, err)
	}
	if len(p.reqs) != 2 || p.reqs[0].ReasoningEffort != "medium" || p.reqs[1].ReasoningEffort != "low" {
		t.Fatalf("efforts: %v", []string{p.reqs[0].ReasoningEffort, p.reqs[len(p.reqs)-1].ReasoningEffort})
	}
	if len(notices) == 0 || !strings.Contains(notices[len(notices)-1], "reasoning_effort=low") {
		t.Fatalf("notices: %v", notices)
	}
}

// Effort follows the room left in the window: configured while the prompt
// is small, one level down once it fills more than half the limit.
func TestEffortStepsDownWhenThePromptFillsHalfTheWindow(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 4000 })
	ag.History.Budget, ag.History.Reserve = 4000, 0
	for _, tc := range []struct{ in, small, big string }{{"high", "high", "medium"}, {"medium", "medium", "low"}, {"low", "low", "low"}, {"", "", ""}} {
		ag.History.Messages = nil
		if got := ag.effortFor(tc.in); got != tc.small {
			t.Fatalf("%q small prompt: got %q want %q", tc.in, got, tc.small)
		}
		ag.History.Add(provider.Message{Role: provider.RoleUser, Content: strings.Repeat("x", 9000)}) // ~3000 tokens > half of 4000
		if got := ag.effortFor(tc.in); got != tc.big {
			t.Fatalf("%q big prompt: got %q want %q", tc.in, got, tc.big)
		}
	}
}

// ---- helpers for the task-record tests -------------------------------------

// filesOf is every file reference in the record, in walk order. It replaces
// the flat Digests() projection the 0.10.0 compat layer used to offer.
func filesOf(st *engine.Store) []engine.FileRef {
	var out []engine.FileRef
	tr := st.Tree()
	tr.Walk(func(n *engine.Node, _ int) { out = append(out, n.Evidence.Files...) })
	return out
}

// activeTask is the text of the task in flight: the newest root that is not
// wholly finished, the same definition the block itself uses.
func activeTask(st *engine.Store) string {
	tr := st.Tree()
	for i := len(tr.Roots) - 1; i >= 0; i-- {
		if !tr.Terminal(tr.Roots[i]) {
			return tr.Roots[i].Text
		}
	}
	return ""
}

func containsAny(lines []string, want string) bool {
	for _, l := range lines {
		if strings.Contains(l, want) {
			return true
		}
	}
	return false
}

func agentWithEngine(t *testing.T) (*Agent, *engine.Store) {
	t.Helper()
	ag, _ := newTestAgent(t, &funcProvider{fn: func(provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok"}, nil
	}}, nil)
	return ag, withEngine(t, ag)
}

// emptySummaryProvider answers every request with nothing at all — the
// failure the validation VM saw on every compaction after the first.
func emptySummaryProvider() *funcProvider {
	return &funcProvider{fn: func(provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "", FinishReason: "stop"}, nil
	}}
}

// TestEvidenceIsRecordedAgainstTheDoingNode: the dispatch hook files tool
// results where the report will later find them.
func TestEvidenceIsRecordedAgainstTheDoingNode(t *testing.T) {
	ag, st := agentWithEngine(t)
	id := st.Plan("fix the parser", []string{"find it"})
	if err := st.SetStatusText(id+".1", "doing", ""); err != nil {
		t.Fatal(err)
	}
	ag.dispatch(context.Background(), provider.ToolCall{
		ID: "c1", Name: "shell", Arguments: `{"command":"echo hi"}`,
	})
	n := st.Tree().Find(id + ".1")
	if n == nil || len(n.Evidence.Raw) == 0 {
		t.Fatalf("nothing recorded against the doing node: %s", st.TreeText())
	}
	if err := st.SetStatusText(id+".1", "done", ""); err != nil {
		t.Fatal(err)
	}
	got := st.Tree().Find(id + ".1")
	if len(got.Evidence.Cmds) != 1 || got.Evidence.Cmds[0].Cmd != "echo hi" {
		t.Fatalf("distilled cmds: %+v", got.Evidence.Cmds)
	}
}

// TestEvidenceRecordedBeforeAStepIsAdoptedByIt: the model works first and
// says what it was doing second, which is the common order. What it did
// must end up on the step it names, not in a permanent unfiled bucket.
func TestEvidenceRecordedBeforeAStepIsAdoptedByIt(t *testing.T) {
	ag, st := agentWithEngine(t)
	id := st.Plan("fix the parser", []string{"find it"})
	// Nothing is doing yet.
	ag.dispatch(context.Background(), provider.ToolCall{
		ID: "c1", Name: "shell", Arguments: `{"command":"echo hi"}`,
	})
	if err := st.SetStatusText(id+".1", "doing", ""); err != nil {
		t.Fatal(err)
	}
	n := st.Tree().Find(id + ".1")
	if n == nil || len(n.Evidence.Raw) != 1 || n.Evidence.Raw[0].Args != "echo hi" {
		t.Fatalf("evidence was not adopted by the step: %s", st.TreeText())
	}
	if strings.Contains(st.TreeText(), "unfiled") {
		t.Fatalf("the unfiled node survived adoption:\n%s", st.TreeText())
	}
}

// TestCompactionSurvivesAnEmptySummary: the failure seen on the validation
// VM. An empty summary must leave the session working from the tree, with a
// notice, rather than falling back to blind trimming.
func TestCompactionSurvivesAnEmptySummary(t *testing.T) {
	ag, st := agentWithEngine(t)
	ag.Provider = emptySummaryProvider()
	var notices []string
	ag.Events.OnNotice = func(m string) { notices = append(notices, m) }
	id := st.Plan("fix the parser", []string{"find it"})
	st.SetStatusText(id+".1", "doing", "")
	st.Observe(engine.Event{Tool: "shell", Args: map[string]any{"command": "go test ./..."},
		Content: "FAIL: TestLex\n", IsError: true})
	for i := 0; i < 3; i++ {
		ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "q"})
		ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "a"})
	}

	if err := ag.Compact(context.Background()); err != nil {
		t.Fatalf("an empty summary must not fail compaction: %v", err)
	}
	sys := ag.History.System.Content
	if !strings.Contains(sys, "fix the parser") {
		t.Fatalf("the tree did not survive compaction:\n%s", sys)
	}
	if !containsAny(notices, "continuing from the task record") {
		t.Fatalf("no notice explaining the empty summary: %v", notices)
	}
	if got := notices[len(notices)-1]; got != "compaction: the model returned no summary; continuing from the task record" {
		t.Fatalf("notice wording: %q", got)
	}
}

// TestCompactionWithoutARecordStillReportsTheFailure: the new path needs a
// tree to continue from. Without an engine there is nothing to fall back
// on, so the old contract stands and the caller trims.
func TestCompactionWithoutARecordStillReportsTheFailure(t *testing.T) {
	ag, _ := newTestAgent(t, emptySummaryProvider(), nil)
	for i := 0; i < 3; i++ {
		ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "q"})
		ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "a"})
	}
	if err := ag.Compact(context.Background()); err == nil || !strings.Contains(err.Error(), "empty summary") {
		t.Fatalf("expected the empty-summary error, got %v", err)
	}
}

// TestTheEngineCannotFailATurn: advisory discipline, which every task in
// this plan inherits. A panicking store, a hanging one and one that returns
// nonsense each leave the turn working.
func TestTheEngineCannotFailATurn(t *testing.T) {
	for _, bad := range []string{"panic", "hang", "garbage"} {
		t.Run(bad, func(t *testing.T) {
			ag := agentWithBadEngine(t, bad)
			var notices []string
			ag.Events.OnNotice = func(m string) { notices = append(notices, m) }
			res := ag.dispatch(context.Background(), provider.ToolCall{
				ID: "c1", Name: "shell", Arguments: `{"command":"echo hi"}`,
			})
			if res.IsError {
				t.Fatalf("a %s engine failed the tool call: %+v", bad, res)
			}
			if !strings.Contains(res.Content, "hi") {
				t.Fatalf("a %s engine cost the tool its result: %q", bad, res.Content)
			}
			if bad == "garbage" && len(res.Content) > 8*1024 {
				t.Fatalf("a nonsense footer was appended whole: %d bytes", len(res.Content))
			}
			if bad != "garbage" && !containsAny(notices, "continuing without working memory") {
				t.Fatalf("a %s engine said nothing: %v", bad, notices)
			}
		})
	}
}

// agentWithBadEngine gives the agent a real store and then swaps the
// recorder seam for one that misbehaves in the named way.
func agentWithBadEngine(t *testing.T, bad string) *Agent {
	t.Helper()
	ag, _ := agentWithEngine(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	ag.observeTimeout = 50 * time.Millisecond
	switch bad {
	case "panic":
		ag.observeFn = func(engine.Event) string { panic("the store exploded") }
	case "hang":
		ag.observeFn = func(engine.Event) string { <-release; return "" }
	case "garbage":
		ag.observeFn = func(engine.Event) string { return strings.Repeat("\x00garbage", 100000) }
	}
	return ag
}
