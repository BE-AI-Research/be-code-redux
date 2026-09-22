package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/provider"
)

func TestStatsReportShowsWhatTheSessionCost(t *testing.T) {
	n := 0
	p := &funcProvider{fn: func(provider.ChatRequest) (*provider.ChatResponse, error) {
		n++
		if n == 1 {
			return &provider.ChatResponse{
				ToolCalls: []provider.ToolCall{{ID: "1", Name: "list_dir", Arguments: `{"path":"."}`}},
				Reasoning: strings.Repeat("r", 2500),
				Usage:     provider.Usage{PromptTokens: 900, CompletionTokens: 40, PromptDuration: 21 * time.Second},
			}, nil
		}
		return &provider.ChatResponse{Content: "done", Usage: provider.Usage{PromptTokens: 1100, CompletionTokens: 10, PromptDuration: 300 * time.Millisecond}}, nil
	}}
	ag, _ := newTestAgent(t, p, nil)
	if _, err := ag.Run(context.Background(), "look around"); err != nil {
		t.Fatal(err)
	}
	rep := ag.StatsReport([]string{"vscode", "ssh from 192.168.1.38"})
	for _, want := range []string{
		"Session", "terminals              2 (vscode, ssh from 192.168.1.38)",
		"Context", "in use", "fixed prompt",
		"Model", "requests               2", "prompt tokens          2000", "completion tokens      50 (plus 2k chars of hidden reasoning)",
		"prompt cache misses    1 of 2 requests",
		"Tools", "calls                  1 · 0 verification repair rounds", "by tool                list_dir 1",
	} {
		if !strings.Contains(rep, want) {
			t.Fatalf("report lacks %q:\n%s", want, rep)
		}
	}
	// A copy, never the live map.
	u := ag.Usage()
	u.ToolsByName["list_dir"] = 99
	if ag.Usage().ToolsByName["list_dir"] != 1 {
		t.Fatal("Usage handed out the live map")
	}
}
