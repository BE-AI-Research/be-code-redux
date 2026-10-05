package agent

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/provider"
)

// recordSleeps replaces the agent's sleep with one that records the
// requested duration and returns at once, so no test waits for real.
func recordSleeps(ag *Agent) *[]time.Duration {
	var mu sync.Mutex
	var got []time.Duration
	ag.sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		got = append(got, d)
		mu.Unlock()
		return ctx.Err()
	}
	return &got
}

func TestChatWithRetryHonoursRetryAfter(t *testing.T) {
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls == 1 {
			return nil, &provider.HTTPError{Provider: "openrouter", Code: 429, Body: "rate limited", RetryAfter: 3 * time.Second}
		}
		return &provider.ChatResponse{Content: "ok"}, nil
	}}
	ag, _ := newTestAgent(t, p, nil)
	ag.retryBase = time.Millisecond
	sleeps := recordSleeps(ag)
	var transients []string
	ag.Events.OnTransient = func(m string) { transients = append(transients, m) }
	answer, err := ag.Run(context.Background(), "do it")
	if err != nil || answer != "ok" {
		t.Fatalf("answer=%q err=%v", answer, err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if len(*sleeps) != 1 || (*sleeps)[0] != 3*time.Second {
		t.Fatalf("sleeps = %v, want [3s]", *sleeps)
	}
	found := false
	for _, m := range transients {
		if m == "rate-limited by openrouter; retrying in 3s" {
			found = true
		}
	}
	if !found {
		t.Fatalf("transients = %q", transients)
	}
}

// A Retry-After beyond the existing 30 s cap is clamped to it, and a 429
// without one falls back to the ordinary backoff.
func TestChatWithRetryClampsRetryAfter(t *testing.T) {
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		switch calls {
		case 1:
			return nil, &provider.HTTPError{Provider: "openrouter", Code: 429, RetryAfter: time.Hour}
		case 2:
			return nil, &provider.HTTPError{Provider: "openrouter", Code: 429}
		}
		return &provider.ChatResponse{Content: "ok"}, nil
	}}
	ag, _ := newTestAgent(t, p, nil)
	ag.retryBase = time.Millisecond
	sleeps := recordSleeps(ag)
	if _, err := ag.Run(context.Background(), "do it"); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{30 * time.Second, 2 * time.Millisecond}
	if len(*sleeps) != 2 || (*sleeps)[0] != want[0] || (*sleeps)[1] != want[1] {
		t.Fatalf("sleeps = %v, want %v", *sleeps, want)
	}
}

// A 429 that never clears is still bounded by maxBackendRetries.
func TestChatWithRetryRateLimitBounded(t *testing.T) {
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		return nil, &provider.HTTPError{Provider: "openrouter", Code: 429, RetryAfter: 5 * time.Second}
	}}
	ag, _ := newTestAgent(t, p, nil)
	ag.retryBase = time.Millisecond
	recordSleeps(ag)
	if _, err := ag.Run(context.Background(), "do it"); err == nil {
		t.Fatal("expected error")
	}
	if calls != maxBackendRetries+1 {
		t.Fatalf("calls = %d, want %d", calls, maxBackendRetries+1)
	}
}

func TestUnauthorizedNotRetried(t *testing.T) {
	for _, code := range []int{401, 403} {
		calls := 0
		p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
			calls++
			return nil, &provider.HTTPError{Provider: "openrouter", Code: code, Body: "bad key"}
		}}
		ag, _ := newTestAgent(t, p, nil)
		ag.retryBase = time.Millisecond
		sleeps := recordSleeps(ag)
		ag.KeyEnv = "OPENROUTER_API_KEY"
		_, err := ag.Run(context.Background(), "do it")
		if err == nil || !strings.Contains(err.Error(), "OPENROUTER_API_KEY rejected by openrouter") {
			t.Fatalf("%d: err = %v", code, err)
		}
		if calls != 1 || len(*sleeps) != 0 {
			t.Fatalf("%d: calls = %d sleeps = %v, want one call, no wait", code, calls, *sleeps)
		}
	}
}

func TestUnauthorizedWithoutKeyEnv(t *testing.T) {
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, &provider.HTTPError{Provider: "openrouter", Code: 401}
	}}
	ag, _ := newTestAgent(t, p, nil)
	_, err := ag.Run(context.Background(), "do it")
	if err == nil || !strings.Contains(err.Error(), "API key rejected by openrouter") {
		t.Fatalf("err = %v", err)
	}
}
