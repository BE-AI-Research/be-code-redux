package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// prefillProvider is funcProvider plus the Prefiller capability. A prefill
// blocks until released or cancelled, like a server reading a long prompt.
type prefillProvider struct {
	*funcProvider
	mu        sync.Mutex
	prefills  []provider.ChatRequest
	cancelled int
	release   chan struct{}
}

func (p *prefillProvider) Prefill(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	p.mu.Lock()
	p.prefills = append(p.prefills, req)
	p.mu.Unlock()
	select {
	case <-p.release:
		return &provider.ChatResponse{Usage: provider.Usage{PromptTokens: 9747, PromptDuration: 21 * time.Second}}, nil
	case <-ctx.Done():
		p.mu.Lock()
		p.cancelled++
		p.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (p *prefillProvider) seen() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.prefills), p.cancelled
}

// After a resolution lands, the prompt the next turn will send is read by the
// server ahead of time: same system prompt, same messages, same tools.
func TestAResolvedModelIsPrefilledWithTheNextPrompt(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 0 })
	pp := &prefillProvider{funcProvider: &funcProvider{}, release: make(chan struct{})}
	close(pp.release)
	ag.Provider = pp
	ag.SetLoader(&fakeLoader{window: 32768})
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "earlier"})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "answer"})

	ag.ResolveModelNow(context.Background())
	waitFor(t, "the prefill", func() bool { n, _ := pp.seen(); return n == 1 })
	ag.stopPrefill()

	pp.mu.Lock()
	req := pp.prefills[0]
	pp.mu.Unlock()
	if len(req.Messages) != 3 || req.Messages[0].Role != provider.RoleSystem || req.Messages[2].Content != "answer" {
		t.Fatalf("the prefill is not the next request's prefix: %+v", req.Messages)
	}
	if len(req.Tools) != len(ag.Tools.Specs()) {
		t.Fatalf("tools are part of the rendered prompt: %d sent, %d registered", len(req.Tools), len(ag.Tools.Specs()))
	}
}

// A real request never waits behind a prefill and never runs beside one: it
// cancels it first. The server keeps whatever it had read (probed).
func TestARealRequestCancelsThePrefill(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 0 })
	pp := &prefillProvider{funcProvider: &funcProvider{fn: func(provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "done"}, nil
	}}, release: make(chan struct{})}
	ag.Provider = pp
	ag.SetLoader(&fakeLoader{window: 32768})
	ag.ResolveModelNow(context.Background())
	waitFor(t, "the prefill to start", func() bool { n, _ := pp.seen(); return n == 1 })

	done := make(chan error, 1)
	go func() { _, err := ag.Run(context.Background(), "hello"); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the request waited behind the prefill")
	}
	if _, cancelled := pp.seen(); cancelled != 1 {
		t.Fatalf("the prefill was not cancelled (cancelled=%d)", cancelled)
	}
}

func TestPrefillCanBeTurnedOffAndNeedsTheCapability(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 0; c.PromptPrefill = false })
	pp := &prefillProvider{funcProvider: &funcProvider{}, release: make(chan struct{})}
	ag.Provider = pp
	ag.SetLoader(&fakeLoader{window: 32768})
	ag.ResolveModelNow(context.Background())
	time.Sleep(50 * time.Millisecond)
	if n, _ := pp.seen(); n != 0 {
		t.Fatalf("prefilled with prompt_prefill off: %d", n)
	}
	// A provider without the capability is simply left alone.
	plain, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 0 })
	plain.SetLoader(&fakeLoader{window: 32768})
	plain.ResolveModelNow(context.Background())
	plain.stopPrefill()
}
