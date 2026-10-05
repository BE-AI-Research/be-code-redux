package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
)

// hugeTool returns far more than any per-call cap allows.
type hugeTool struct{}

func (hugeTool) Name() string            { return "huge" }
func (hugeTool) Description() string     { return "huge" }
func (hugeTool) Schema() json.RawMessage { return schema(`{"type":"object"}`) }
func (hugeTool) Run(context.Context, map[string]any) Result {
	return Result{Content: strings.Repeat("x", 200*1024)}
}

// Every tool result is held to MaxOutput, whether or not the tool caps its
// own output: one uncapped result (a whole task tree from `task show`) was
// larger than the model's window, and compaction keeps the newest result
// whole, so nothing could shrink it.
func TestDispatchCapsEveryToolResult(t *testing.T) {
	reg, _ := NewRegistry(t.TempDir(), nil)
	reg.AddTool(hugeTool{})
	res := reg.Dispatch(context.Background(), provider.ToolCall{Name: "huge", Arguments: `{}`})
	if len(res.Content) > reg.MaxOutput()+200 {
		t.Fatalf("result is %d bytes; cap is %d", len(res.Content), reg.MaxOutput())
	}
	if !strings.Contains(res.Content, "truncated") {
		t.Fatalf("a cut result must say so: %q", res.Content[len(res.Content)-120:])
	}
}
