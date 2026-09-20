package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// TestAPanicAnywhereInTheEngineDetachesItOnce is I6. Only observe used to
// recover, so a store that panicked inside Render took the session down from
// composeSystem — before every model call — and one that panicked in Flush
// took it down on the way out of a request that had succeeded. Every call
// now shares one fence: one notice, the engine off for the session, the run
// unharmed.
func TestAPanicAnywhereInTheEngineDetachesItOnce(t *testing.T) {
	for _, op := range []string{
		"render", "reload", "ensure root", "start task", "turn", "cache",
		"observe", "flush", "task plan", "task show", "baseline",
	} {
		t.Run(op, func(t *testing.T) {
			calls := 0
			p := &funcProvider{fn: func(provider.ChatRequest) (*provider.ChatResponse, error) {
				calls++
				switch calls {
				case 1:
					return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "1", Name: "search", Arguments: `{"pattern":"zzz"}`}}}, nil
				case 2:
					return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "2", Name: "task", Arguments: `{"action":"plan","text":"x","steps":["a"]}`}}}, nil
				case 3:
					return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "3", Name: "task", Arguments: `{"action":"show"}`}}}, nil
				}
				return &provider.ChatResponse{Content: "finished"}, nil
			}}
			ag, _ := newTestAgent(t, p, nil)
			withEngine(t, ag)
			ag.Tools.AddTool(tools.NewTask(ag.TaskLedger()))
			ag.RefreshSystem()
			notices := collectNotices(ag)
			ag.engineFault = func(got string) {
				if got == op {
					panic("the store exploded in " + op)
				}
			}
			answer, _, err := ag.RunFull(context.Background(), "do the thing")
			if err != nil || answer != "finished" {
				t.Fatalf("the run did not survive a panic in %s: %q, %v", op, answer, err)
			}
			if op == "baseline" {
				// Nothing in this run reads the baseline; ask for it.
				ag.EngineBaseline()
			}
			n := 0
			for _, m := range *notices {
				if strings.Contains(m, "continuing without working memory") {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("a panic in %s raised %d notices, want exactly one: %v", op, n, *notices)
			}
			if ag.engine() != nil {
				t.Fatalf("the engine is still attached after a panic in %s", op)
			}
			// Detached means detached: the next request runs without a block
			// and without a second notice.
			calls = 3
			if _, _, err := ag.RunFull(context.Background(), "and again"); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(ag.History.System.Content, "Working memory:\n") {
				t.Fatalf("a detached engine still renders a block")
			}
			if got := len(*notices); got != 1 && n == 1 {
				for _, m := range (*notices)[1:] {
					if strings.Contains(m, "engine") {
						t.Fatalf("a second engine notice after detaching: %v", *notices)
					}
				}
			}
		})
	}
}
