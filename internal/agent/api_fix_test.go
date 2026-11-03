package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/api"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// apiNamedProvider answers "ok" under a name of its own, recording requests.
type apiNamedProvider struct {
	name string
	mu   sync.Mutex
	reqs []provider.ChatRequest
}

func (n *apiNamedProvider) Name() string { return n.name }
func (n *apiNamedProvider) Chat(_ context.Context, req provider.ChatRequest, _ provider.StreamFunc) (*provider.ChatResponse, error) {
	n.mu.Lock()
	n.reqs = append(n.reqs, req)
	n.mu.Unlock()
	return &provider.ChatResponse{Content: "ok"}, nil
}
func (n *apiNamedProvider) ListModels(context.Context) ([]provider.ModelInfo, error) { return nil, nil }
func (n *apiNamedProvider) Ping(context.Context) (string, error)                     { return "ok", nil }
func (n *apiNamedProvider) calls() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.reqs)
}

// Finding 1: a named model is built on the session's current provider,
// never on cfg.DefaultProvider, and the cache follows a provider switch.
func TestNamedModelIsBuiltOnTheCurrentProvider(t *testing.T) {
	primary := &apiNamedProvider{name: "ollama"}
	ag, _ := newTestAgent(t, primary, func(c *config.Config) { c.RepoMap = false; c.DefaultProvider = "openrouter" })
	var got []string
	APIModelFactory = func(ctx context.Context, cfg *config.Config, providerName, model string) (provider.Provider, error) {
		got = append(got, providerName+"/"+model)
		return &apiNamedProvider{name: providerName}, nil
	}
	t.Cleanup(func() { APIModelFactory = nil })
	b := NewAPIBackend(ag, "1")
	if _, err := b.Query(context.Background(), api.QueryRequest{Prompt: "q", Model: "big"}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "ollama/big" {
		t.Fatalf("built on %v, want the current provider ollama", got)
	}
	// Same provider: cached.
	_, _ = b.Query(context.Background(), api.QueryRequest{Prompt: "q", Model: "big"})
	if len(got) != 1 {
		t.Fatalf("rebuilt for the same provider: %v", got)
	}
	// A /provider switch: the ("ollama","big") provider is not reused.
	ag.modelMu.Lock()
	ag.Provider = &apiNamedProvider{name: "p2"}
	ag.modelMu.Unlock()
	if _, err := b.Query(context.Background(), api.QueryRequest{Prompt: "q", Model: "big"}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1] != "p2/big" {
		t.Fatalf("after a switch built on %v, want p2/big", got)
	}
}

// Finding 1 (and 4): under an online primary a named model is refused
// before anything is built — its spend could be neither counted nor capped.
func TestNamedModelRefusedWhileOnline(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	p := &scriptedProvider{}
	ag, _ := onlineAgent(t, p, "openrouter", func(c *config.Config) { c.RepoMap = false })
	ag.approveOnline("openrouter")
	calls := 0
	APIModelFactory = func(ctx context.Context, cfg *config.Config, providerName, model string) (provider.Provider, error) {
		calls++
		return &scriptedProvider{}, nil
	}
	t.Cleanup(func() { APIModelFactory = nil })
	_, err := NewAPIBackend(ag, "1").Query(context.Background(), api.QueryRequest{Prompt: "q", Model: "big"})
	var e *api.Error
	if !errors.As(err, &e) || e.Status != 403 || e.Code != "model_not_allowed" || calls != 0 || p.i != 0 {
		t.Fatalf("want 403 model_not_allowed, factory uncalled (calls=%d): %v", calls, err)
	}
	// The default model is still served.
	if _, err := NewAPIBackend(ag, "1").Query(context.Background(), api.QueryRequest{Prompt: "q"}); err != nil {
		t.Fatalf("the primary is still served online: %v", err)
	}
}

// gatedProvider holds every Chat until released, counting how many are
// inside at once.
type gatedProvider struct {
	active, peak atomic.Int32
	entered      chan string
	release      chan struct{}
}

func (g *gatedProvider) Name() string { return "gated" }
func (g *gatedProvider) Chat(ctx context.Context, req provider.ChatRequest, _ provider.StreamFunc) (*provider.ChatResponse, error) {
	n := g.active.Add(1)
	defer g.active.Add(-1)
	for {
		p := g.peak.Load()
		if n <= p || g.peak.CompareAndSwap(p, n) {
			break
		}
	}
	who := "agent"
	if len(req.Messages) > 0 && req.Messages[len(req.Messages)-1].Content == "from-api" {
		who = "api"
	}
	g.entered <- who
	select {
	case <-g.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &provider.ChatResponse{Content: "ok"}, nil
}
func (g *gatedProvider) ListModels(context.Context) ([]provider.ModelInfo, error) { return nil, nil }
func (g *gatedProvider) Ping(context.Context) (string, error)                     { return "ok", nil }

// Finding 2: with the primary lane ensured (no sub-agent configured), an
// API call and the person's own request never reach the server together.
func TestAPICallSharesThePrimaryLane(t *testing.T) {
	g := &gatedProvider{entered: make(chan string, 4), release: make(chan struct{}, 4)}
	ag, _ := newTestAgent(t, g, func(c *config.Config) { c.RepoMap = false })
	if ag.SubAgentsEnabled() {
		t.Fatal("setup: no sub-agent runner")
	}
	ag.EnsurePrimaryLane()
	if ag.SubAgentsEnabled() {
		t.Fatal("ensuring a lane must not create the sub-agent runner")
	}
	b := NewAPIBackend(ag, "1")
	apiDone := make(chan error, 1)
	go func() {
		_, err := b.Query(context.Background(), api.QueryRequest{Prompt: "from-api"})
		apiDone <- err
	}()
	if who := <-g.entered; who != "api" {
		t.Fatalf("first in: %s", who)
	}
	agentDone := make(chan error, 1)
	go func() {
		_, err := ag.chatInLane(context.Background(), provider.ChatRequest{Messages: []provider.Message{{Role: provider.RoleUser, Content: "from-person"}}})
		agentDone <- err
	}()
	select {
	case who := <-g.entered:
		t.Fatalf("%s reached the server while the API call held it", who)
	case <-time.After(150 * time.Millisecond):
	}
	g.release <- struct{}{}
	if err := <-apiDone; err != nil {
		t.Fatal(err)
	}
	if who := <-g.entered; who != "agent" {
		t.Fatalf("second in: %s", who)
	}
	g.release <- struct{}{}
	if err := <-agentDone; err != nil {
		t.Fatal(err)
	}
	if p := g.peak.Load(); p != 1 {
		t.Fatalf("peak concurrency %d, want 1", p)
	}
}

// Finding 3: while a window resolution runs, the API's primary call waits.
func TestAPICallWaitsForTheWindow(t *testing.T) {
	p := &apiNamedProvider{name: "ollama"}
	ag, _ := newTestAgent(t, p, func(c *config.Config) { c.RepoMap = false })
	ag.modelMu.Lock()
	ag.beginResolveLocked(4242)
	ag.modelMu.Unlock()
	done := make(chan error, 1)
	go func() {
		_, err := NewAPIBackend(ag, "1").Query(context.Background(), api.QueryRequest{Prompt: "q"})
		done <- err
	}()
	time.Sleep(150 * time.Millisecond)
	if n := p.calls(); n != 0 {
		t.Fatalf("the request went out during the resolution (%d calls)", n)
	}
	ag.endResolve(4242)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := p.calls(); n != 1 {
		t.Fatalf("calls after release: %d", n)
	}
}

// Finding 6: a query's schema reaches the provider as Format; a completion's
// response_format passes unchanged, and JSON null is none.
func TestStructuredOutputReachesTheProvider(t *testing.T) {
	p := &apiNamedProvider{name: "ollama"}
	ag, _ := newTestAgent(t, p, func(c *config.Config) { c.RepoMap = false })
	b := NewAPIBackend(ag, "1")
	schema := json.RawMessage(`{"type":"object","properties":{"n":{"type":"integer"}}}`)
	if _, err := b.Query(context.Background(), api.QueryRequest{Prompt: "q", JSONSchema: schema}); err != nil {
		t.Fatal(err)
	}
	rf := json.RawMessage(`{"type":"json_object"}`)
	if _, err := b.Complete(context.Background(), api.CompletionRequest{Messages: []api.ChatMessage{{Role: "user", Content: "x"}}, ResponseFormat: rf}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Complete(context.Background(), api.CompletionRequest{Messages: []api.ChatMessage{{Role: "user", Content: "x"}}, ResponseFormat: json.RawMessage(" null ")}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Query(context.Background(), api.QueryRequest{Prompt: "q"}); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if string(p.reqs[0].Format) != string(schema) || p.reqs[0].ResponseFormat != nil {
		t.Fatalf("query: Format %s ResponseFormat %s", p.reqs[0].Format, p.reqs[0].ResponseFormat)
	}
	if string(p.reqs[1].ResponseFormat) != string(rf) || p.reqs[1].Format != nil {
		t.Fatalf("completion: ResponseFormat %s Format %s", p.reqs[1].ResponseFormat, p.reqs[1].Format)
	}
	if p.reqs[2].ResponseFormat != nil || p.reqs[3].Format != nil {
		t.Fatalf("null / absent must be none: %s %s", p.reqs[2].ResponseFormat, p.reqs[3].Format)
	}
}

// Finding 7: a panic while the snapshot is taken leaves both locks free.
func TestSessionQuerySnapshotPanicFreesTheLocks(t *testing.T) {
	ag, _ := apiAgent(t, "x")
	ag.History = nil // the snapshot dereferences it: a panic inside the locks
	func() {
		defer func() { _ = recover() }()
		_, _ = NewAPIBackend(ag, "1").SessionQuery(context.Background(), api.QueryRequest{Prompt: "q"})
	}()
	if !ag.turnMu.TryLock() {
		t.Fatal("turnMu is still held after a panicking snapshot")
	}
	ag.turnMu.Unlock()
	if !ag.modelMu.TryLock() {
		t.Fatal("modelMu is still held after a panicking snapshot")
	}
	ag.modelMu.Unlock()
}
