package agent

import (
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/store"
)

// A session saved before tool results were capped (6EE9MY: one 225 KB
// task show, ~76k tokens in a 32k window, the context wheel at 300%) is cut
// down to the per-call cap when it is resumed, so it opens at a sane size.
func TestResumeClipsOversizedToolResults(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 32768 })
	ag.ApplyWindow(32768)
	huge := strings.Repeat("note line with words in it\n", 9000) // ~240 KB
	small := "noted"
	ag.Resume(&store.Session{ID: "s", Code: "6EE9MY", Messages: []provider.Message{
		{Role: provider.RoleUser, Content: "show the tree"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c1", Name: "task", Arguments: `{"action":"show"}`}}},
		{Role: provider.RoleTool, ToolCallID: "c1", Name: "task", Content: huge},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c2", Name: "task", Arguments: `{"action":"note"}`}}},
		{Role: provider.RoleTool, ToolCallID: "c2", Name: "task", Content: small},
	}})
	max := ag.Tools.MaxOutput()
	got := ag.History.Messages[2].Content
	if len(got) > max+200 || !strings.Contains(got, "truncated") {
		t.Fatalf("stored result not clipped: %d bytes (cap %d)", len(got), max)
	}
	if ag.History.Messages[4].Content != small {
		t.Fatal("a small result was changed")
	}
	if ag.History.Messages[0].Content != "show the tree" {
		t.Fatal("a person's own message was changed")
	}
	if pct := ag.History.Tokens() * 100 / ag.History.Limit(); pct > 100 {
		t.Fatalf("resumed session still at %d%% of the limit", pct)
	}
}
