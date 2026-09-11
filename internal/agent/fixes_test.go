package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/checkpoint"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// Plan mode's scratch agent must keep the planning prompt when Run refreshes
// the system prompt with git state; it used to be replaced by the normal
// coding prompt (which tells a read-only agent to write files).
func TestPlanAgentKeepsPlanPromptAcrossGitRefresh(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	scratch := ag.planAgent()
	sys := scratch.composeSystem("On branch main, clean")
	if !strings.HasPrefix(sys, planSystemPrompt[:60]) {
		t.Fatalf("plan prompt lost after git refresh:\n%.200s", sys)
	}
	if !strings.Contains(sys, "On branch main") {
		t.Fatal("git summary not appended")
	}
}

// goProject scaffolds a minimal Go module so verify.Detect finds checks.
func goProject(t *testing.T, ag *Agent, dir string) {
	t.Helper()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/x\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644)
	cp, err := checkpoint.New(dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ag.Checkpoints = cp
}

// A request that changes no files (a question) must not trigger the
// verification suite.
func TestRunFullSkipsVerificationWhenNothingChanged(t *testing.T) {
	p := &scriptedProvider{responses: []provider.ChatResponse{{Content: "It is a Go project."}}}
	ag, dir := newTestAgent(t, p, func(c *config.Config) { c.VerifyOnDone = true })
	goProject(t, ag, dir)
	_, rep, err := ag.RunFull(context.Background(), "what kind of project is this?")
	if err != nil {
		t.Fatal(err)
	}
	if rep != nil && rep.Verify != nil {
		t.Fatalf("verification ran with no file changes: %+v", rep.Verify)
	}
}

// Repair turns belong to the same checkpoint turn as the request, so /undo
// reverts the whole task and the reviewer sees every changed file.
func TestRunFullRepairsShareOneCheckpointTurn(t *testing.T) {
	p := &scriptedProvider{responses: []provider.ChatResponse{
		{ToolCalls: []provider.ToolCall{{ID: "1", Name: "write_file",
			Arguments: `{"path":"add.go","content":"package main\n\nfunc Add(a, b int) int { return a + b\n"}`}}},
		{Content: "added Add"},
		// repair turn after vet fails:
		{ToolCalls: []provider.ToolCall{{ID: "2", Name: "write_file",
			Arguments: `{"path":"add.go","content":"package main\n\nfunc Add(a, b int) int { return a + b }\n"}`}}},
		{Content: "fixed"},
	}}
	ag, dir := newTestAgent(t, p, func(c *config.Config) { c.VerifyOnDone = true; c.MaxRepairs = 1 })
	goProject(t, ag, dir)
	_, rep, err := ag.RunFull(context.Background(), "add an Add function")
	if err != nil {
		t.Fatal(err)
	}
	if rep == nil || rep.Verify == nil || !rep.Verify.Passed() {
		t.Fatalf("expected verification to pass after repair: %+v", rep)
	}
	if d := ag.Checkpoints.Depth(); d != 1 {
		t.Fatalf("checkpoint depth = %d, want 1 (repair opened its own turn)", d)
	}
}

// A model profile's temperature applies only when the user left the config
// at its default; an explicit config value always wins.
func TestProfileTemperatureAppliesOnlyOverDefault(t *testing.T) {
	p := &scriptedProvider{responses: []provider.ChatResponse{{Content: "ok"}, {Content: "ok"}}}
	ag, _ := newTestAgent(t, p, nil) // default temperature
	ag.SetModel("deepseek-r1:7b")    // profile temperature 0.3
	ag.Run(context.Background(), "hi")
	if p.lastReq.Temperature != 0.3 {
		t.Fatalf("profile temperature not applied: %v", p.lastReq.Temperature)
	}
	ag.Cfg.Temperature = 1.0 // explicit user choice
	ag.Run(context.Background(), "hi")
	if p.lastReq.Temperature != 1.0 {
		t.Fatalf("explicit temperature overridden: %v", p.lastReq.Temperature)
	}
}

// @-mention completion must not list directories outside the workspace.
func TestCompleteMentionRefusesParentTraversal(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "inside.txt"), nil, 0o644)
	if got := CompleteMention(root, "../"); len(got) != 0 {
		t.Fatalf("listed outside workspace: %v", got)
	}
	if got := CompleteMention(root, "in"); len(got) != 1 {
		t.Fatalf("normal completion broken: %v", got)
	}
}

// The repo map must reflect files the agent itself creates, so later turns
// see the new symbols.
func TestRepoMapRefreshesAfterAgentWrites(t *testing.T) {
	p := &scriptedProvider{responses: []provider.ChatResponse{
		{ToolCalls: []provider.ToolCall{{ID: "1", Name: "write_file",
			Arguments: `{"path":"greet.go","content":"package main\n\nfunc HelloWorldSymbol() {}\n"}`}}},
		{Content: "done"},
		{Content: "second turn"},
	}}
	ag, dir := newTestAgent(t, p, func(c *config.Config) { c.RepoMap = true })
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644)
	ag.Run(context.Background(), "add greet.go")
	ag.Run(context.Background(), "next")
	if !strings.Contains(ag.RepoMap(), "HelloWorldSymbol") {
		t.Fatalf("repo map stale after agent write:\n%s", ag.RepoMap())
	}
}

// When the history is over budget only because of old tool outputs, the
// cheap collapse pass must run first; the model-written compaction is the
// last resort, not the first reaction.
func TestMaybeCompactCollapsesOldToolResultsBeforeCallingModel(t *testing.T) {
	called := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		called++
		return &provider.ChatResponse{Content: "SUMMARY"}, nil
	}}
	ag, _ := newTestAgent(t, p, func(c *config.Config) { c.ContextTokens = 4000 })
	big := strings.Repeat("output line\n", 500) // ~2000 tokens each
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "task"})
	for i := 0; i < 4; i++ {
		ag.History.Add(provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c", Name: "read_file", Arguments: "{}"}}})
		ag.History.Add(provider.Message{Role: provider.RoleTool, ToolCallID: "c", Content: big})
	}
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "reading done"})
	if !ag.History.Over() {
		t.Fatal("precondition: history should be over budget")
	}
	ag.maybeCompact(context.Background())
	if called != 0 {
		t.Fatalf("model compaction ran (%d calls) although collapsing old tool results was enough", called)
	}
	if ag.History.Over() {
		t.Fatalf("still over budget after collapse: %d > %d", ag.History.Tokens(), ag.History.Limit())
	}
	if ag.History.Messages[0].Content != "task" {
		t.Fatal("task message lost")
	}
}

// Tool output caps must follow the window: a 24KB result is a quarter of an
// 8k window, which forces compaction every couple of calls.
func TestApplyWindowScalesToolOutputCap(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	ag.ApplyWindow(8192)
	small := ag.Tools.MaxOutput
	if small >= 24*1024 || small < 4*1024 {
		t.Fatalf("cap for 8k window = %d", small)
	}
	big, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 131072 })
	big.ApplyWindow(131072)
	if big.Tools.MaxOutput != 24*1024 {
		t.Fatalf("cap for a large window should stay at the 24KB default, got %d", big.Tools.MaxOutput)
	}
}
