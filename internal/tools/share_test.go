package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/provider"
)

// shareAsks is every share_page question the log holds, in order.
func (l *askLog) shareAsks() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for i, a := range l.actions {
		if a == "share_page" {
			out = append(out, l.details[i])
		}
	}
	return out
}

// shareApprover answers share_page by host (yes unless the question names
// a host in no) and every other question yes.
func (l *askLog) shareApprover(no ...string) ApproveFunc {
	return func(action, detail string) bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.actions = append(l.actions, action)
		l.details = append(l.details, detail)
		if action == "share_page" {
			for _, h := range no {
				if strings.Contains(detail, " on "+h+" ") {
					return false
				}
			}
		}
		return true
	}
}

const formText = `textbox "Email" [e1]`

func TestSharePageAskedOncePerHost(t *testing.T) {
	var log askLog
	reg, bt, _, _ := browserFixture(t, "https://acme.test/login", nil, log.approver(true))
	reg.SetShareGate(&ShareGate{Provider: "openrouter"})
	for i := 0; i < 3; i++ {
		if res := do(bt, map[string]any{"action": "snapshot"}); res.IsError || !strings.Contains(res.Content, formText) {
			t.Fatalf("snapshot %d:\n%s", i, res.Content)
		}
	}
	asks := log.shareAsks()
	if len(asks) != 1 || asks[0] != "Send what the agent reads on acme.test to openrouter?" {
		t.Fatalf("share asks %q", asks)
	}
	if got := bt.PageURL(); got != "https://acme.test/login" {
		t.Fatalf("a shared page is recorded in full: %q", got)
	}
	res := do(bt, map[string]any{"action": "tabs"})
	if !strings.Contains(res.Content, "1. Sign in — Acme — acme.test/login (current)") {
		t.Fatalf("a shared host's tab keeps its title:\n%s", res.Content)
	}
}

func TestSharePageEveryTimeInMyChrome(t *testing.T) {
	var log askLog
	reg, bt, _, _ := myChromeFixture(t, nil, log.approver(true))
	reg.SetShareGate(&ShareGate{Provider: "openrouter"})
	for _, args := range []map[string]any{
		{"action": "open", "url": "https://acme.test/login"}, {"action": "snapshot"}, {"action": "read"},
	} {
		if res := do(bt, args); res.IsError {
			t.Fatalf("%v: %s", args, res.Content)
		}
	}
	asks := log.shareAsks()
	if len(asks) != 3 {
		t.Fatalf("every page in your own Chrome asks: %q", asks)
	}
	for _, a := range asks {
		if a != "Send what the agent reads on acme.test to openrouter?" {
			t.Fatalf("question %q", a)
		}
	}
	if reg.shareGranted("acme.test") {
		t.Fatal("my-Chrome mode granted a host for the session")
	}
	// The tab list in your own Chrome shows only the allow tier's titles,
	// whatever was shared.
	res := do(bt, map[string]any{"action": "tabs"})
	if strings.Contains(res.Content, "Sign in — Acme") {
		t.Fatalf("a title outside the allow tier was listed:\n%s", res.Content)
	}
}

func TestSharePageAllowTierSkips(t *testing.T) {
	var log askLog
	reg, bt, _, _ := browserFixture(t, "https://acme.test/login", map[string]string{"acme.test": "allow"}, log.approver(false))
	reg.SetShareGate(&ShareGate{Provider: "openrouter"})
	if res := do(bt, map[string]any{"action": "snapshot"}); res.IsError || !strings.Contains(res.Content, formText) {
		t.Fatalf("snapshot:\n%s", res.Content)
	}
	res := do(bt, map[string]any{"action": "tabs"})
	if !strings.Contains(res.Content, "Sign in — Acme") {
		t.Fatalf("an allow-tier tab keeps its title:\n%s", res.Content)
	}
	if log.count() != 0 {
		t.Fatalf("asked %v", log.actions)
	}
}

func TestSharePageRefusedWithholds(t *testing.T) {
	var log askLog
	reg, bt, _, _ := browserFixture(t, "https://acme.test/login", nil, log.approver(false))
	reg.SetShareGate(&ShareGate{Provider: "openrouter"})
	for _, act := range []string{"snapshot", "read"} {
		res := do(bt, map[string]any{"action": act})
		if strings.Contains(res.Content, formText) || strings.Contains(res.Content, "Sign in — Acme") {
			t.Fatalf("%s: a refused page was shown:\n%s", act, res.Content)
		}
		if !strings.HasPrefix(res.Content, WebHeader+"\n") || !strings.Contains(res.Content, "the page on acme.test is not shown (not shared with openrouter)") {
			t.Fatalf("%s: result:\n%s", act, res.Content)
		}
	}
	if n := len(log.shareAsks()); n != 2 {
		t.Fatalf("a refusal is not remembered as a grant either way; asks %d", n)
	}
	if got := bt.PageURL(); got != "acme.test" {
		t.Fatalf("working memory records a withheld page by host alone, got %q", got)
	}
	res := do(bt, map[string]any{"action": "tabs"})
	if strings.Contains(res.Content, "Sign in — Acme") || strings.Contains(res.Content, "/login") || !strings.Contains(res.Content, "1. acme.test (current)") {
		t.Fatalf("an unshared tab is listed by host alone:\n%s", res.Content)
	}
}

func TestSharePageJudgedOnFinalHost(t *testing.T) {
	var log askLog
	reg, bt, ps, _ := browserFixture(t, "https://a.example/", nil, log.shareApprover("b.example"))
	ps.Lock()
	ps.Pages["https://b.example/landing"] = [2]string{"Bee", `{"nodes":[{"nodeId":"1","role":{"value":"RootWebArea"},"name":{"value":"Bee"},"childIds":["2"]},{"nodeId":"2","role":{"value":"heading"},"name":{"value":"Bee secrets"},"backendDOMNodeId":5}]}`}
	ps.Redirects = map[string]string{"https://a.example/next": "https://b.example/landing"}
	ps.Unlock()
	reg.SetShareGate(&ShareGate{Provider: "openrouter"})
	if res := do(bt, map[string]any{"action": "snapshot"}); !strings.Contains(res.Content, formText) {
		t.Fatalf("a.example is shared:\n%s", res.Content)
	}
	res := do(bt, map[string]any{"action": "open", "url": "https://a.example/next"})
	if strings.Contains(res.Content, "Bee secrets") || !strings.Contains(res.Content, "the page on b.example is not shown (not shared with openrouter)") {
		t.Fatalf("redirected page:\n%s", res.Content)
	}
	asks := log.shareAsks()
	if len(asks) != 2 || !strings.Contains(asks[0], " on a.example ") || asks[1] != "Send what the agent reads on b.example to openrouter?" {
		t.Fatalf("asks %q", asks)
	}
}

func TestWebFetchShareGate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/r" {
			http.Redirect(w, r, "http://localhost:"+r.Host[strings.LastIndex(r.Host, ":")+1:]+"/page", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("secret page text"))
	}))
	defer srv.Close()
	fetch := func(reg *Registry, u string) Result {
		return reg.Dispatch(context.Background(), provider.ToolCall{Name: "web_fetch", Arguments: `{"url":"` + u + `"}`})
	}
	// 127.0.0.1 is in the watch tier here (a rule beats loopback); localhost
	// stays loopback, so allow.
	sites := map[string]string{"127.0.0.1": "watch"}

	var no askLog
	reg, _ := NewRegistry(t.TempDir(), no.approver(false))
	reg.AddTool(NewWebFetch(sites))
	reg.SetShareGate(&ShareGate{Provider: "openrouter"})
	res := fetch(reg, srv.URL+"/page")
	if !res.IsError || res.Content != "not shared with openrouter: 127.0.0.1" {
		t.Fatalf("refused fetch: %+v", res)
	}
	if reg.UntrustedWeb() {
		t.Fatal("a page nobody was shown marked the request untrusted")
	}
	// Judged on the final host: a redirect to an allow-tier host asks nothing.
	if res := fetch(reg, srv.URL+"/r"); res.IsError || !strings.Contains(res.Content, "secret page text") {
		t.Fatalf("redirect to allow tier: %+v", res)
	}
	if n := len(no.shareAsks()); n != 1 {
		t.Fatalf("asks %d", n)
	}

	var yes askLog
	reg2, _ := NewRegistry(t.TempDir(), yes.approver(true))
	reg2.AddTool(NewWebFetch(map[string]string{"127.0.0.1": "watch", "localhost": "watch"}))
	reg2.SetShareGate(&ShareGate{Provider: "openrouter"})
	for i := 0; i < 2; i++ {
		if res := fetch(reg2, srv.URL+"/page"); res.IsError || !strings.Contains(res.Content, "secret page text") {
			t.Fatalf("approved fetch: %+v", res)
		}
	}
	fetch(reg2, srv.URL+"/r")
	asks := yes.shareAsks()
	if len(asks) != 2 || asks[0] != "Send what the agent reads on 127.0.0.1 to openrouter?" || asks[1] != "Send what the agent reads on localhost to openrouter?" {
		t.Fatalf("asks %q", asks)
	}

	// Reached through a Subset, the parent's gate and grants still apply.
	sub := reg2.Subset("web_fetch")
	if res := fetch(sub, srv.URL+"/page"); res.IsError {
		t.Fatalf("subset: %+v", res)
	}
	if len(yes.shareAsks()) != 2 {
		t.Fatal("a subset did not see the parent's grant")
	}
	var log3 askLog
	reg3, _ := NewRegistry(t.TempDir(), log3.approver(false))
	reg3.AddTool(NewWebFetch(sites))
	reg3.SetShareGate(&ShareGate{Provider: "openrouter"})
	if res := fetch(reg3.Subset("web_fetch"), srv.URL+"/page"); !res.IsError || !strings.Contains(res.Content, "not shared with openrouter") {
		t.Fatalf("a subset bypassed the gate: %+v", res)
	}
}

func TestWebSearchShareOncePerSession(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"items":[{"title":"Go slices","link":"https://go.dev/blog/slices","snippet":"Slices are views."}]}`))
	}))
	defer srv.Close()
	t.Setenv("TEST_PSE_KEY", "k123")
	search := func(reg *Registry) Result {
		return reg.Dispatch(context.Background(), provider.ToolCall{Name: "web_search", Arguments: `{"query":"go slices"}`})
	}
	var log askLog
	reg, _ := NewRegistry(t.TempDir(), log.approver(true))
	reg.AddTool(NewWebSearch(WebSearchConfig{CX: "cx", APIKeyEnv: "TEST_PSE_KEY", Endpoint: srv.URL}))
	reg.SetShareGate(&ShareGate{Provider: "openrouter"})
	for i := 0; i < 3; i++ {
		if res := search(reg); res.IsError || !strings.Contains(res.Content, "Slices are views.") {
			t.Fatalf("search: %+v", res)
		}
	}
	if asks := log.shareAsks(); len(asks) != 1 || asks[0] != "Send web search results to openrouter?" {
		t.Fatalf("asks %q", asks)
	}

	var no askLog
	reg2, _ := NewRegistry(t.TempDir(), no.approver(false))
	reg2.AddTool(NewWebSearch(WebSearchConfig{CX: "cx", APIKeyEnv: "TEST_PSE_KEY", Endpoint: srv.URL}))
	reg2.SetShareGate(&ShareGate{Provider: "openrouter"})
	res := search(reg2)
	if !res.IsError || strings.Contains(res.Content, "Slices") || res.Content != "not shared with openrouter: web search results" {
		t.Fatalf("refused search: %+v", res)
	}
}

func TestShareGateNilWhenLocal(t *testing.T) {
	var log askLog
	reg, bt, _, _ := browserFixture(t, "https://acme.test/login", nil, log.approver(false))
	if res := do(bt, map[string]any{"action": "snapshot"}); !strings.Contains(res.Content, formText) {
		t.Fatalf("snapshot:\n%s", res.Content)
	}
	if got := bt.PageURL(); got != "https://acme.test/login" {
		t.Fatalf("page url %q", got)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("page")) }))
	defer srv.Close()
	reg.AddTool(NewWebFetch(map[string]string{"127.0.0.1": "watch"}))
	if res := reg.Dispatch(context.Background(), provider.ToolCall{Name: "web_fetch", Arguments: `{"url":"` + srv.URL + `"}`}); res.IsError {
		t.Fatalf("fetch: %+v", res)
	}
	// A gate set and then cleared (a switch to a local model) asks nothing
	// either, and keeps no grant for a later online provider.
	reg.SetShareGate(&ShareGate{Provider: "openrouter"})
	reg.SetShareGate(nil)
	do(bt, map[string]any{"action": "snapshot"})
	if log.count() != 0 {
		t.Fatalf("a local main model was asked %v", log.actions)
	}
}

func TestSharePageFiredTurnIgnoresSessionGrant(t *testing.T) {
	var log askLog
	reg, bt, _, _ := browserFixture(t, "https://acme.test/login", nil, log.approver(true))
	reg.SetShareGate(&ShareGate{Provider: "openrouter"})
	do(bt, map[string]any{"action": "snapshot"})
	if n := len(log.shareAsks()); n != 1 {
		t.Fatalf("asks %d", n)
	}
	reg.SetFiredPolicy(nil, time.Minute, true) // inheriting or not, share_page takes no shortcut
	if res := do(bt, map[string]any{"action": "snapshot"}); !strings.Contains(res.Content, formText) {
		t.Fatalf("approved in the fired turn:\n%s", res.Content)
	}
	if res := do(bt, map[string]any{"action": "tabs"}); strings.Contains(res.Content, "Sign in — Acme") {
		t.Fatalf("a fired turn listed a title on a session grant:\n%s", res.Content)
	}
	reg.ClearAllowance()
	if n := len(log.shareAsks()); n != 2 {
		t.Fatalf("a fired turn used the session grant: asks %d", n)
	}

	// A yes given inside a fired turn is not kept for the session.
	var log2 askLog
	reg2, bt2, _, _ := browserFixture(t, "https://acme.test/login", nil, log2.approver(true))
	reg2.SetShareGate(&ShareGate{Provider: "openrouter"})
	reg2.SetAllowance(nil, time.Minute)
	do(bt2, map[string]any{"action": "snapshot"})
	reg2.ClearAllowance()
	do(bt2, map[string]any{"action": "snapshot"})
	if n := len(log2.shareAsks()); n != 2 {
		t.Fatalf("a fired turn's yes was kept: asks %d", n)
	}
}

// A grant was given for one provider: a switch to another starts afresh,
// and a cancelled question is a refusal, never a grant.
func TestShareGrantsResetOnProviderChangeAndCancelRefuses(t *testing.T) {
	var log askLog
	reg, bt, _, _ := browserFixture(t, "https://acme.test/login", nil, log.approver(true))
	reg.SetShareGate(&ShareGate{Provider: "openrouter"})
	do(bt, map[string]any{"action": "snapshot"})
	reg.SetShareGate(&ShareGate{Provider: "openrouter"}) // a /model switch on the same provider keeps it
	do(bt, map[string]any{"action": "snapshot"})
	reg.SetShareGate(&ShareGate{Provider: "groq"})
	do(bt, map[string]any{"action": "snapshot"})
	asks := log.shareAsks()
	if len(asks) != 2 || asks[1] != "Send what the agent reads on acme.test to groq?" {
		t.Fatalf("asks %q", asks)
	}

	ctx, cancel := context.WithCancel(context.Background())
	reg2, _ := NewRegistry(t.TempDir(), nil)
	reg2.ApproveCtx = func(c context.Context, action, detail string) bool { cancel(); return true }
	reg2.SetShareGate(&ShareGate{Provider: "openrouter"})
	if reg2.shareOK(ctx, "acme.test", false) || reg2.shareGranted("acme.test") {
		t.Fatal("a cancelled question was taken as a yes")
	}
}

// Fix round 1, item 1: an action's error on a page not shared carries
// nothing of the page — a select's "no option" lists every option.
func TestSharePageRefusedHidesActionErrorText(t *testing.T) {
	var log askLog
	reg, bt, ps, _ := browserFixture(t, "https://acme.test/login", nil, log.shareApprover("acme.test"))
	reg.SetShareGate(&ShareGate{Provider: "openrouter"})
	do(bt, map[string]any{"action": "snapshot"}) // withheld, but the refs are the page's
	ps.Lock()
	ps.SelectResult = `no option "13"; the options are: Secret Option A, Secret Option B`
	ps.Unlock()
	res := do(bt, map[string]any{"action": "select", "ref": "e1", "value": "13"})
	if strings.Contains(res.Content, "Secret Option") || !strings.Contains(res.Content, "the action failed") ||
		!strings.Contains(res.Content, "the page on acme.test is not shown (not shared with openrouter)") {
		t.Fatalf("result:\n%s", res.Content)
	}
	// A ref the model gave is its own: that error still reads as before.
	res = do(bt, map[string]any{"action": "click", "ref": "e99"})
	if !strings.Contains(res.Content, "e99 is not an element on this page") {
		t.Fatalf("unknown ref:\n%s", res.Content)
	}
}

// Fix round 1, item 2: a declined interaction on a page not shared names
// the action and ref only; the person's question keeps the label.
func TestSharePageDeclineNamesRefOnly(t *testing.T) {
	var log askLog
	reg, bt, _, _ := browserFixture(t, "https://acme.test/login", nil, log.approver(false))
	reg.SetShareGate(&ShareGate{Provider: "openrouter"})
	do(bt, map[string]any{"action": "snapshot"})
	res := do(bt, map[string]any{"action": "click", "ref": "e4"})
	if !res.IsError || res.Content != "the user declined: click e4 on acme.test" {
		t.Fatalf("result %q", res.Content)
	}
	res = do(bt, map[string]any{"action": "type", "ref": "e1", "text": "x"})
	if res.Content != "the user declined: type into e1 on acme.test" {
		t.Fatalf("result %q", res.Content)
	}
	log.mu.Lock()
	prompt := log.details[len(log.details)-2]
	log.mu.Unlock()
	if !strings.Contains(prompt, `click button "Sign in" [e4]`) {
		t.Fatalf("the person's question lost the label:\n%s", prompt)
	}

	// Shared, the label is the model's to read again.
	var log2 askLog
	reg2, bt2, _, _ := browserFixture(t, "https://acme.test/login", nil, func(action, detail string) bool {
		log2.approver(action == "share_page")(action, detail)
		return action == "share_page"
	})
	reg2.SetShareGate(&ShareGate{Provider: "openrouter"})
	do(bt2, map[string]any{"action": "snapshot"})
	if res := do(bt2, map[string]any{"action": "click", "ref": "e4"}); res.Content != `the user declined: click button "Sign in" [e4] on acme.test` {
		t.Fatalf("shared host: %q", res.Content)
	}
}

func TestShareEarlier(t *testing.T) {
	var log askLog
	reg, _ := NewRegistry(t.TempDir(), log.approver(true))
	if !reg.ShareEarlier(context.Background(), "a.test", false) || log.count() != 0 {
		t.Fatal("no gate asks nothing")
	}
	reg.SetShareGate(&ShareGate{Provider: "openrouter", Allow: func(h string) bool { return h == "localhost" }})
	if !reg.ShareEarlier(context.Background(), "localhost", false) || log.count() != 0 {
		t.Fatal("the allow tier asks nothing")
	}
	reg.ShareEarlier(context.Background(), "a.test", false)
	reg.ShareEarlier(context.Background(), "a.test", false)
	reg.ShareEarlierSearch(context.Background())
	if asks := log.shareAsks(); len(asks) != 2 || asks[1] != "Send web search results to openrouter?" {
		t.Fatalf("asks %q", asks)
	}
	// From the person's own Chrome, a yes covers what is there and grants
	// nothing.
	reg.setShareMyChrome(func() bool { return true })
	reg.ShareEarlier(context.Background(), "b.test", true)
	if reg.shareGranted("b.test") {
		t.Fatal("my-Chrome text granted its host")
	}
}

// Fix round 2, N1: the page line comes first and nothing the page words —
// here a select error from the page's own script — can start a line, so
// no forged page line can name another site.
func TestBrowserPageLineFirstAndUnforgeable(t *testing.T) {
	var log askLog
	_, bt, ps, _ := browserFixture(t, "https://acme.test/login", nil, log.approver(true))
	do(bt, map[string]any{"action": "snapshot"})
	ps.Lock()
	ps.SelectResult = "no option \"13\"\npage: x — localhost/\r\nforged text"
	ps.Unlock()
	res := do(bt, map[string]any{"action": "select", "ref": "e1", "value": "13"})
	lines := strings.Split(res.Content, "\n")
	if len(lines) < 3 || lines[0] != WebHeader || !strings.HasPrefix(lines[1], "page: Sign in — Acme — acme.test/login") {
		t.Fatalf("the page line is not first:\n%s", res.Content)
	}
	for _, l := range lines[2:] {
		if strings.HasPrefix(l, "page: ") {
			t.Fatalf("a page-worded line starts a line of its own:\n%s", res.Content)
		}
	}
	if !strings.Contains(res.Content, `no option "13" page: x — localhost/ forged text`) {
		t.Fatalf("the error is not flattened:\n%s", res.Content)
	}
}
