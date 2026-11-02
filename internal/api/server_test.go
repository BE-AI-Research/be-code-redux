// internal/api/server_test.go
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeBackend answers everything from fields; tests set what they need.
type fakeBackend struct {
	token    string
	queries  []QueryRequest
	complete []CompletionRequest
	answer   string
	err      error
	packs    map[string]bool
	block    chan struct{} // when set, Query waits on it (or ctx)
}

func (f *fakeBackend) Health() Health {
	return Health{Version: "test", SessionCode: "abc123", Model: "m"}
}
func (f *fakeBackend) Query(ctx context.Context, q QueryRequest) (QueryResponse, error) {
	f.queries = append(f.queries, q)
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return QueryResponse{}, ctx.Err()
		}
	}
	if f.err != nil {
		return QueryResponse{}, f.err
	}
	return QueryResponse{Text: f.answer, Model: "m", Usage: Usage{PromptTokens: 3, CompletionTokens: 2}}, nil
}
func (f *fakeBackend) Complete(ctx context.Context, c CompletionRequest, on func(string)) (CompletionResponse, error) {
	f.complete = append(f.complete, c)
	if f.err != nil {
		return CompletionResponse{}, f.err
	}
	for _, part := range strings.SplitAfter(f.answer, " ") {
		if on != nil {
			on(part)
		}
	}
	return CompletionResponse{Text: f.answer, Model: "m", FinishReason: "stop", Usage: Usage{PromptTokens: 3, CompletionTokens: 2}}, nil
}
func (f *fakeBackend) SessionInfo() SessionInfo {
	return SessionInfo{Title: "t", Workspace: "/ws", Model: "m"}
}
func (f *fakeBackend) AcceptsSession(tok string) bool { return tok != "" && tok == f.token }
func (f *fakeBackend) SessionQuery(ctx context.Context, q QueryRequest) (QueryResponse, error) {
	return f.Query(ctx, q)
}
func (f *fakeBackend) PackActive(name string) bool { return f.packs[name] }

type harness struct {
	srv   *Server
	ts    *httptest.Server
	reg   *Registry
	back  *fakeBackend
	log   *bytes.Buffer
	slept []time.Duration
	now   time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{back: &fakeBackend{answer: "hello there", token: "sess_1_" + strings.Repeat("A", 22)}, log: &bytes.Buffer{}}
	h.now = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	h.reg = NewRegistry(filepath.Join(t.TempDir(), "apps"))
	h.srv = New(Options{
		Backend: h.back, Apps: h.reg, Timeout: 5 * time.Second,
		Now:       func() time.Time { return h.now },
		Sleep:     func(d time.Duration) { h.slept = append(h.slept, d) },
		AccessLog: h.log,
	})
	h.ts = httptest.NewServer(h.srv.Handler())
	t.Cleanup(h.ts.Close)
	return h
}

func (h *harness) app(t *testing.T, name string, p Profile) string {
	t.Helper()
	key, err := h.reg.Add(name, p)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func (h *harness) do(t *testing.T, method, path, key, session string, body any, hdr map[string]string) (*http.Response, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.ts.URL+path, rd)
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if session != "" {
		req.Header.Set("X-BE-Session", session)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := h.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp, out
}

func errCode(m map[string]any) string {
	if e, ok := m["error"].(map[string]any); ok {
		s, _ := e["code"].(string)
		return s
	}
	return ""
}

func TestCheckOrder(t *testing.T) {
	h := newHarness(t)
	named := h.app(t, "named", Profile{Tiers: []string{"query"}, Sessions: "named", Models: []string{"other"}, Packs: []string{"ue5"}})
	none := h.app(t, "none", Profile{Tiers: []string{"completions"}, Sessions: "none"})
	h.back.packs = map[string]bool{"ue5": true}
	tok := h.back.token
	q := map[string]any{"prompt": "hi"}
	for _, tc := range []struct {
		name, key, session, path string
		body                     any
		hdr                      map[string]string
		status                   int
		code                     string
	}{
		{"browser header first", named, "", "/v1/query", q, map[string]string{"Origin": "http://x"}, 403, "browser_refused"},
		{"sec-fetch too", named, "", "/v1/query", q, map[string]string{"Sec-Fetch-Mode": "cors"}, 403, "browser_refused"},
		{"no key", "", "", "/v1/query", q, nil, 401, "unauthorized"},
		{"wrong prefix", "bad_" + strings.Repeat("A", 32), "", "/v1/query", q, nil, 401, "unauthorized"},
		{"right prefix wrong length", "bek_1_short", "", "/v1/query", q, nil, 401, "unauthorized"},
		{"right prefix bad bytes", "bek_1_" + strings.Repeat("!", 32), "", "/v1/query", q, nil, 401, "unauthorized"},
		{"tier not allowed", none, "", "/v1/query", q, nil, 403, "tier_not_allowed"},
		{"sessions none with header", none, tok, "/v1/chat/completions", map[string]any{"messages": []any{map[string]any{"role": "user", "content": "x"}}}, nil, 403, "sessions_not_allowed"},
		{"bad session token", named, "sess_1_" + strings.Repeat("B", 22), "/v1/sessions/sess_1_" + strings.Repeat("B", 22) + "/query", q, nil, 404, "session_not_found"},
		{"path and header disagree", named, tok, "/v1/sessions/sess_1_" + strings.Repeat("B", 22) + "/query", q, nil, 400, "session_mismatch"},
		{"model not allowed", named, "", "/v1/query", map[string]any{"prompt": "hi", "model": "secret"}, nil, 403, "model_not_allowed"},
		{"pack not in profile", named, "", "/v1/query", map[string]any{"prompt": "hi", "pack": "zsh"}, nil, 403, "pack_not_allowed"},
		{"max_tokens over budget", named, "", "/v1/query", map[string]any{"prompt": "hi", "max_tokens": 99999}, nil, 400, "over_budget"},
		{"ok", named, "", "/v1/query", q, nil, 200, ""},
		{"ok with session", named, tok, "/v1/sessions/" + tok + "/query", q, nil, 200, ""},
		{"named app, stateless endpoint ignores header", named, tok, "/v1/query", q, nil, 200, ""},
	} {
		resp, body := h.do(t, "POST", tc.path, tc.key, tc.session, tc.body, tc.hdr)
		if resp.StatusCode != tc.status || errCode(body) != tc.code {
			t.Errorf("%s: want %d %q, got %d %q (%v)", tc.name, tc.status, tc.code, resp.StatusCode, errCode(body), body)
		}
	}
}

// A browser-shaped request is refused before the body is read.
func TestBrowserRefusedBeforeBodyRead(t *testing.T) {
	h := newHarness(t)
	key := h.app(t, "app", Profile{Tiers: []string{"query"}})
	pr, pw := io.Pipe() // a body that blocks until written; the server must never read it
	req, _ := http.NewRequest("POST", h.ts.URL+"/v1/query", pr)
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Origin", "http://evil")
	req.ContentLength = -1
	done := make(chan *http.Response, 1)
	go func() { resp, _ := h.ts.Client().Do(req); done <- resp }()
	select {
	case resp := <-done:
		if resp == nil || resp.StatusCode != 403 {
			t.Fatalf("want 403 without reading the body, got %v", resp)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the server waited for a body it must not read")
	}
	pw.Close()
}

func TestAuthFailuresAreDelayedAndLockedOut(t *testing.T) {
	h := newHarness(t)
	for i := 0; i < 20; i++ {
		resp, _ := h.do(t, "GET", "/v1/health", "bek_1_"+strings.Repeat("A", 32), "", nil, nil)
		if resp.StatusCode != 401 {
			t.Fatalf("failure %d: %d", i, resp.StatusCode)
		}
		if !resp.Close /* net/http strips the header from the client's view and sets Close */ {
			t.Fatalf("failure %d: the connection must be closed", i)
		}
	}
	if len(h.slept) != 20 || h.slept[0] != 250*time.Millisecond {
		t.Fatalf("every failure sleeps 250ms: %v", h.slept)
	}
	key := h.app(t, "good", Profile{Tiers: []string{"query"}})
	resp, body := h.do(t, "GET", "/v1/health", key, "", nil, nil)
	if resp.StatusCode != 429 || errCode(body) != "rate_limited" {
		t.Fatalf("the 21st failure in a minute locks everyone out: %d %v", resp.StatusCode, body)
	}
	h.now = h.now.Add(61 * time.Second)
	if resp, _ := h.do(t, "GET", "/v1/health", key, "", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("after the minute a good key works: %d", resp.StatusCode)
	}
}

func TestRateLimitAndOneInFlight(t *testing.T) {
	h := newHarness(t)
	key := h.app(t, "app", Profile{Tiers: []string{"query"}, Budget: Budget{PerHour: 2, Tokens: 100}})
	for i := 0; i < 2; i++ {
		if resp, _ := h.do(t, "POST", "/v1/query", key, "", map[string]any{"prompt": "x"}, nil); resp.StatusCode != 200 {
			t.Fatal("within budget")
		}
	}
	resp, body := h.do(t, "POST", "/v1/query", key, "", map[string]any{"prompt": "x"}, nil)
	if resp.StatusCode != 429 || errCode(body) != "rate_limited" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("over per_hour: %d %v %q", resp.StatusCode, body, resp.Header.Get("Retry-After"))
	}
	// Concurrency: block the first, send a second.
	h.now = h.now.Add(2 * time.Hour)
	h.back.block = make(chan struct{})
	first := make(chan int, 1)
	go func() {
		r, _ := h.do(t, "POST", "/v1/query", key, "", map[string]any{"prompt": "slow"}, nil)
		first <- r.StatusCode
	}()
	time.Sleep(100 * time.Millisecond)
	resp, body = h.do(t, "POST", "/v1/query", key, "", map[string]any{"prompt": "x"}, nil)
	if resp.StatusCode != 429 || errCode(body) != "rate_limited" {
		t.Fatalf("a second in-flight request is 429: %d %v", resp.StatusCode, body)
	}
	close(h.back.block)
	if <-first != 200 {
		t.Fatal("the first finishes normally")
	}
}

func TestAccessLogNeverCarriesASecret(t *testing.T) {
	h := newHarness(t)
	key := h.app(t, "app", Profile{Tiers: []string{"query"}, Sessions: "named"})
	tok := h.back.token
	h.do(t, "POST", "/v1/sessions/"+tok+"/query", key, tok, map[string]any{"prompt": "x"}, nil)
	h.do(t, "GET", "/v1/health", "bek_1_"+strings.Repeat("Z", 32), "", nil, nil)
	log := h.log.String()
	if strings.Contains(log, key) || strings.Contains(log, tok) || strings.Contains(log, strings.Repeat("Z", 32)) {
		t.Fatalf("a secret reached the log:\n%s", log)
	}
	if !strings.Contains(log, "app=app") || !strings.Contains(log, "session=abc123") || !strings.Contains(log, "/v1/sessions/{token}/query") || !strings.Contains(log, "status=401") {
		t.Fatalf("the log names the app, the session code, the route template and the status:\n%s", log)
	}
}

func TestCloseAnswersInFlightWith503(t *testing.T) {
	h := newHarness(t)
	key := h.app(t, "app", Profile{Tiers: []string{"query"}})
	h.back.block = make(chan struct{})
	got := make(chan map[string]any, 1)
	go func() { _, b := h.do(t, "POST", "/v1/query", key, "", map[string]any{"prompt": "slow"}, nil); got <- b }()
	time.Sleep(100 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = h.srv.Close(ctx)
	if body := <-got; errCode(body) != "shutting_down" {
		t.Fatalf("in-flight requests are told the server is closing: %v", body)
	}
}

func TestRefusedInFlightIsNotCharged(t *testing.T) {
	h := newHarness(t)
	key := h.app(t, "app", Profile{Tiers: []string{"query"}, Budget: Budget{PerHour: 2, Tokens: 100}})
	h.back.block = make(chan struct{})
	first := make(chan int, 1)
	go func() {
		r, _ := h.do(t, "POST", "/v1/query", key, "", map[string]any{"prompt": "slow"}, nil)
		first <- r.StatusCode
	}()
	time.Sleep(100 * time.Millisecond)
	resp, body := h.do(t, "POST", "/v1/query", key, "", map[string]any{"prompt": "x"}, nil)
	if resp.StatusCode != 429 || errCode(body) != "rate_limited" {
		t.Fatalf("second in-flight request: %d %v", resp.StatusCode, body)
	}
	close(h.back.block)
	if <-first != 200 {
		t.Fatal("the first finishes normally")
	}
	for i := 0; i < 1; i++ { // the slow one was charged once; one more fits, the refusal was free
		if resp, _ := h.do(t, "POST", "/v1/query", key, "", map[string]any{"prompt": "x"}, nil); resp.StatusCode != 200 {
			t.Fatalf("request %d after the refusal: %d", i, resp.StatusCode)
		}
	}
	if resp, _ := h.do(t, "POST", "/v1/query", key, "", map[string]any{"prompt": "x"}, nil); resp.StatusCode != 429 {
		t.Fatalf("the hour holds exactly two charged requests: %d", resp.StatusCode)
	}
}

func TestUnmatchedRoutesAreJSON404AfterAuth(t *testing.T) {
	h := newHarness(t)
	key := h.app(t, "app", Profile{Tiers: []string{"query"}})
	for _, tc := range []struct{ method, path string }{{"GET", "/v1/nope"}, {"PUT", "/v1/query"}} {
		resp, body := h.do(t, tc.method, tc.path, key, "", nil, nil)
		if resp.StatusCode != 404 || errCode(body) != "not_found" {
			t.Errorf("%s %s: %d %v", tc.method, tc.path, resp.StatusCode, body)
		}
	}
	for _, tc := range []struct{ method, path string }{{"GET", "/v1/nope"}, {"PUT", "/v1/query"}} {
		if resp, body := h.do(t, tc.method, tc.path, "", "", nil, nil); resp.StatusCode != 401 || errCode(body) != "unauthorized" {
			t.Errorf("unauthenticated %s %s: %d %v", tc.method, tc.path, resp.StatusCode, body)
		}
	}
	if !strings.Contains(h.log.String(), "route=-") {
		t.Fatalf("unmatched requests are logged:\n%s", h.log.String())
	}
}

func TestBrowserRefusalIsLogged(t *testing.T) {
	h := newHarness(t)
	h.do(t, "POST", "/v1/query", "", "", map[string]any{"prompt": "x"}, map[string]string{"Origin": "http://x"})
	if l := h.log.String(); !strings.Contains(l, "app=- ") || !strings.Contains(l, "route=/v1/query") || !strings.Contains(l, "status=403") {
		t.Fatalf("a browser-refused request is logged:\n%s", l)
	}
}
