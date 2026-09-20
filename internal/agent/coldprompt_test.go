package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// A model that is about to load or reload reads the whole prompt from
// nothing. Old tool output is the cheapest thing in it to drop and the most
// expensive to re-read, so it goes before the reload rather than after; the
// newest results — the work in hand — stay exactly as they are.
func TestOldToolOutputIsCollapsedBeforeTheModelReloads(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 0 })
	ag.SetLoader(&fakeLoader{window: 8192})
	ag.Provider = &statusProvider{funcProvider: &funcProvider{}, window: 8192, loaded: true}
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "read everything"})
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("c%d", i)
		ag.History.Add(provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: id, Name: "read_file", Arguments: `{"path":"f.go"}`}}})
		ag.History.Add(provider.Message{Role: provider.RoleTool, ToolCallID: id, Name: "read_file", Content: fmt.Sprintf("result %d %s", i, strings.Repeat("x", 3000))})
	}
	notices := collectNotices(ag)

	ag.ResolveModelNow(context.Background())

	var kept, stubbed int
	for _, m := range ag.History.Messages {
		if m.Role != provider.RoleTool {
			continue
		}
		if m.Content == collapsedStub {
			stubbed++
		} else {
			kept++
		}
	}
	if kept != 2 || stubbed != 4 {
		t.Fatalf("kept %d, stubbed %d; want the newest 2 kept and the 4 before them stubbed", kept, stubbed)
	}
	last := ag.History.Messages[len(ag.History.Messages)-1]
	if !strings.HasPrefix(last.Content, "result 5 ") {
		t.Fatalf("the newest result was touched: %.40q", last.Content)
	}
	if !strings.Contains(strings.Join(*notices, "\n"), "4 old tool results") {
		t.Fatalf("the cleanup was silent: %q", *notices)
	}
}

// A short conversation is left alone: there is nothing worth dropping.
func TestASmallConversationIsNotTouchedBeforeAReload(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 0 })
	ag.SetLoader(&fakeLoader{window: 32768})
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "hi"})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "a", Name: "read_file", Arguments: `{}`}}})
	ag.History.Add(provider.Message{Role: provider.RoleTool, ToolCallID: "a", Content: strings.Repeat("x", 3000)})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "b", Name: "read_file", Arguments: `{}`}}})
	ag.History.Add(provider.Message{Role: provider.RoleTool, ToolCallID: "b", Content: strings.Repeat("y", 3000)})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c", Name: "read_file", Arguments: `{}`}}})
	ag.History.Add(provider.Message{Role: provider.RoleTool, ToolCallID: "c", Content: strings.Repeat("z", 3000)})
	ag.ResolveModelNow(context.Background())
	for _, m := range ag.History.Messages {
		if m.Content == collapsedStub {
			t.Fatal("a conversation well under its target was collapsed")
		}
	}
}
