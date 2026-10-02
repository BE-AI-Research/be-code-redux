package agent

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/schedule"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// probeTool stands in for an MCP or editor tool: no gate of its own.
type probeTool struct{ ran atomic.Int32 }

func (p *probeTool) Name() string            { return "mcp_srv_probe" }
func (p *probeTool) Description() string     { return "probe" }
func (p *probeTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (p *probeTool) Run(context.Context, map[string]any) tools.Result {
	p.ran.Add(1)
	return tools.Result{Content: "probe ran"}
}

func probeScript() *scriptedProvider {
	return &scriptedProvider{responses: []provider.ChatResponse{
		{ToolCalls: []provider.ToolCall{{ID: "1", Name: "mcp_srv_probe", Arguments: `{"q":"x"}`}}},
		{Content: "carried on"},
	}}
}

// TestFiredTurnGatesMCPTool: an MCP tool in a fired turn asks tool_call;
// refused, the turn carries on; a tool: grant — the schedule's own or the
// standing schedules.allow — runs it unasked.
func TestFiredTurnGatesMCPTool(t *testing.T) {
	cases := []struct {
		name     string
		allow    []string
		standing []string
		ran      bool
	}{
		{"none", nil, nil, false},
		{"schedule grant", []string{"tool: mcp_srv_*"}, nil, true},
		{"standing grant", nil, []string{"tool: mcp_srv_probe"}, true},
		{"other grant", []string{"tool: mcp_other_*"}, nil, false},
	}
	for _, c := range cases {
		p := probeScript()
		ag, clock, _ := settingsAgent(t, p, func(s *config.SchedulesConfig) { s.Allow = c.standing })
		probe := &probeTool{}
		ag.Tools.AddTool(probe)
		ag.RefreshSystem()
		var asked []string
		ag.Tools.ApproveCtx = func(ctx context.Context, action, detail string) bool {
			if !tools.FiredAsk(ctx) {
				t.Errorf("%s: unmarked question %s", c.name, action)
			}
			asked = append(asked, action)
			return false
		}
		sc := mk("p", "in 1m")
		for _, g := range c.allow {
			gr, err := schedule.ParseGrant(g)
			if err != nil {
				t.Fatal(err)
			}
			sc.Allow = append(sc.Allow, gr)
		}
		runOneFired(t, ag, clock, sc)
		if (probe.ran.Load() == 1) != c.ran {
			t.Fatalf("%s: ran=%d asked=%v", c.name, probe.ran.Load(), asked)
		}
		if c.ran != (len(asked) == 0) || (!c.ran && asked[0] != "tool_call") {
			t.Fatalf("%s: asked=%v", c.name, asked)
		}
		if p.i != 2 {
			t.Fatalf("%s: the turn did not carry on after the tool (requests=%d)", c.name, p.i)
		}
		if ag.Tools.Fired() {
			t.Fatalf("%s: allowance not cleared", c.name)
		}
	}
}

// TestOrdinaryTurnNeverAsksToolCall: outside a fired turn the gate is not
// there at all.
func TestOrdinaryTurnNeverAsksToolCall(t *testing.T) {
	ag, _ := newTestAgent(t, probeScript(), nil)
	probe := &probeTool{}
	ag.Tools.AddTool(probe)
	ag.RefreshSystem()
	var asked []string
	ag.Tools.Approve = func(action, _ string) bool { asked = append(asked, action); return false }
	if _, _, err := ag.RunFull(context.Background(), "probe it"); err != nil {
		t.Fatal(err)
	}
	if probe.ran.Load() != 1 || len(asked) != 0 {
		t.Fatalf("ran=%d asked=%v", probe.ran.Load(), asked)
	}
}

// Part B: a saved history that holds a web_fetch or web_search result
// starts the session flagged, as one holding a browser page does.
func TestResumeWithAWebResultStartsFlagged(t *testing.T) {
	for _, name := range []string{"web_fetch", "web_search"} {
		for kind, msgs := range map[string][]provider.Message{
			"native": {
				{Role: provider.RoleUser, Content: "read it"},
				{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "1", Name: name, Arguments: `{}`}}},
				{Role: provider.RoleTool, ToolCallID: "1", Content: "PAGE TEXT"},
			},
			"embedded": {
				{Role: provider.RoleUser, Content: "read it"},
				{Role: provider.RoleAssistant, Content: `<tool_call>{"name":"` + name + `"}</tool_call>`},
				{Role: provider.RoleUser, Content: "<tool_result name=\"" + name + "\" status=\"ok\">\nPAGE TEXT\n</tool_result>\n"},
			},
		} {
			ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
			s := store.NewSession("s", "test-model", ag.Tools.Root)
			s.Messages = msgs
			ag.Resume(s)
			if !ag.Tools.UntrustedWeb() {
				t.Fatalf("%s %s: a resumed history with a web result started unflagged", name, kind)
			}
		}
	}
}

func TestDescribeListsToolGrant(t *testing.T) {
	sc := mk("p", "in 1m")
	g, err := schedule.ParseGrant("tool: mcp_github_*")
	if err != nil {
		t.Fatal(err)
	}
	sc.Allow = schedule.Allowance{g}
	sp, err := schedule.Parse(sc.When, t0)
	if err != nil {
		t.Fatal(err)
	}
	d := describe(sc, sp, t0.Add(time.Second))
	if !strings.Contains(d, "tool: mcp_github_*") || !strings.Contains(d, "← every tool whose name matches") {
		t.Fatalf("describe:\n%s", d)
	}
}
