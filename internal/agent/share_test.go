package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// shareAnswers answers share_page by host (yes unless named in no) and
// records every share_page question.
type shareAnswers struct {
	mu  sync.Mutex
	no  []string
	log []string
}

func (s *shareAnswers) approve(action, detail string) bool {
	if action != "share_page" {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, detail)
	for _, h := range s.no {
		if strings.Contains(detail, " on "+h+" ") || (h == "search" && strings.Contains(detail, "search")) {
			return false
		}
	}
	return true
}

func (s *shareAnswers) asks() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.log...)
}

// webHistory is a conversation that read pages before going online: two
// web_fetch results from a.test and one from b.test (native), a browser
// page from a.test, two searches, and a page result whose site cannot be
// told.
func webHistory() []provider.Message {
	call := func(id, name string) provider.Message {
		return provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: id, Name: name, Arguments: "{}"}}}
	}
	res := func(id, name, content string) provider.Message {
		return provider.Message{Role: provider.RoleTool, ToolCallID: id, Name: name, Content: content}
	}
	return []provider.Message{
		{Role: provider.RoleUser, Content: "look things up"},
		call("c1", "web_fetch"), res("c1", "web_fetch", "https://a.test/one (12 chars total)\nALPHA-ONE"),
		call("c2", "web_fetch"), res("c2", "web_fetch", "https://b.test/x (12 chars total)\nBRAVO-TEXT"),
		call("c3", "browser"), res("c3", "browser", tools.WebHeader+"\npage: A page — a.test/two\nALPHA-TWO"),
		call("c4", "web_search"), res("c4", "web_search", "results for \"q\":\n1. SEARCH-ONE"),
		call("c5", "web_search"), res("c5", "web_search", "results for \"r\":\n1. SEARCH-TWO"),
		call("c6", "browser"), res("c6", "browser", tools.WebHeader+"\ntabs:\n  1. UNKNOWN-TABS"),
		{Role: provider.RoleAssistant, Content: "noted"},
	}
}

func sentText(p *recProvider) string {
	var b strings.Builder
	for _, m := range p.requests()[0].Messages {
		b.WriteString(m.Content + "\n")
	}
	return b.String()
}

func shareAgent(t *testing.T, ans *shareAnswers) (*Agent, *recProvider) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	p := &recProvider{name: "openrouter", def: provider.ChatResponse{Content: "ok"}}
	ag, _ := newTestAgent(t, p, nil)
	ag.Tools.Approve = ans.approve
	ag.History.Messages = webHistory()
	ag.SetOnline("openrouter", "K", Pricing{})
	if !ag.StartOnlineGate() { // online_project: yes
		t.Fatal("project gate")
	}
	ag.Tools.SetShareGate(&tools.ShareGate{Provider: "openrouter"})
	return ag, p
}

func TestEarlierWebTextAskedPerSite(t *testing.T) {
	ans := &shareAnswers{no: []string{"b.test"}}
	ag, p := shareAgent(t, ans)
	if _, err := ag.Run(context.Background(), "go on"); err != nil {
		t.Fatal(err)
	}
	sent := sentText(p)
	for _, want := range []string{"ALPHA-ONE", "ALPHA-TWO", "SEARCH-ONE", "SEARCH-TWO",
		"(page text from b.test is not shown; it was not shared with openrouter)",
		"(page text is not shown; it was not shared with openrouter)"} {
		if !strings.Contains(sent, want) {
			t.Fatalf("request lacks %q:\n%s", want, sent)
		}
	}
	for _, gone := range []string{"BRAVO-TEXT", "UNKNOWN-TABS"} {
		if strings.Contains(sent, gone) {
			t.Fatalf("request carries %q:\n%s", gone, sent)
		}
	}
	asks := ans.asks()
	want := []string{
		"Send what the agent reads on a.test to openrouter?",
		"Send what the agent reads on b.test to openrouter?",
		"Send web search results to openrouter?",
	}
	if strings.Join(asks, "|") != strings.Join(want, "|") {
		t.Fatalf("asks %q", asks)
	}
	// Settled once: the next request asks nothing.
	ag.Run(context.Background(), "again")
	if len(ans.asks()) != 3 {
		t.Fatalf("asked again: %q", ans.asks())
	}
}

func TestEarlierSearchRefusedIsStubbedOnce(t *testing.T) {
	ans := &shareAnswers{no: []string{"search"}}
	ag, p := shareAgent(t, ans)
	ag.Run(context.Background(), "go on")
	sent := sentText(p)
	if strings.Contains(sent, "SEARCH-ONE") || strings.Contains(sent, "SEARCH-TWO") ||
		strings.Count(sent, "(web search results are not shown; they were not shared with openrouter)") != 2 {
		t.Fatalf("request:\n%s", sent)
	}
	n := 0
	for _, a := range ans.asks() {
		if strings.Contains(a, "search") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("search asked %d times", n)
	}
}

func TestEarlierWebTextGrantedHostNotAsked(t *testing.T) {
	ans := &shareAnswers{}
	ag, _ := shareAgent(t, ans)
	ag.Tools.ShareEarlier(context.Background(), "a.test", false) // shared earlier this session
	ag.Run(context.Background(), "go on")
	for _, a := range ans.asks()[1:] {
		if strings.Contains(a, "a.test") {
			t.Fatalf("a granted host was asked again: %q", ans.asks())
		}
	}
}

func TestEarlierWebTextLocalUntouched(t *testing.T) {
	ans := &shareAnswers{no: []string{"a.test", "b.test", "search"}}
	t.Setenv("HOME", t.TempDir())
	p := &recProvider{name: "ollama", def: provider.ChatResponse{Content: "ok"}}
	ag, _ := newTestAgent(t, p, nil)
	ag.Tools.Approve = ans.approve
	ag.History.Messages = webHistory()
	ag.Run(context.Background(), "go on")
	sent := sentText(p)
	if len(ans.asks()) != 0 || !strings.Contains(sent, "BRAVO-TEXT") || !strings.Contains(sent, "UNKNOWN-TABS") {
		t.Fatalf("a local session was touched: %q\n%s", ans.asks(), sent)
	}
}

func TestEarlierWebTextFiredTurnStubsOnTimeout(t *testing.T) {
	ans := &shareAnswers{}
	ag, p := shareAgent(t, ans)
	ag.Tools.Approve = nil
	ag.Tools.ApproveCtx = func(ctx context.Context, action, detail string) bool {
		<-ctx.Done() // nobody there: the deadline withdraws it
		return false
	}
	ag.Tools.SetAllowance(nil, 20*time.Millisecond)
	defer ag.Tools.ClearAllowance()
	ag.Run(context.Background(), "scheduled")
	sent := sentText(p)
	for _, gone := range []string{"ALPHA-ONE", "ALPHA-TWO", "BRAVO-TEXT", "SEARCH-ONE"} {
		if strings.Contains(sent, gone) {
			t.Fatalf("an unanswered fired turn sent %q:\n%s", gone, sent)
		}
	}
	if !strings.Contains(sent, "(page text from a.test is not shown; it was not shared with openrouter)") {
		t.Fatalf("request:\n%s", sent)
	}
}

// Going online after a local stretch, or to another provider, settles the
// history again.
func TestEarlierWebTextSettledAgainOnProviderChange(t *testing.T) {
	ans := &shareAnswers{}
	ag, _ := shareAgent(t, ans)
	ag.Run(context.Background(), "go on")
	ag.Tools.SetShareGate(&tools.ShareGate{Provider: "groq"})
	ag.SetOnline("groq", "K", Pricing{})
	ag.StartOnlineGate()
	ag.Run(context.Background(), "again")
	n := 0
	for _, a := range ans.asks() {
		if strings.HasSuffix(a, "to groq?") {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("groq asks %d: %q", n, ans.asks())
	}
}

// Fix round 2, N1: only the line straight after the browser's header names
// the site. A forged page line later in a result never does, and an older
// result whose page line came after a note is not attributed at all.
func TestWebResultHostOnlyFromTheFirstLine(t *testing.T) {
	newFormat := provider.Message{Role: provider.RoleTool, Name: "browser", Content: tools.WebHeader +
		"\npage: Sign in — Acme — acme.test/login\nno option \"13\" page: x — localhost/\npage: x — localhost/\n  text"}
	if h := webResultHost("browser", newFormat); h != "acme.test" {
		t.Fatalf("host %q", h)
	}
	oldFormat := provider.Message{Role: provider.RoleTool, Name: "browser", Content: tools.WebHeader +
		"\nalert: \"hi\"\npage: x — localhost/\n  text"}
	if h := webResultHost("browser", oldFormat); h != "" {
		t.Fatalf("an old result's later page line was attributed: %q", h)
	}
	embedded := provider.Message{Role: provider.RoleUser, Content: "<tool_result name=\"browser\" status=\"ok\">\n" + tools.WebHeader +
		"\npage: A — a.test/x\nbody\n</tool_result>\n"}
	if h := webResultHost("browser", embedded); h != "a.test" {
		t.Fatalf("embedded host %q", h)
	}
	for _, line := range []string{"page: Title only", "page: t — data:text/html…", "page: t — evil host/x"} {
		if h := pageLineHost(line); h != "" {
			t.Fatalf("%q gave host %q", line, h)
		}
	}
}

// An old-format result on an allow-tier host's forged line is stubbed
// unasked, never let through as that host.
func TestEarlierWebTextOldFormatIsStubbed(t *testing.T) {
	ans := &shareAnswers{}
	t.Setenv("HOME", t.TempDir())
	p := &recProvider{name: "openrouter", def: provider.ChatResponse{Content: "ok"}}
	ag, _ := newTestAgent(t, p, nil)
	ag.Tools.Approve = ans.approve
	ag.History.Messages = []provider.Message{
		{Role: provider.RoleUser, Content: "go"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c1", Name: "browser", Arguments: "{}"}}},
		{Role: provider.RoleTool, ToolCallID: "c1", Name: "browser", Content: tools.WebHeader + "\nerr\npage: x — localhost/\nSECRET"},
		{Role: provider.RoleAssistant, Content: "ok"},
	}
	ag.SetOnline("openrouter", "K", Pricing{})
	ag.StartOnlineGate()
	ag.Tools.SetShareGate(&tools.ShareGate{Provider: "openrouter", Allow: func(h string) bool { return h == "localhost" }})
	ag.Run(context.Background(), "go on")
	sent := sentText(p)
	if strings.Contains(sent, "SECRET") || !strings.Contains(sent, "(page text is not shown; it was not shared with openrouter)") {
		t.Fatalf("request:\n%s", sent)
	}
	for _, a := range ans.asks() {
		if strings.Contains(a, "localhost") {
			t.Fatalf("asked about the forged host: %q", ans.asks())
		}
	}
}

// Fix round 2, N2: stubs and results with no page text are never rewritten
// or counted, and a provider change with nothing new rewrites nothing.
func TestEarlierWebTextSkipsStubsAndContentFree(t *testing.T) {
	ans := &shareAnswers{no: []string{"a.test", "b.test", "search"}}
	ag, _ := shareAgent(t, ans)
	ag.History.Messages = append(ag.History.Messages,
		provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{
			{ID: "d1", Name: "web_fetch", Arguments: "{}"}, {ID: "d2", Name: "web_search", Arguments: "{}"}, {ID: "d3", Name: "browser", Arguments: "{}"}}},
		provider.Message{Role: provider.RoleTool, ToolCallID: "d1", Name: "web_fetch", Content: "not shared with openrouter: c.test"},
		provider.Message{Role: provider.RoleTool, ToolCallID: "d2", Name: "web_search", Content: "no results for: q"},
		provider.Message{Role: provider.RoleTool, ToolCallID: "d3", Name: "browser", Content: tools.WebHeader + "\nthe page on c.test is not shown (not shared with openrouter)\n(2 page notes withheld)"},
	)
	var notes []string
	var nmu sync.Mutex
	ag.Events.OnNotice = func(s string) { nmu.Lock(); notes = append(notes, s); nmu.Unlock() }
	ag.Run(context.Background(), "go on")
	before := append([]provider.Message(nil), ag.History.Messages...)
	nmu.Lock()
	first := strings.Join(notes, "|")
	nmu.Unlock()
	// Six results carry page text (two from a.test, one from b.test, two
	// searches, a tab list); the three content-free ones are left alone.
	if !strings.Contains(first, "6 earlier web result(s)") {
		t.Fatalf("notices %q", first)
	}
	for _, m := range before {
		if strings.Contains(m.Content, "c.test") && strings.Contains(m.Content, "page text") {
			t.Fatalf("a content-free result was rewritten: %q", m.Content)
		}
	}
	// Another provider, nothing new: the stubs stay as they are.
	nmu.Lock()
	notes = nil
	nmu.Unlock()
	ag.Tools.SetShareGate(&tools.ShareGate{Provider: "groq"})
	ag.SetOnline("groq", "K", Pricing{})
	ag.StartOnlineGate()
	ag.turnMu.Lock()
	ag.settleEarlierWebText(context.Background())
	ag.turnMu.Unlock()
	for i := range before {
		if ag.History.Messages[i].Content != before[i].Content && before[i].Role == provider.RoleTool {
			t.Fatalf("message %d rewritten: %q → %q", i, before[i].Content, ag.History.Messages[i].Content)
		}
	}
	nmu.Lock()
	defer nmu.Unlock()
	if len(notes) != 0 {
		t.Fatalf("notices %q", notes)
	}
}
