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
)

// The reply that ended the owner's VM run on 2026-09-20, in shape: Qwen's own
// tool-call layout, which neither Ollama's parser nor ours recognised, so a
// write_file the model plainly made was taken for its final answer and the
// file was never written.
const qwenXMLCall = "Context is tight. I have enough to write player.py now.\n\n<tool_call>\n<function=write_file>\n<parameter=path>\nsrc/worldsim/player.py\n</parameter>\n<parameter=content>\n\"\"\"First-person player controller (M4).\"\"\"\n\nclass Player:\n    def step(self):\n        self.pos = Vec3(px, py, pz)\n\n</parameter>\n</function>\n</tool_call>"

func args(t *testing.T, raw string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("arguments are not JSON: %v\n%s", err, raw)
	}
	return m
}

func TestQwenXMLToolCallIsParsed(t *testing.T) {
	known := map[string]bool{"write_file": true}
	clean, calls := ParseEmbeddedCalls(qwenXMLCall, known)
	if len(calls) != 1 || calls[0].Name != "write_file" {
		t.Fatalf("calls: %+v", calls)
	}
	a := args(t, calls[0].Arguments)
	if a["path"] != "src/worldsim/player.py" {
		t.Fatalf("path: %q", a["path"])
	}
	content, _ := a["content"].(string)
	if !strings.HasPrefix(content, `"""First-person player controller`) || !strings.HasSuffix(content, "self.pos = Vec3(px, py, pz)\n") {
		t.Fatalf("content was not kept verbatim (one framing newline off each end, no more):\n%q", content)
	}
	if clean != "Context is tight. I have enough to write player.py now." {
		t.Fatalf("the prose around the call: %q", clean)
	}
}

func TestQwenXMLVariants(t *testing.T) {
	known := map[string]bool{"read_file": true, "task": true, "write_file": true}
	types := func(tool, param string) string {
		switch tool + "." + param {
		case "read_file.offset", "read_file.limit":
			return "integer"
		case "task.steps":
			return "array"
		}
		return "string"
	}
	// No <tool_call> wrapper, two calls, typed parameters.
	in := "<function=read_file>\n<parameter=path>\na.go\n</parameter>\n<parameter=offset>\n244\n</parameter>\n</function>\n" +
		"<tool_call><function=task><parameter=action>plan</parameter><parameter=steps>[\"one\", \"two\"]</parameter></function></tool_call>"
	_, calls := ParseEmbeddedCallsTyped(in, known, types)
	if len(calls) != 2 {
		t.Fatalf("calls: %+v", calls)
	}
	if a := args(t, calls[0].Arguments); a["offset"] != float64(244) || a["path"] != "a.go" {
		t.Fatalf("read_file args: %v", a)
	}
	a := args(t, calls[1].Arguments)
	if steps, ok := a["steps"].([]any); !ok || len(steps) != 2 || a["action"] != "plan" {
		t.Fatalf("task args: %v", a)
	}
	if calls[0].ID == calls[1].ID {
		t.Fatal("two calls share an id")
	}

	// A file whose content is itself JSON stays a string: the schema says so.
	jsonFile := "<tool_call><function=write_file><parameter=path>x.json</parameter><parameter=content>\n[1, 2]\n</parameter></function></tool_call>"
	_, calls = ParseEmbeddedCallsTyped(jsonFile, known, types)
	if a := args(t, calls[0].Arguments); a["content"] != "[1, 2]" {
		t.Fatalf("JSON file content was decoded: %#v", a["content"])
	}

	// A tool nobody registered is not a call, and the text is left alone.
	clean, calls := ParseEmbeddedCalls("<function=rm_rf><parameter=path>/</parameter></function>", known)
	if len(calls) != 0 || !strings.Contains(clean, "rm_rf") {
		t.Fatalf("unknown tool: %+v %q", calls, clean)
	}
	// The JSON form still wins where both could match, and still works.
	_, calls = ParseEmbeddedCalls(`<tool_call>{"name":"read_file","arguments":{"path":"a.go"}}</tool_call>`, known)
	if len(calls) != 1 || calls[0].Name != "read_file" {
		t.Fatalf("JSON form: %+v", calls)
	}
}

// Through the whole loop: the reply that ended the VM run now writes its file
// and the run carries on to the model's real final answer.
func TestARunContinuesPastAQwenXMLToolCall(t *testing.T) {
	n := 0
	p := &funcProvider{}
	p.fn = func(provider.ChatRequest) (*provider.ChatResponse, error) {
		n++
		if n == 1 {
			return &provider.ChatResponse{Content: qwenXMLCall}, nil // plain content, no native tool call
		}
		return &provider.ChatResponse{Content: "player.py is written."}, nil
	}
	// compat_tool_calls "auto", as the owner's config has it: the helper's
	// default of "never" turns every text form of a call off.
	ag, dir := newTestAgent(t, p, func(c *config.Config) { c.CompatToolCalls = "auto" })
	out, err := ag.Run(context.Background(), "write the player controller")
	if err != nil {
		t.Fatal(err)
	}
	if out != "player.py is written." || n != 2 {
		t.Fatalf("the run ended on the tool call: out=%q after %d request(s)", out, n)
	}
	b, err := os.ReadFile(filepath.Join(dir, "src", "worldsim", "player.py"))
	if err != nil || !strings.Contains(string(b), "class Player:") {
		t.Fatalf("the file was not written: %v %q", err, b)
	}
}
