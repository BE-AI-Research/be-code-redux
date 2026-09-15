package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// funcProvider answers each request through a function, so tests can react
// to what the agent actually sent (compaction prompts, message counts).
type funcProvider struct {
	fn   func(req provider.ChatRequest) (*provider.ChatResponse, error)
	reqs []provider.ChatRequest
}

func (f *funcProvider) Name() string { return "func" }
func (f *funcProvider) Chat(_ context.Context, req provider.ChatRequest, _ provider.StreamFunc) (*provider.ChatResponse, error) {
	f.reqs = append(f.reqs, req)
	return f.fn(req)
}
func (f *funcProvider) ListModels(context.Context) ([]provider.ModelInfo, error) { return nil, nil }
func (f *funcProvider) Ping(context.Context) (string, error)                     { return "ok", nil }

func newTestAgent(t *testing.T, p provider.Provider, mut func(*config.Config)) (*Agent, string) {
	t.Helper()
	dir := t.TempDir()
	reg, err := tools.NewRegistry(dir, func(a, d string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.VerifyOnDone = false
	cfg.CompatToolCalls = "never"
	cfg.RepoMap = false
	if mut != nil {
		mut(cfg)
	}
	return New(cfg, p, "test-model", reg, ""), dir
}

// An empty reply with no tool calls is not an answer: nudge once, then use
// the real reply.
func TestRunRetriesEmptyReplyOnce(t *testing.T) {
	p := &scriptedProvider{responses: []provider.ChatResponse{
		{Content: "   "},
		{Content: "the real answer"},
	}}
	ag, _ := newTestAgent(t, p, nil)
	answer, err := ag.Run(context.Background(), "do the thing")
	if err != nil {
		t.Fatal(err)
	}
	if answer != "the real answer" {
		t.Fatalf("answer = %q", answer)
	}
	if p.i != 2 {
		t.Fatalf("expected 2 model calls, got %d", p.i)
	}
	// The nudge must be visible to the model on the second call.
	last := p.lastReq.Messages[len(p.lastReq.Messages)-1]
	if last.Role != provider.RoleUser || !strings.Contains(strings.ToLower(last.Content), "empty") {
		t.Fatalf("no nudge message sent: %+v", last)
	}
}

func TestRunFailsOnRepeatedEmptyReply(t *testing.T) {
	p := &scriptedProvider{responses: []provider.ChatResponse{{Content: ""}, {Content: ""}}}
	ag, _ := newTestAgent(t, p, nil)
	_, err := ag.Run(context.Background(), "do the thing")
	if err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("expected empty-reply error, got %v", err)
	}
}

// finish_reason=length with nothing usable means the window is exhausted:
// one retry at low reasoning effort, then the user must be told why.
func TestRunReportsTruncatedOutput(t *testing.T) {
	p := &scriptedProvider{responses: []provider.ChatResponse{{Content: "", FinishReason: "length"}, {Content: "", FinishReason: "length"}}}
	ag, _ := newTestAgent(t, p, nil)
	_, err := ag.Run(context.Background(), "do the thing")
	if err == nil || !strings.Contains(err.Error(), "length") {
		t.Fatalf("expected output-limit error, got %v", err)
	}
	if p.i != 2 {
		t.Fatalf("should retry a length cutoff exactly once, made %d calls", p.i)
	}
}

// Server-reported prompt_tokens recalibrate the estimator mid-session.
func TestRunCalibratesEstimateFromUsage(t *testing.T) {
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		// A real server counts the rendered tools schema too.
		specs, _ := json.Marshal(req.Tools)
		est := estimateTokens(string(specs))
		for _, m := range req.Messages {
			est += messageTokens(m) * 2
		}
		return &provider.ChatResponse{Content: "ok", Usage: provider.Usage{PromptTokens: est}}, nil
	}}
	ag, _ := newTestAgent(t, p, nil)
	before := ag.History.CharsPerToken
	if _, err := ag.Run(context.Background(), strings.Repeat("some task text ", 100)); err != nil {
		t.Fatal(err)
	}
	if ag.History.CharsPerToken >= before {
		t.Fatalf("ratio not recalibrated: before=%v after=%v", before, ag.History.CharsPerToken)
	}
	if ag.History.Extra == 0 {
		t.Fatal("tools schema overhead not accounted in History.Extra")
	}
}

// A single long tool-using request must compact while it runs, not only at
// the start of the next user request.
func TestRunCompactsInsideToolLoop(t *testing.T) {
	toolTurns := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		if strings.HasPrefix(req.Messages[0].Content, "Summarize") {
			return &provider.ChatResponse{Content: "SUMMARY: read big.txt several times"}, nil
		}
		if toolTurns < 6 {
			toolTurns++
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "c", Name: "read_file", Arguments: `{"path":"big.txt"}`}}}, nil
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}}
	ag, dir := newTestAgent(t, p, func(c *config.Config) { c.ContextTokens = 2500; c.MaxTurns = 20 })
	os.WriteFile(filepath.Join(dir, "big.txt"), []byte(strings.Repeat("line of text here\n", 200)), 0o644)

	if _, err := ag.Run(context.Background(), "read big.txt"); err != nil {
		t.Fatal(err)
	}
	summarized := false
	for _, m := range ag.History.Messages {
		if strings.Contains(m.Content, "SUMMARY:") {
			summarized = true
		}
	}
	if !summarized {
		t.Fatalf("no compaction happened inside the tool loop (%d messages)", len(ag.History.Messages))
	}
	final := p.reqs[len(p.reqs)-1]
	if len(final.Messages) > 8 {
		t.Fatalf("final prompt still carries %d messages", len(final.Messages))
	}
}

// The summary request must include the original task and the NEWEST work,
// not just the oldest 24KB of transcript.
func TestCompactKeepsTaskAndNewestWork(t *testing.T) {
	var summaryReq provider.ChatRequest
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		summaryReq = req
		return &provider.ChatResponse{Content: "summary"}, nil
	}}
	ag, _ := newTestAgent(t, p, nil)
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "TASK-MARKER build the widget"})
	filler := strings.Repeat("filler ", 100) // 700 chars per message
	for i := 0; i < 60; i++ {                // ~42KB of transcript, well over the 24KB cap
		ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: filler})
	}
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "NEWEST-MARKER decided to use sqlite"})
	for i := 0; i < 4; i++ { // tail kept verbatim
		ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "tail"})
	}
	if err := ag.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := summaryReq.Messages[1].Content
	if !strings.Contains(body, "TASK-MARKER") {
		t.Fatal("original task missing from summary request")
	}
	if !strings.Contains(body, "NEWEST-MARKER") {
		t.Fatal("newest work missing from summary request (oldest-first truncation)")
	}
	if !strings.HasPrefix(ag.History.Messages[0].Content, "[Conversation summary") {
		t.Fatalf("summary not installed: %q", ag.History.Messages[0].Content[:40])
	}
}
