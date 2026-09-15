package ui

import (
	"testing"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/provider"
)

func TestReplayNativeCompatSummaryAndCap(t *testing.T) {
	msgs := []provider.Message{
		{Role: provider.RoleUser, Content: agent.SummaryPrefix + "Original task: fix the build."},
		{Role: provider.RoleUser, Content: "read a.go"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "1", Name: "read_file", Arguments: `{"path":"a.go"}`}}},
		{Role: provider.RoleTool, ToolCallID: "1", Name: "read_file", Content: "    1\tpackage a\n    2\tfunc A() {}\n"},
		{Role: provider.RoleAssistant, Content: "A is defined."},
		{Role: provider.RoleUser, Content: "now compat"},
		{Role: provider.RoleAssistant, Content: "<tool_call>{\"name\":\"shell\"}</tool_call>", ToolCalls: nil},
		{Role: provider.RoleUser, Content: "<tool_result name=\"shell\" status=\"error\">\nexit status 1\nmore\n</tool_result>\n<tool_result name=\"list_dir\" status=\"ok\">\na.go\n</tool_result>\n"},
		{Role: provider.RoleTool, ToolCallID: "2", Name: "read_file", Content: "open nope.go: no such file or directory"},
	}
	lines := Replay(msgs, 0)
	want := []ReplayLine{
		{Kind: ReplaySummary, Text: "Original task: fix the build."},
		{Kind: ReplayUser, Text: "read a.go"},
		{Kind: ReplayTool, Label: "read_file", Text: `{"path":"a.go"}`},
		{Kind: ReplayToolOK, Label: "read_file", Text: "1\tpackage a"},
		{Kind: ReplayAssistant, Text: "A is defined."},
		{Kind: ReplayUser, Text: "now compat"},
		{Kind: ReplayAssistant, Text: "<tool_call>{\"name\":\"shell\"}</tool_call>"},
		{Kind: ReplayToolErr, Label: "shell", Text: "exit status 1"},
		{Kind: ReplayToolOK, Label: "list_dir", Text: "a.go"},
		{Kind: ReplayToolErr, Label: "read_file", Text: "open nope.go: no such file or directory"},
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%+v", len(lines), len(want), lines)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("line %d = %+v, want %+v", i, lines[i], want[i])
		}
	}
	// A turn cap keeps the last N user requests and what followed them.
	capped := Replay(msgs, 1)
	if len(capped) != 5 || capped[0].Kind != ReplayUser || capped[0].Text != "now compat" {
		t.Fatalf("capped: %+v", capped)
	}
	if got := Replay(nil, 0); len(got) != 0 {
		t.Fatal("nil messages must replay nothing")
	}
}
