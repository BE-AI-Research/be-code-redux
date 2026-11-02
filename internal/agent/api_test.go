// internal/agent/api_test.go
package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/api"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/packs"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/store"
	"os"
)

func apiAgent(t *testing.T, replies ...string) (*Agent, *scriptedProvider) {
	t.Helper()
	var rs []provider.ChatResponse
	for _, r := range replies {
		rs = append(rs, provider.ChatResponse{Content: r, Usage: provider.Usage{PromptTokens: 5, CompletionTokens: 2}})
	}
	p := &scriptedProvider{responses: rs}
	ag, _ := newTestAgent(t, p, func(c *config.Config) { c.RepoMap = false })
	ag.Session = store.NewSession("scripted", "test-model", ag.Tools.Root)
	return ag, p
}

func TestQueryIsToolLessAndHistoryLess(t *testing.T) {
	ag, p := apiAgent(t, "42")
	ag.History.Messages = []provider.Message{{Role: provider.RoleUser, Content: "secret earlier turn"}}
	b := NewAPIBackend(ag, "1.3.1")
	resp, err := b.Query(context.Background(), api.QueryRequest{Prompt: "what is 6*7?", System: "be terse", MaxTokens: 50})
	if err != nil || resp.Text != "42" || resp.Model != "test-model" {
		t.Fatalf("%v %+v", err, resp)
	}
	req := p.lastReq
	if len(req.Tools) != 0 || req.MaxTokens != 50 || !req.NoThink {
		t.Fatalf("a query carries no tools, the cap, and NoThink: %+v", req)
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != provider.RoleSystem || req.Messages[0].Content != "be terse" || req.Messages[1].Content != "what is 6*7?" {
		t.Fatalf("messages: %+v", req.Messages)
	}
	for _, m := range req.Messages {
		if strings.Contains(m.Content, "secret earlier turn") {
			t.Fatal("a stateless query must not see the session's history")
		}
	}
	if len(ag.History.Messages) != 1 {
		t.Fatal("a query never appends to the session")
	}
	if resp.Usage.PromptTokens != 5 || resp.Usage.CompletionTokens != 2 {
		t.Fatalf("usage: %+v", resp.Usage)
	}
}

func TestQueryPrependsThePackRulesAndReportsIt(t *testing.T) {
	ag, p := apiAgent(t, "ok")
	ag.EnablePacks([]*packs.Pack{loadTestPack(t, "ue5", "always use UPROPERTY", "", packs.SourceGlobal)})
	b := NewAPIBackend(ag, "1.3.1")
	if !b.PackActive("ue5") || b.PackActive("zsh") {
		t.Fatal("PackActive reports the active packs")
	}
	resp, err := b.Query(context.Background(), api.QueryRequest{Prompt: "q", System: "mine", Pack: "ue5"})
	if err != nil || resp.Pack == nil || *resp.Pack != "ue5" {
		t.Fatalf("%v %+v", err, resp)
	}
	sys := p.lastReq.Messages[0].Content
	if !strings.Contains(sys, "always use UPROPERTY") || !strings.HasSuffix(strings.TrimSpace(sys), "mine") {
		t.Fatalf("pack rules come first, the caller's system text last:\n%s", sys)
	}
	ag.EnablePacks(nil)
	resp, err = b.Query(context.Background(), api.QueryRequest{Prompt: "q", Pack: "ue5"})
	if err != nil || resp.Pack != nil {
		t.Fatalf("a pack gone inactive runs without it and says pack: null, got %v %+v", err, resp)
	}
}

func TestCompleteStreamsAndStripsThink(t *testing.T) {
	ag, _ := apiAgent(t)
	ag.Profile.StripThink = true
	ag.Provider = &scriptedProvider{responses: []provider.ChatResponse{{Content: "<think>hmm</think>answer", FinishReason: "stop"}}}
	b := NewAPIBackend(ag, "1.3.1")
	var deltas []string
	resp, err := b.Complete(context.Background(), api.CompletionRequest{Messages: []api.ChatMessage{{Role: "user", Content: "hi"}}, MaxTokens: 10}, func(d string) { deltas = append(deltas, d) })
	if err != nil || resp.Text != "answer" || resp.FinishReason != "stop" {
		t.Fatalf("%v %+v", err, resp)
	}
	if got := strings.Join(deltas, ""); got != "answer" {
		t.Fatalf("streamed deltas must be think-stripped, got %q", got)
	}
}

func TestSessionQueryUsesASnapshotAndWithholdsWebText(t *testing.T) {
	ag, p := apiAgent(t, "you were reading page X")
	ag.History.System = provider.Message{Role: provider.RoleSystem, Content: "the session prompt"}
	ag.History.Messages = []provider.Message{
		{Role: provider.RoleUser, Content: "fetch the docs"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "1", Name: "web_fetch", Arguments: `{"url":"http://x"}`}}},
		{Role: provider.RoleTool, ToolCallID: "1", Name: "web_fetch", Content: "SECRET PAGE TEXT"},
		{Role: provider.RoleAssistant, Content: "done"},
	}
	before := append([]provider.Message(nil), ag.History.Messages...)
	b := NewAPIBackend(ag, "1.3.1")
	resp, err := b.SessionQuery(context.Background(), api.QueryRequest{Prompt: "what was I doing?", System: "answer in one line", MaxTokens: 20})
	if err != nil || resp.Text == "" {
		t.Fatalf("%v %+v", err, resp)
	}
	req := p.lastReq
	if req.Messages[0].Content != "the session prompt\n\nanswer in one line" {
		t.Fatalf("the session's system prompt first, the caller's appended: %q", req.Messages[0].Content)
	}
	for _, m := range req.Messages {
		if strings.Contains(m.Content, "SECRET PAGE TEXT") {
			t.Fatal("web page text must be withheld")
		}
	}
	if last := req.Messages[len(req.Messages)-1]; last.Role != provider.RoleUser || last.Content != "what was I doing?" {
		t.Fatalf("the prompt is the final user message: %+v", last)
	}
	if len(ag.History.Messages) != len(before) || ag.History.Messages[2].Content != "SECRET PAGE TEXT" {
		t.Fatal("the session's own messages are byte-identical afterwards")
	}
}

func TestSessionQueryIsBusyWhileATurnRuns(t *testing.T) {
	ag, _ := apiAgent(t, "x")
	ag.turnMu.Lock()
	defer ag.turnMu.Unlock()
	b := NewAPIBackend(ag, "1.3.1")
	_, err := b.SessionQuery(context.Background(), api.QueryRequest{Prompt: "q"})
	var e *api.Error
	if !errors.As(err, &e) || e.Status != 409 || e.Code != "session_busy" {
		t.Fatalf("want 409 session_busy, got %v", err)
	}
}

func TestSessionQueryTooLargeIs413(t *testing.T) {
	ag, _ := apiAgent(t, "x")
	ag.ApplyWindow(600)
	ag.History.Messages = []provider.Message{{Role: provider.RoleUser, Content: strings.Repeat("word ", 1000)}}
	b := NewAPIBackend(ag, "1.3.1")
	_, err := b.SessionQuery(context.Background(), api.QueryRequest{Prompt: "q", MaxTokens: 100})
	var e *api.Error
	if !errors.As(err, &e) || e.Status != 413 || e.Code != "context_too_large" {
		t.Fatalf("want 413, got %v", err)
	}
}

func TestSessionTokenMintsSavesAndAccepts(t *testing.T) {
	ag, _ := apiAgent(t)
	t.Setenv("HOME", t.TempDir())
	b := NewAPIBackend(ag, "1.3.1")
	tok, err := ag.SessionToken()
	if err != nil || !strings.HasPrefix(tok, "sess_1_") {
		t.Fatalf("%v %q", err, tok)
	}
	if !b.AcceptsSession(tok) || b.AcceptsSession("sess_1_nope") {
		t.Fatal("the backend accepts exactly the session's token")
	}
	saved, err := store.Load(ag.Session.ID)
	if err != nil || saved.Token != tok {
		t.Fatalf("the token is persisted: %v %+v", err, saved)
	}
	tok2, err := ag.RotateSessionToken(0)
	if err != nil || tok2 == tok || b.AcceptsSession(tok) || !b.AcceptsSession(tok2) {
		t.Fatalf("rotate with zero grace cuts the old token: %v", err)
	}
	if h := b.Health(); h.Version != "1.3.1" || h.SessionCode != ag.Session.ResumeCode() || h.Model != "test-model" {
		t.Fatalf("health: %+v", h)
	}
}

func TestNamedModelGoesThroughTheFactory(t *testing.T) {
	ag, p := apiAgent(t, "primary")
	other := &scriptedProvider{responses: []provider.ChatResponse{{Content: "other"}}}
	calls := 0
	APIModelFactory = func(ctx context.Context, cfg *config.Config, model string) (provider.Provider, error) {
		calls++
		return other, nil
	}
	t.Cleanup(func() { APIModelFactory = nil })
	b := NewAPIBackend(ag, "1.3.1")
	resp, err := b.Query(context.Background(), api.QueryRequest{Prompt: "q", Model: "big"})
	if err != nil || resp.Text != "other" || resp.Model != "big" || p.i != 0 {
		t.Fatalf("%v %+v (primary calls %d)", err, resp, p.i)
	}
	if _, err := b.Query(context.Background(), api.QueryRequest{Prompt: "q", Model: "big"}); err != nil || calls != 1 {
		t.Fatalf("the provider is built once per model: calls=%d err=%v", calls, err)
	}
	APIModelFactory = nil
	if _, err := b.Query(context.Background(), api.QueryRequest{Prompt: "q", Model: "big"}); err == nil {
		t.Fatal("with no factory a named model is an error")
	}
}

func TestAPISpendCapIs429WithoutAsking(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ag, _ := onlineAgent(t, &scriptedProvider{}, "openrouter", func(c *config.Config) { c.MaxSpendUSD = 1; c.RepoMap = false })
	ag.approveOnline("openrouter")
	ag.addStats(Stats{SpendUSD: 2})
	var l safeLog
	ag.Tools.Approve = l.approver(true)
	_, err := NewAPIBackend(ag, "1").Query(context.Background(), api.QueryRequest{Prompt: "q"})
	var e *api.Error
	if !errors.As(err, &e) || e.Status != 429 || e.Code != "spend_cap" {
		t.Fatalf("want 429 spend_cap, got %v", err)
	}
	if got := l.get(); len(got) != 0 {
		t.Fatalf("the person must not be asked: %q", got)
	}
}

func TestAPIUnapprovedOnlineProviderIs403(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := &scriptedProvider{}
	ag, _ := onlineAgent(t, p, "openrouter", func(c *config.Config) { c.RepoMap = false })
	var l safeLog
	ag.Tools.Approve = l.approver(true)
	_, err := NewAPIBackend(ag, "1").Query(context.Background(), api.QueryRequest{Prompt: "q"})
	var e *api.Error
	if !errors.As(err, &e) || e.Status != 403 || e.Code != "online_not_approved" || p.i != 0 || len(l.get()) != 0 {
		t.Fatalf("want 403 online_not_approved, nothing sent, nobody asked: %v", err)
	}
	calls := 0
	APIModelFactory = func(ctx context.Context, cfg *config.Config, model string) (provider.Provider, error) {
		calls++
		return &scriptedProvider{}, nil
	}
	t.Cleanup(func() { APIModelFactory = nil })
	_, err = NewAPIBackend(ag, "1").Query(context.Background(), api.QueryRequest{Prompt: "q", Model: "big"})
	if !errors.As(err, &e) || e.Status != 403 || e.Code != "online_not_approved" || calls != 0 {
		t.Fatalf("a named model is gated too, factory uncalled (calls=%d): %v", calls, err)
	}
}

func TestSessionTokenRefusedWhileATurnRuns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ag, _ := apiAgent(t)
	ag.turnMu.Lock()
	_, err := ag.SessionToken()
	_, err2 := ag.RotateSessionToken(0)
	ag.turnMu.Unlock()
	if err == nil || err2 == nil || !strings.Contains(err.Error(), "middle of a turn") || ag.Session.Token != "" {
		t.Fatalf("%v %v token=%q", err, err2, ag.Session.Token)
	}
}

func TestSessionTokenSaveStampsHostPID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ag, _ := apiAgent(t)
	if _, err := ag.SessionToken(); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Load(ag.Session.ID)
	if err != nil || saved.HostPID != os.Getpid() {
		t.Fatalf("%v host_pid=%d", err, saved.HostPID)
	}
}
