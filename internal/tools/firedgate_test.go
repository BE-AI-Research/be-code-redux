package tools

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/mcp"
)

// probeTool is a tool added through AddTool that records whether it ran.
type probeTool struct {
	name string
	ran  atomic.Int32
}

func (p *probeTool) Name() string            { return p.name }
func (p *probeTool) Description() string     { return "probe" }
func (p *probeTool) Schema() json.RawMessage { return schema(`{"type":"object"}`) }
func (p *probeTool) Run(context.Context, map[string]any) Result {
	p.ran.Add(1)
	return Result{Content: "probe ran"}
}

type gateAsk struct {
	action, detail string
	fired          bool
	shortcuts      bool
}

// gateReg is a registry whose ApproveCtx records every question and
// answers with answer.
func gateReg(t *testing.T, answer bool) (*Registry, func() []gateAsk) {
	t.Helper()
	r, err := NewRegistry(t.TempDir(), func(string, string) bool { return answer })
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var asks []gateAsk
	r.ApproveCtx = func(ctx context.Context, action, detail string) bool {
		mu.Lock()
		asks = append(asks, gateAsk{action, detail, FiredAsk(ctx), SessionShortcuts(ctx)})
		mu.Unlock()
		return answer
	}
	return r, func() []gateAsk {
		mu.Lock()
		defer mu.Unlock()
		return append([]gateAsk(nil), asks...)
	}
}

// localTransport sends every request, whatever its host, to srv: lets a
// web_fetch of "docs.example.com" reach an httptest server without the
// network.
func localTransport(srv *httptest.Server) *http.Transport {
	u, _ := url.Parse(srv.URL)
	return &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, u.Host)
	}}
}

func pageServer(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("page text"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFiredTurnGatesUngatedTools(t *testing.T) {
	srv := pageServer(t)
	for _, answer := range []bool{false, true} {
		r, asks := gateReg(t, answer)
		probe := &probeTool{name: "custom_probe"}
		r.AddTool(probe)
		addr, _ := fakeTCPServer(t, "tok")
		ide, err := mcp.DialTCP(context.Background(), "vscode", addr, "tok")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(ide.Close)
		r.AttachMCPPrefixed(ide, "ide_")
		addr2, _ := fakeTCPServer(t, "tok")
		other, err := mcp.DialTCP(context.Background(), "srv", addr2, "tok")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(other.Close)
		r.AttachMCP(other)
		r.AddTool(NewWebFetch(nil))
		r.SetAllowance(nil, time.Minute)
		calls := map[string]string{
			"custom_probe":        `{"x":1}`,
			"ide_diagnostics":     `{"path":"a.go"}`,
			"mcp_srv_diagnostics": `{}`,
			"web_fetch":           `{"url":"` + srv.URL + `/p"}`,
		}
		for name, args := range calls {
			before := len(asks())
			res := dispatchCall(r, name, args)
			got := asks()
			if len(got) != before+1 || got[before].action != "tool_call" || !got[before].fired {
				t.Fatalf("answer=%v %s: asks %+v", answer, name, got)
			}
			if !strings.Contains(got[before].detail, name) {
				t.Fatalf("%s: detail names the tool: %q", name, got[before].detail)
			}
			if got[before].shortcuts {
				t.Fatalf("%s: a tool_call question takes no session shortcut", name)
			}
			if answer == res.IsError {
				t.Fatalf("answer=%v %s: %+v", answer, name, res)
			}
			if !answer && !strings.Contains(res.Content, name+" needs a person's approval during a scheduled event, and none was given") {
				t.Fatalf("%s refusal: %q", name, res.Content)
			}
		}
		if (probe.ran.Load() == 1) != answer {
			t.Fatalf("answer=%v: probe ran %d times", answer, probe.ran.Load())
		}
		refused, _ := r.ClearAllowance()
		if (refused == "tool_call") == answer {
			t.Fatalf("answer=%v: refusal recorded %q", answer, refused)
		}
	}
}

func TestFiredToolCallDetailCarriesArgs(t *testing.T) {
	r, asks := gateReg(t, false)
	r.AddTool(&probeTool{name: "custom_probe"})
	r.SetAllowance(nil, time.Minute)
	dispatchCall(r, "custom_probe", `{"query":"needle","n":3}`)
	big := strings.Repeat("x", 10000)
	dispatchCall(r, "custom_probe", `{"blob":"`+big+`"}`)
	got := asks()
	if len(got) != 2 || !strings.Contains(got[0].detail, `"query": "needle"`) {
		t.Fatalf("detail: %+v", got)
	}
	if len(got[1].detail) > 2600 || !strings.Contains(got[1].detail, "truncated") {
		t.Fatalf("detail is capped: %d bytes", len(got[1].detail))
	}
}

func TestFiredToolGrantCoversTool(t *testing.T) {
	r, asks := gateReg(t, false)
	probe := &probeTool{name: "mcp_github_issues"}
	other := &probeTool{name: "mcp_jira_issues"}
	r.AddTool(probe)
	r.AddTool(other)
	r.SetAllowance(grants(t, "tool: mcp_github_*"), time.Minute)
	if res := dispatchCall(r, "mcp_github_issues", `{}`); res.IsError || len(asks()) != 0 || probe.ran.Load() != 1 {
		t.Fatalf("covered tool runs unasked: %+v %v", res, asks())
	}
	if res := dispatchCall(r, "mcp_jira_issues", `{}`); !res.IsError || len(asks()) != 1 || other.ran.Load() != 0 {
		t.Fatalf("uncovered tool asks: %+v %v", res, asks())
	}
}

func TestFiredToolCallTimesOutAndContinues(t *testing.T) {
	r, err := NewRegistry(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	r.ApproveCtx = func(ctx context.Context, action, detail string) bool {
		<-ctx.Done()
		return false
	}
	probe := &probeTool{name: "custom_probe"}
	r.AddTool(probe)
	r.SetAllowance(nil, 20*time.Millisecond)
	start := time.Now()
	res := dispatchCall(r, "custom_probe", `{}`)
	if !res.IsError || probe.ran.Load() != 0 || time.Since(start) > 2*time.Second {
		t.Fatalf("refused after the deadline: %+v", res)
	}
	if refused, timedOut := r.ClearAllowance(); refused != "tool_call" || !timedOut {
		t.Fatalf("%q %v", refused, timedOut)
	}
}

func TestFiredTurnWithNoApproverRefusesToolCall(t *testing.T) {
	r, err := NewRegistry(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	probe := &probeTool{name: "custom_probe"}
	r.AddTool(probe)
	r.SetAllowance(nil, time.Minute)
	if res := dispatchCall(r, "custom_probe", `{}`); !res.IsError || probe.ran.Load() != 0 {
		t.Fatalf("refused: %+v", res)
	}
}

func TestInheritingFiredTurnStillAsksToolCall(t *testing.T) {
	r, asks := gateReg(t, false)
	r.AddTool(&probeTool{name: "custom_probe"})
	r.SetFiredPolicy(nil, time.Minute, true)
	dispatchCall(r, "custom_probe", `{}`)
	got := asks()
	if len(got) != 1 || got[0].action != "tool_call" || got[0].shortcuts {
		t.Fatalf("inherit gives tool_call no shortcut: %+v", got)
	}
}

func TestSelfGatedToolsNeverAskToolCall(t *testing.T) {
	r, asks := gateReg(t, true)
	r.SetAllowance(nil, time.Minute)
	for _, c := range [][2]string{
		{"read_file", `{"path":"nope.txt"}`},
		{"list_dir", `{"path":"."}`},
		{"search", `{"pattern":"x"}`},
		{"process", `{"action":"list"}`},
	} {
		dispatchCall(r, c[0], c[1])
	}
	for _, a := range asks() {
		if a.action == "tool_call" {
			t.Fatalf("a self-gated tool asked tool_call: %+v", a)
		}
	}
	for name := range firedSelfGated {
		if !strings.Contains(" read_file write_file edit_file list_dir search shell process browser consult schedule task lookup history show changes ", " "+name+" ") {
			t.Fatalf("unexpected self-gated tool %q", name)
		}
	}
}

func TestOutsideFiredTurnNoToolCallPrompt(t *testing.T) {
	r, asks := gateReg(t, false)
	probe := &probeTool{name: "custom_probe"}
	r.AddTool(probe)
	if res := dispatchCall(r, "custom_probe", `{}`); res.IsError || probe.ran.Load() != 1 || len(asks()) != 0 {
		t.Fatalf("no fired turn, no question: %+v %v", res, asks())
	}
	// Subset and Scoped never carry the fired policy.
	r.SetAllowance(nil, time.Minute)
	sub := r.Subset("custom_probe")
	if res := dispatchCall(sub, "custom_probe", `{}`); res.IsError || len(asks()) != 0 {
		t.Fatalf("a subset is not fired: %+v %v", res, asks())
	}
}

// TestEveryBuiltinToolIsClassified fails when a tool is added to
// NewRegistry without deciding whether a fired turn gates it: add it to
// firedSelfGated (it asks for itself, or only reads) or to
// firedGatedBuiltins (it should ask tool_call).
func TestEveryBuiltinToolIsClassified(t *testing.T) {
	r, err := NewRegistry(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range r.Names() {
		if !firedSelfGated[n] && !firedGatedBuiltins[n] {
			t.Errorf("built-in tool %q is not classified for fired turns", n)
		}
	}
}

func TestGrantsCoveredToolGrants(t *testing.T) {
	standing := grants(t, "tool: mcp_github_*", "tool: web_fetch", "tool: ide_?iag*")
	for req, want := range map[string]bool{
		"tool: web_fetch":         true,
		"tool: mcp_github_*":      true,
		"tool: mcp_github_issues": true,
		"tool: mcp_github_a*":     true,
		"tool: mcp_*":             false,
		"tool: web_search":        false,
		"tool: ide_diagnostics":   true,
		"tool: ide_?iagnostics":   false, // '?' never compared as text
		"tool: ide_d*":            false,
	} {
		if got := GrantsCovered(grants(t, req), standing); got != want {
			t.Errorf("%s: covered=%v, want %v", req, got, want)
		}
	}
	if GrantsCovered(grants(t, "tool: web_fetch"), grants(t, "shell: web_fetch")) {
		t.Fatal("a shell grant never covers a tool grant")
	}
}

// --- Part B: web tools mark the request untrusted ----------------------------

func TestWebFetchMarksUntrusted(t *testing.T) {
	srv := pageServer(t)
	cases := []struct {
		url   string
		sites map[string]string
		want  bool
	}{
		{"http://docs.example.com/p", nil, true},
		{"http://docs.example.com/p", map[string]string{"*.example.com": "allow"}, false},
		{"http://docs.example.com/p", map[string]string{"*.example.com": "watch"}, true},
		{srv.URL + "/p", nil, false}, // loopback is the allow tier by default
	}
	for _, c := range cases {
		r, _ := NewRegistry(t.TempDir(), nil)
		ft := NewWebFetch(c.sites).(*webFetchTool)
		ft.client.Transport = localTransport(srv)
		r.AddTool(ft)
		res := dispatchCall(r, "web_fetch", `{"url":"`+c.url+`"}`)
		if res.IsError {
			t.Fatal(res.Content)
		}
		if r.UntrustedWeb() != c.want {
			t.Errorf("%s sites=%v: untrusted=%v, want %v", c.url, c.sites, r.UntrustedWeb(), c.want)
		}
	}
	// A failed fetch shows no page: not marked.
	r, _ := NewRegistry(t.TempDir(), nil)
	ft := NewWebFetch(nil).(*webFetchTool)
	srv404 := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv404.Close)
	ft.client.Transport = localTransport(srv404)
	r.AddTool(ft)
	if res := dispatchCall(r, "web_fetch", `{"url":"http://docs.example.com/x"}`); !res.IsError || r.UntrustedWeb() {
		t.Fatalf("a 404 is not a page: %+v untrusted=%v", res, r.UntrustedWeb())
	}
}

func TestWebFetchUntrustedThenShellAsksAfterWeb(t *testing.T) {
	srv := pageServer(t)
	log := &allowLog{answer: false}
	r, _ := NewRegistry(t.TempDir(), log.approve)
	r.ShellAllow = []string{"echo *"}
	ft := NewWebFetch(nil).(*webFetchTool)
	ft.client.Transport = localTransport(srv)
	r.AddTool(ft)
	dispatchCall(r, "web_fetch", `{"url":"http://docs.example.com/p"}`)
	dispatchCall(r, "shell", `{"command":"echo hi"}`)
	if got := log.asked(); len(got) != 1 || got[0] != "shell_after_web" {
		t.Fatalf("after a fetched page the shell asks shell_after_web: %v", got)
	}
	// Through a Subset, the mark still reaches the primary registry.
	r.ClearUntrustedWeb()
	sub := r.Subset("web_fetch")
	dispatchCall(sub, "web_fetch", `{"url":"http://docs.example.com/p"}`)
	if !r.UntrustedWeb() {
		t.Fatal("a subset's fetch marks the primary registry")
	}
}

func TestWebSearchMarksUntrusted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[{"title":"T","link":"https://go.dev","snippet":"S"}]}`))
	}))
	defer srv.Close()
	t.Setenv("TEST_PSE_KEY", "k")
	r, _ := NewRegistry(t.TempDir(), nil)
	r.AddTool(NewWebSearch(WebSearchConfig{CX: "cx", APIKeyEnv: "TEST_PSE_KEY", Endpoint: srv.URL}))
	if res := dispatchCall(r, "web_search", `{"query":"q"}`); res.IsError || !r.UntrustedWeb() {
		t.Fatalf("a search result marks the request: %+v untrusted=%v", res, r.UntrustedWeb())
	}
	r2, _ := NewRegistry(t.TempDir(), nil)
	r2.AddTool(NewWebSearch(WebSearchConfig{CX: "cx", APIKeyEnv: "UNSET_PSE_KEY_XYZ", Endpoint: srv.URL}))
	if dispatchCall(r2, "web_search", `{"query":"q"}`); r2.UntrustedWeb() {
		t.Fatal("a failed search shows nothing: not marked")
	}
}
