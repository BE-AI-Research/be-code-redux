package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// ---- embedded tool-call parsing -------------------------------------------

func known() map[string]bool {
	return map[string]bool{"read_file": true, "write_file": true, "shell": true}
}

func TestParseEmbeddedCallsTag(t *testing.T) {
	text := `I'll read the file first.
<tool_call>{"name": "read_file", "arguments": {"path": "main.go"}}</tool_call>`
	clean, calls := ParseEmbeddedCalls(text, known())
	if len(calls) != 1 || calls[0].Name != "read_file" {
		t.Fatalf("calls = %+v", calls)
	}
	if !strings.Contains(calls[0].Arguments, "main.go") {
		t.Fatalf("arguments lost: %s", calls[0].Arguments)
	}
	if strings.Contains(clean, "tool_call") {
		t.Fatalf("call block not removed from text: %q", clean)
	}
}

func TestParseEmbeddedCallsVariants(t *testing.T) {
	// {"tool": ..., "parameters": ...} variant in a json fence.
	text := "```json\n{\"tool\": \"shell\", \"parameters\": {\"command\": \"go test\"}}\n```"
	_, calls := ParseEmbeddedCalls(text, known())
	if len(calls) != 1 || calls[0].Name != "shell" {
		t.Fatalf("variant not parsed: %+v", calls)
	}
}

func TestParseEmbeddedCallsIgnoresUnknownAndPlainJSON(t *testing.T) {
	// A JSON code sample that is NOT a tool call must not be executed.
	text := "Here is the config format:\n```json\n{\"port\": 8443, \"tls\": true}\n```"
	clean, calls := ParseEmbeddedCalls(text, known())
	if len(calls) != 0 {
		t.Fatalf("plain JSON misparsed as call: %+v", calls)
	}
	if !strings.Contains(clean, "8443") {
		t.Fatal("innocent JSON was stripped from the reply")
	}
}

// ---- history trimming ------------------------------------------------------

func TestHistoryTrimsToBudget(t *testing.T) {
	h := NewHistory("system", 800) // ~3200 chars
	big := strings.Repeat("x", 2000)
	h.Add(provider.Message{Role: provider.RoleUser, Content: "the task"})
	for i := 0; i < 10; i++ {
		h.Add(provider.Message{Role: provider.RoleAssistant, Content: "calling tool",
			ToolCalls: []provider.ToolCall{{ID: "c", Name: "read_file", Arguments: "{}"}}})
		h.Add(provider.Message{Role: provider.RoleTool, ToolCallID: "c", Content: big})
	}
	before := h.Tokens()
	msgs := h.Prompt()
	after := h.Tokens()
	if after > before {
		t.Fatal("trim increased size")
	}
	if after > h.Budget*13/10 {
		t.Fatalf("still far over budget after trim: %d > %d", after, h.Budget)
	}
	// The original task must survive trimming.
	found := false
	for _, m := range msgs {
		if m.Role == provider.RoleUser && m.Content == "the task" {
			found = true
		}
	}
	if !found {
		t.Fatal("first user message (the task) was dropped")
	}
}

// ---- full loop with a scripted provider -----------------------------------

// scriptedProvider returns canned responses in order.
type scriptedProvider struct {
	responses []provider.ChatResponse
	i         int
	lastReq   provider.ChatRequest
}

func (s *scriptedProvider) Name() string { return "scripted" }
func (s *scriptedProvider) Chat(_ context.Context, req provider.ChatRequest, onDelta provider.StreamFunc) (*provider.ChatResponse, error) {
	s.lastReq = req
	if s.i >= len(s.responses) {
		return &provider.ChatResponse{Content: "done"}, nil
	}
	r := s.responses[s.i]
	s.i++
	if onDelta != nil && r.Content != "" {
		onDelta(r.Content)
	}
	return &r, nil
}
func (s *scriptedProvider) ListModels(context.Context) ([]provider.ModelInfo, error) {
	return nil, nil
}
func (s *scriptedProvider) Ping(context.Context) (string, error) { return "ok", nil }

func TestAgentLoopNativeCalls(t *testing.T) {
	dir := t.TempDir()
	reg, err := tools.NewRegistry(dir, func(a, d string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.VerifyOnDone = false
	cfg.CompatToolCalls = "never"

	p := &scriptedProvider{responses: []provider.ChatResponse{
		{ToolCalls: []provider.ToolCall{{ID: "1", Name: "write_file",
			Arguments: `{"path":"hi.txt","content":"hello"}`}}},
		{Content: "I wrote hi.txt containing hello."},
	}}
	ag := New(cfg, p, "test-model", reg, "")
	answer, err := ag.Run(context.Background(), "create hi.txt with hello")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answer, "hi.txt") {
		t.Fatalf("unexpected answer: %q", answer)
	}
	// The tool actually ran.
	res := reg.Dispatch(context.Background(), provider.ToolCall{Name: "read_file", Arguments: `{"path":"hi.txt"}`})
	if !strings.Contains(res.Content, "hello") {
		t.Fatal("write_file side effect missing")
	}
	// Tool result was fed back with proper pairing.
	foundPair := false
	for _, m := range p.lastReq.Messages {
		if m.Role == provider.RoleTool && m.ToolCallID == "1" {
			foundPair = true
		}
	}
	if !foundPair {
		t.Fatal("tool result not paired into next request")
	}
}

func TestAgentLoopEmbeddedCalls(t *testing.T) {
	dir := t.TempDir()
	reg, _ := tools.NewRegistry(dir, func(a, d string) bool { return true })
	cfg := config.Default()
	cfg.VerifyOnDone = false
	cfg.CompatToolCalls = "always"

	p := &scriptedProvider{responses: []provider.ChatResponse{
		{Content: `<tool_call>{"name":"write_file","arguments":{"path":"x.txt","content":"embedded"}}</tool_call>`},
		{Content: "done, x.txt written"},
	}}
	ag := New(cfg, p, "test-model", reg, "")
	answer, err := ag.Run(context.Background(), "write x.txt")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answer, "x.txt") {
		t.Fatalf("answer: %q", answer)
	}
	res := reg.Dispatch(context.Background(), provider.ToolCall{Name: "read_file", Arguments: `{"path":"x.txt"}`})
	if !strings.Contains(res.Content, "embedded") {
		t.Fatal("embedded tool call did not execute")
	}
	// In compat mode the request must not carry native tool specs.
	if len(p.lastReq.Tools) != 0 {
		t.Fatal("compat mode leaked native tools field")
	}
}

func TestAgentStopsAtMaxTurns(t *testing.T) {
	dir := t.TempDir()
	reg, _ := tools.NewRegistry(dir, func(a, d string) bool { return true })
	cfg := config.Default()
	cfg.VerifyOnDone = false
	cfg.MaxTurns = 3
	cfg.CompatToolCalls = "never"

	// Provider that always asks for another tool call.
	loop := make([]provider.ChatResponse, 10)
	for i := range loop {
		loop[i] = provider.ChatResponse{ToolCalls: []provider.ToolCall{
			{ID: "c", Name: "read_file", Arguments: `{"path":"nope.txt"}`}}}
	}
	ag := New(cfg, &scriptedProvider{responses: loop}, "m", reg, "")
	_, err := ag.Run(context.Background(), "loop forever")
	if err == nil || !strings.Contains(err.Error(), "3 tool turns") {
		t.Fatalf("expected max-turns error, got %v", err)
	}
}
