package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// write_file/edit_file calls carry whole file bodies in their arguments;
// those must be stubbed on collapse just like tool results, or every file
// the agent ever wrote stays in the prompt forever.
func TestCollapseShrinksLargeToolCallArguments(t *testing.T) {
	h := NewHistory("sys", 100000)
	body := strings.Repeat("package main\n// generated line\n", 800)
	h.Add(provider.Message{Role: provider.RoleUser, Content: "task"})
	for i := 0; i < 3; i++ {
		h.Add(provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "w", Name: "write_file",
			Arguments: `{"path":"gen` + string(rune('a'+i)) + `.go","content":` + jsonString(body) + `}`}}})
		h.Add(provider.Message{Role: provider.RoleTool, ToolCallID: "w", Content: "wrote 24000 bytes"})
	}
	h.Add(provider.Message{Role: provider.RoleAssistant, Content: "done"})
	before := h.Tokens()
	h.CollapseToolResults(1)
	after := h.Tokens()
	if after > before/2 {
		t.Fatalf("arguments not shrunk: before=%d after=%d", before, after)
	}
	args := h.Messages[1].ToolCalls[0].Arguments
	if !strings.Contains(args, "gena.go") || strings.Contains(args, "generated line") {
		t.Fatalf("path lost or body kept: %.120s", args)
	}
	// The newest exchange keeps its arguments: the model may still refer
	// to what it just wrote.
	if !strings.Contains(h.Messages[5].ToolCalls[0].Arguments, "generated line") {
		t.Fatal("newest write's content was shrunk")
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// Compression must leave real runway: when over the limit, compress to the
// target fraction (default half) rather than to just under the limit, so
// the next few tool calls do not trigger another compaction each time.
func TestMaybeCompactCompressesToTargetNotJustUnderLimit(t *testing.T) {
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "SUMMARY"}, nil
	}}
	ag, _ := newTestAgent(t, p, func(c *config.Config) { c.ContextTokens = 6000 })
	big := strings.Repeat("output line\n", 300) // ~1200 tokens
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "task"})
	for i := 0; i < 6; i++ {
		ag.History.Add(provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c", Name: "read_file", Arguments: "{}"}}})
		ag.History.Add(provider.Message{Role: provider.RoleTool, ToolCallID: "c", Content: big})
	}
	ag.maybeCompact(context.Background())
	if ag.History.Tokens() > ag.History.Limit()/2 {
		t.Fatalf("compressed only to %d of limit %d; want at most half", ag.History.Tokens(), ag.History.Limit())
	}
}

// Model compaction keeps only a small verbatim tail, and big tool results
// in that tail are collapsed too, so the post-compaction prompt is mostly
// the summary.
func TestCompactLeavesSmallTail(t *testing.T) {
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "SUMMARY"}, nil
	}}
	ag, _ := newTestAgent(t, p, func(c *config.Config) { c.ContextTokens = 8000 })
	big := strings.Repeat("output line\n", 400)
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "task"})
	for i := 0; i < 5; i++ {
		ag.History.Add(provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c", Name: "read_file", Arguments: "{}"}}})
		ag.History.Add(provider.Message{Role: provider.RoleTool, ToolCallID: "c", Content: big})
	}
	if err := ag.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	sys := ag.History.MessageTokens(ag.History.System)
	if n := ag.History.Tokens(); n > sys+300 {
		t.Fatalf("post-compaction prompt still %d tokens (system is %d; summary+tail should be small)", n, sys)
	}
	if !strings.HasPrefix(ag.History.Messages[0].Content, summaryPrefix) {
		t.Fatal("summary missing")
	}
}
