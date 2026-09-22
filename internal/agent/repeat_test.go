package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// countingTool answers with a fixed text, or with the call number when
// changing is set — a command whose output moves, like a test run after an edit.
type countingTool struct {
	name     string
	changing bool
	n        int
}

func (c *countingTool) Name() string        { return c.name }
func (c *countingTool) Description() string { return "stub" }
func (c *countingTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (c *countingTool) Run(context.Context, map[string]any) tools.Result {
	c.n++
	if c.changing {
		return tools.Result{Content: fmt.Sprintf("run %d", c.n)}
	}
	return tools.Result{Content: "same as ever"}
}

// A small model in a loop makes the same call and reads the same answer again
// and again. The third identical answer to an identical call says so.
func TestAnIdenticalCallWithAnIdenticalResultIsCalledOut(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	ag.Tools.AddTool(&countingTool{name: "probe"})
	call := provider.ToolCall{Name: "probe", Arguments: `{"q": "x"}`}
	for i := 1; i <= 2; i++ {
		if res := ag.dispatch(context.Background(), call); strings.Contains(res.Content, "exact call") {
			t.Fatalf("call %d was called out too early: %q", i, res.Content)
		}
	}
	res := ag.dispatch(context.Background(), call)
	if !strings.Contains(res.Content, "same as ever") || !strings.Contains(res.Content, "this exact call 3 times") {
		t.Fatalf("third identical call not called out: %q", res.Content)
	}
	// Spacing and key order are not a different call.
	res = ag.dispatch(context.Background(), provider.ToolCall{Name: "probe", Arguments: `{"q":"x"}`})
	if !strings.Contains(res.Content, "this exact call 4 times") {
		t.Fatalf("reformatted arguments counted as a new call: %q", res.Content)
	}
	// Different arguments are a different call.
	res = ag.dispatch(context.Background(), provider.ToolCall{Name: "probe", Arguments: `{"q":"y"}`})
	if strings.Contains(res.Content, "exact call") {
		t.Fatalf("a different call was called out: %q", res.Content)
	}
}

// Running the tests again after an edit is the same call on purpose. It is
// only a loop when the answer has not moved either.
func TestARepeatedCallWhoseResultChangesIsLeftAlone(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	ag.Tools.AddTool(&countingTool{name: "probe", changing: true})
	call := provider.ToolCall{Name: "probe", Arguments: `{}`}
	for i := 0; i < 5; i++ {
		if res := ag.dispatch(context.Background(), call); strings.Contains(res.Content, "exact call") {
			t.Fatalf("a call whose result changed was called out: %q", res.Content)
		}
	}
}

// The task tool is how the model keeps its own record; repeating it is not a loop.
func TestBookkeepingToolsAreNeverCalledOut(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	ag.Tools.AddTool(&countingTool{name: "task"})
	call := provider.ToolCall{Name: "task", Arguments: `{"action":"show"}`}
	for i := 0; i < 5; i++ {
		if res := ag.dispatch(context.Background(), call); strings.Contains(res.Content, "exact call") {
			t.Fatalf("task was called out: %q", res.Content)
		}
	}
}
