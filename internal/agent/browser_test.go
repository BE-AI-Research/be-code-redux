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

// pageTool stands in for the browser: a failing result carrying page text.
type pageTool struct{}

func (pageTool) Name() string            { return "browser" }
func (pageTool) Description() string     { return "test browser" }
func (pageTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (pageTool) Run(context.Context, map[string]any) tools.Result {
	return tools.Result{IsError: true, Content: tools.WebHeader + "\nSECRET PAGE TEXT"}
}

func TestRunFullClearsTheUntrustedWebFlag(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{responses: []provider.ChatResponse{{Content: "done"}}}, nil)
	ag.Tools.MarkUntrustedWeb()
	if _, _, err := ag.RunFull(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if ag.Tools.UntrustedWeb() {
		t.Fatal("a new request kept the previous request's untrusted-web flag")
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
