package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// pageTool stands in for the browser: a failing result carrying page text.
type pageTool struct{}

func (pageTool) Name() string            { return "browser" }
func (pageTool) Description() string     { return "test browser" }
func (pageTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (pageTool) Run(context.Context, map[string]any) tools.Result {
	return tools.Result{IsError: true, Content: tools.WebHeader + "\nSECRET PAGE TEXT"}
}

// Final review I1: RunFull no longer clears the flag — a sub-agent's
// hand-back re-enters through RunFull while the page is still in history.
func TestRunFullKeepsTheUntrustedWebFlag(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{responses: []provider.ChatResponse{{Content: "done"}}}, nil)
	ag.Tools.MarkUntrustedWeb()
	if _, _, err := ag.RunFull(context.Background(), "sub-agent big finished 3.2 (done): ported it"); err != nil {
		t.Fatal(err)
	}
	if !ag.Tools.UntrustedWeb() {
		t.Fatal("a request nobody typed cleared the untrusted-web flag")
	}
}

// Only a request a person typed clears it.
func TestBeginTypedRequestClearsTheUntrustedWebFlag(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	ag.Tools.MarkUntrustedWeb()
	ag.BeginTypedRequest()
	if ag.Tools.UntrustedWeb() {
		t.Fatal("a typed request kept the untrusted-web flag")
	}
}

// The queue tells a hand-back from a person's line.
func TestQueueMarksHarnessLines(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	ag.Enqueue("typed")
	ag.EnqueueFrom("typed elsewhere", 2)
	if !TypedByPerson(ag.PeekItems()) {
		t.Fatal("typed lines read as harness lines")
	}
	ag.EnqueueHarness("sub-agent big finished 3.2 (done): ported it")
	if TypedByPerson(ag.PeekItems()) {
		t.Fatal("a queue holding a hand-back reads as typed by a person")
	}
	if TypedByPerson(nil) {
		t.Fatal("an empty drain reads as typed by a person")
	}
}

// An approved plan is a person's request.
func TestExecutePlanClearsTheUntrustedWebFlag(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{responses: []provider.ChatResponse{{Content: "done"}}}, nil)
	ag.Tools.MarkUntrustedWeb()
	if _, _, err := ag.ExecutePlan(context.Background(), "do it", "1. do it"); err != nil {
		t.Fatal(err)
	}
	if ag.Tools.UntrustedWeb() {
		t.Fatal("an approved plan kept the untrusted-web flag")
	}
}

// A resumed history holding a page starts flagged, native or embedded.
func TestResumeWithABrowserResultStartsFlagged(t *testing.T) {
	native := []provider.Message{
		{Role: provider.RoleUser, Content: "read it"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "1", Name: "browser", Arguments: `{"action":"snapshot"}`}}},
		{Role: provider.RoleTool, ToolCallID: "1", Name: "browser", Content: "SECRET PAGE TEXT"},
	}
	unnamed := []provider.Message{
		{Role: provider.RoleUser, Content: "read it"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "1", Name: "browser", Arguments: `{"action":"snapshot"}`}}},
		{Role: provider.RoleTool, ToolCallID: "1", Content: "SECRET PAGE TEXT"},
	}
	embedded := []provider.Message{
		{Role: provider.RoleUser, Content: "read it"},
		{Role: provider.RoleAssistant, Content: `<tool_call>{"name":"browser"}</tool_call>`},
		{Role: provider.RoleUser, Content: "<tool_result name=\"browser\" status=\"ok\">\nSECRET PAGE TEXT\n</tool_result>\n"},
	}
	for name, msgs := range map[string][]provider.Message{"native": native, "unnamed": unnamed, "embedded": embedded} {
		ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
		s := store.NewSession("s", "test-model", ag.Tools.Root)
		s.Messages = msgs
		ag.Resume(s)
		if !ag.Tools.UntrustedWeb() {
			t.Fatalf("%s: a resumed history with a page started unflagged", name)
		}
	}
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	s := store.NewSession("s", "test-model", ag.Tools.Root)
	s.Messages = []provider.Message{{Role: provider.RoleUser, Content: "hi"}, {Role: provider.RoleAssistant, Content: "the browser is off"}}
	ag.Resume(s)
	if ag.Tools.UntrustedWeb() {
		t.Fatal("a resumed history with no page started flagged")
	}
}

// Final review I2: page text never reaches the handoff summarizer.
func TestHandoffWithholdsBrowserResults(t *testing.T) {
	for _, compat := range []bool{false, true} {
		var got provider.ChatRequest
		p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
			got = req
			return &provider.ChatResponse{Content: "briefing"}, nil
		}}
		ag, _ := newTestAgent(t, p, nil)
		ag.Session = store.NewSession("s", "test-model", ag.Tools.Root)
		ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "read the page"})
		if compat {
			ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "<tool_call>{}</tool_call>"})
			ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "<tool_result name=\"browser\" status=\"ok\">\nSECRET PAGE TEXT\n</tool_result>\n"})
		} else {
			ag.History.Add(provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "1", Name: "browser", Arguments: `{"action":"snapshot"}`}}})
			ag.History.Add(provider.Message{Role: provider.RoleTool, ToolCallID: "1", Name: "browser", Content: "SECRET PAGE TEXT"})
		}
		ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "read it"})
		if _, err := ag.WriteHandoff(context.Background(), true); err != nil {
			t.Fatal(err)
		}
		body := got.Messages[len(got.Messages)-1].Content
		if strings.Contains(body, "SECRET PAGE TEXT") {
			t.Fatalf("compat=%v: page text reached the handoff:\n%s", compat, body)
		}
		if !strings.Contains(body, "[browser result withheld]") {
			t.Fatalf("compat=%v: no placeholder in the handoff:\n%s", compat, body)
		}
	}
}

func TestRecentContextKeepsPageTextOut(t *testing.T) {
	p := &scriptedProvider{responses: []provider.ChatResponse{
		{ToolCalls: []provider.ToolCall{{ID: "1", Name: "browser", Arguments: `{"action":"snapshot"}`}}},
		{Content: "I could not read it."},
	}}
	ag, _ := newTestAgent(t, p, nil)
	ag.Tools.AddTool(pageTool{})
	ag.Run(context.Background(), "read the page")
	if rc := ag.RecentContext(); strings.Contains(rc, "SECRET PAGE TEXT") {
		t.Fatalf("page text reached the co-worker context:\n%s", rc)
	}
}

// TestAutoToolConsultationWithholdsBrowserPageText is fix round 1's Important
// (browser spec §3.5): three consecutive failing browser calls still trigger
// auto:tool, but the streak buffer handed to the co-worker must never carry
// the page text those failures returned.
func TestAutoToolConsultationWithholdsBrowserPageText(t *testing.T) {
	cw := coworkerStub(t, provider.ChatResponse{Content: "try a different selector."})
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls <= 3 {
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: fmt.Sprint(calls), Name: "browser", Arguments: `{"action":"click","ref":"e1"}`}}}, nil
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}}
	ag, _ := newTestAgent(t, p, withCoworkers("big"))
	ag.Tools.AddTool(pageTool{})
	if _, err := ag.Run(context.Background(), "click the button"); err != nil {
		t.Fatal(err)
	}
	if len(cw.lastReq.Messages) == 0 {
		t.Fatal("the co-worker was never consulted")
	}
	for _, m := range cw.lastReq.Messages {
		if strings.Contains(m.Content, "SECRET PAGE TEXT") {
			t.Fatalf("page text reached the co-worker:\n%s", m.Content)
		}
	}
	if !strings.Contains(cw.lastReq.Messages[1].Content, "(browser result withheld: page content stays on this machine)") {
		t.Fatalf("auto:tool question lacks the withheld placeholder:\n%s", cw.lastReq.Messages[1].Content)
	}
}

func TestBrowserGuidanceOnlyWithTheBrowserTool(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	if strings.Contains(ag.composeSystem(""), browserGuidance) {
		t.Fatal("browser guidance without a browser")
	}
	ag.Tools.AddTool(tools.NewBrowser(tools.BrowserConfig{Address: "127.0.0.1:1"}))
	if !strings.Contains(ag.composeSystem(""), browserGuidance) {
		t.Fatal("no browser guidance with the browser registered")
	}
}
