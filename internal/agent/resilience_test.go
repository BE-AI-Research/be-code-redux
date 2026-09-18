package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/store"
)

// statusProvider adds the optional backend-status and keep-alive
// capabilities (as Ollama has) on top of funcProvider.
type statusProvider struct {
	*funcProvider
	mu     sync.Mutex
	window int
	loaded bool
	kept   []time.Duration
}

func (s *statusProvider) Status(context.Context, string) (int, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.window, s.loaded, nil
}
func (s *statusProvider) KeepAlive(_ context.Context, _ string, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.kept = append(s.kept, d)
	return nil
}
func (s *statusProvider) keepAlives() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.kept)
}

func collectNotices(ag *Agent) *[]string {
	var out []string
	ag.Events.OnNotice = func(m string) { out = append(out, m) }
	return &out
}

func hasNotice(ns []string, sub string) bool {
	for _, n := range ns {
		if strings.Contains(strings.ToLower(n), sub) {
			return true
		}
	}
	return false
}

// A backend that drops the connection mid-task (Ollama swapping models,
// restarting) must be retried in place with the tool loop state intact.
func TestRunRetriesTransientBackendErrors(t *testing.T) {
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls <= 2 {
			return nil, errors.New("ollama: Post \"http://x/v1/chat/completions\": connection refused")
		}
		return &provider.ChatResponse{Content: "recovered"}, nil
	}}
	ag, _ := newTestAgent(t, p, nil)
	ag.retryBase = time.Millisecond
	notes := collectNotices(ag)
	answer, err := ag.Run(context.Background(), "do it")
	if err != nil || answer != "recovered" {
		t.Fatalf("answer=%q err=%v", answer, err)
	}
	if calls != 3 {
		t.Fatalf("calls = %d, want 3", calls)
	}
	if !hasNotice(*notes, "retry") {
		t.Fatalf("no retry notice: %v", *notes)
	}
}

func TestRunDoesNotRetryClientErrors(t *testing.T) {
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		return nil, errors.New("ollama: HTTP 400: invalid request")
	}}
	ag, _ := newTestAgent(t, p, nil)
	ag.retryBase = time.Millisecond
	if _, err := ag.Run(context.Background(), "do it"); err == nil {
		t.Fatal("expected error")
	}
	if calls != 1 {
		t.Fatalf("client error retried %d times", calls-1)
	}
}

func TestRunGivesUpAfterMaxRetries(t *testing.T) {
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		return nil, errors.New("ollama: HTTP 503: server busy")
	}}
	ag, _ := newTestAgent(t, p, nil)
	ag.retryBase = time.Millisecond
	_, err := ag.Run(context.Background(), "do it")
	if err == nil || !strings.Contains(err.Error(), "backend") {
		t.Fatalf("err = %v", err)
	}
	if calls != maxBackendRetries+1 {
		t.Fatalf("calls = %d, want %d", calls, maxBackendRetries+1)
	}
}

// Progress made before a backend failure must reach the session file.
func TestRunAutosavesOnBackendError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, errors.New("ollama: HTTP 400: nope")
	}}
	ag, _ := newTestAgent(t, p, nil)
	ag.Session = store.NewSession("func", "m", ag.Tools.Root)
	ag.Run(context.Background(), "remember this task")
	got, err := store.Load(ag.Session.ID)
	if err != nil || len(got.Messages) == 0 || !strings.Contains(got.Messages[0].Content, "remember this task") {
		t.Fatalf("session not saved on error: %v %+v", err, got)
	}
}

// If another client made Ollama reload our model with a smaller window, the
// budget must follow before the next prompt goes out; when the window comes
// back, the budget returns to the configured value.
func TestBackendWindowChangeReappliesBudget(t *testing.T) {
	sp := &statusProvider{funcProvider: &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok"}, nil
	}}, window: 32768, loaded: true}
	ag, _ := newTestAgent(t, sp, func(c *config.Config) { c.ContextTokens = 32768 })
	ag.ApplyWindow(32768)
	notes := collectNotices(ag)

	sp.window = 8192
	ag.Run(context.Background(), "one")
	if ag.History.Budget != 8192 {
		t.Fatalf("budget not clamped to shrunken window: %d", ag.History.Budget)
	}
	if !hasNotice(*notes, "window") {
		t.Fatalf("no window-change notice: %v", *notes)
	}
	sp.window = 32768
	ag.Run(context.Background(), "two")
	if ag.History.Budget != 32768 {
		t.Fatalf("budget not restored when window returned: %d", ag.History.Budget)
	}
}

func TestNoticeWhenModelWasUnloaded(t *testing.T) {
	sp := &statusProvider{funcProvider: &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok"}, nil
	}}, window: 32768, loaded: false}
	ag, _ := newTestAgent(t, sp, nil)
	ag.ApplyWindow(32768)
	notes := collectNotices(ag)
	ag.Run(context.Background(), "one")
	if !hasNotice(*notes, "not loaded") {
		t.Fatalf("no unloaded notice: %v", *notes)
	}
}

// A silent backend (model reloading, queued behind another client) must be
// reported rather than looking like a hang.
func TestStallNoticeWhenBackendSilent(t *testing.T) {
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		time.Sleep(80 * time.Millisecond)
		return &provider.ChatResponse{Content: "late"}, nil
	}}
	ag, _ := newTestAgent(t, p, nil)
	ag.stallAfter = 10 * time.Millisecond
	notes := collectNotices(ag)
	ag.Run(context.Background(), "one")
	if !hasNotice(*notes, "waiting") {
		t.Fatalf("no stall notice: %v", *notes)
	}
}

// After each request the model's keep-alive is refreshed so idle expiry
// does not evict it between the user's prompts.
func TestKeepAliveRefreshedAfterRun(t *testing.T) {
	sp := &statusProvider{funcProvider: &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok"}, nil
	}}, window: 32768, loaded: true}
	ag, _ := newTestAgent(t, sp, nil)
	ag.Run(context.Background(), "one")
	deadline := time.Now().Add(2 * time.Second)
	for sp.keepAlives() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if sp.keepAlives() == 0 {
		t.Fatal("keep-alive never refreshed")
	}
}

// A reasoning model spends part of the window thinking before it answers,
// so its generation reserve must be far larger than a plain model's.
func TestReserveSizedForThinkingModels(t *testing.T) {
	thinker, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 32768 })
	thinker.SetModel("qwen3:8b") // StripThink profile
	thinker.ApplyWindow(32768)
	plain, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 32768 })
	plain.SetModel("llama3:8b")
	plain.ApplyWindow(32768)
	if thinker.History.Reserve < 8192 {
		t.Fatalf("thinking reserve too small: %d", thinker.History.Reserve)
	}
	if plain.History.Reserve != 4096 {
		t.Fatalf("plain reserve changed: %d", plain.History.Reserve)
	}
	// Switching models re-derives the reserve for the same window.
	plain.SetModel("qwen3:8b")
	if plain.History.Reserve < 8192 {
		t.Fatalf("reserve not re-derived on SetModel: %d", plain.History.Reserve)
	}
}

// When the model runs out of window while still reasoning (empty content,
// finish_reason=length, reasoning present), free context and retry once
// instead of failing the task.
func TestRunRecoversFromLengthCutoffDuringReasoning(t *testing.T) {
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		if strings.HasPrefix(req.Messages[0].Content, "Summarize") {
			return &provider.ChatResponse{Content: "SUMMARY of earlier work"}, nil
		}
		calls++
		if calls == 1 {
			return &provider.ChatResponse{Content: "", Reasoning: strings.Repeat("hmm ", 2000), FinishReason: "length"}, nil
		}
		return &provider.ChatResponse{Content: "final answer"}, nil
	}}
	ag, _ := newTestAgent(t, p, func(c *config.Config) { c.ContextTokens = 4000 })
	notes := collectNotices(ag)
	big := strings.Repeat("tool output line\n", 300)
	for i := 0; i < 4; i++ {
		ag.History.Add(provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c", Name: "read_file", Arguments: "{}"}}})
		ag.History.Add(provider.Message{Role: provider.RoleTool, ToolCallID: "c", Content: big})
	}
	answer, err := ag.Run(context.Background(), "finish the task")
	if err != nil || answer != "final answer" {
		t.Fatalf("answer=%q err=%v notes=%v", answer, err, *notes)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
	if !hasNotice(*notes, "reason") {
		t.Fatalf("no explanatory notice: %v", *notes)
	}
}

// If the low-effort retry is cut off too, the failure is reported with the
// real cause (reasoning exhausted the window) and says what was tried.
func TestRunLengthCutoffErrorNamesReasoning(t *testing.T) {
	p := &scriptedProvider{responses: []provider.ChatResponse{
		{Content: "", Reasoning: strings.Repeat("x", 5000), FinishReason: "length"},
		{Content: "", Reasoning: strings.Repeat("x", 4000), FinishReason: "length"},
	}}
	ag, _ := newTestAgent(t, p, nil)
	_, err := ag.Run(context.Background(), "do the thing")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "reasoning") || !strings.Contains(err.Error(), "reasoning_effort=low") {
		t.Fatalf("err = %v", err)
	}
}

// fallbackProvider is a backend that has given up its native endpoint.
type fallbackProvider struct {
	funcProvider
	fellBack bool
}

func (f *fallbackProvider) NativeFallback() bool { return f.fellBack }

// A session running on the fallback endpoint cannot set the model's context
// window, so it must not look identical to a healthy one — but it is also
// not an error, so it is said once and only once.
func TestNativeFallbackIsNoticedOnce(t *testing.T) {
	p := &fallbackProvider{}
	p.fn = func(provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok"}, nil
	}
	ag, _ := newTestAgent(t, p, nil)
	notices := collectNotices(ag)
	if _, err := ag.Run(context.Background(), "first"); err != nil {
		t.Fatal(err)
	}
	if hasNotice(*notices, "openai-compatible path") {
		t.Fatalf("nothing was downgraded: %v", *notices)
	}
	p.fellBack = true
	for i := 0; i < 2; i++ {
		if _, err := ag.Run(context.Background(), "again"); err != nil {
			t.Fatal(err)
		}
	}
	n := 0
	for _, s := range *notices {
		if strings.Contains(strings.ToLower(s), "openai-compatible path") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly one downgrade notice, got %d: %v", n, *notices)
	}
}

// TestSetProviderClearsTheNoticeLatches: the downgrade notice and the
// eviction notice describe *a backend*, and must not outlive the backend
// they described. Switching providers with a bare assignment left both
// latched, so the same thing happening on the newly chosen backend was
// silent — a session quietly unable to set its context window with nothing
// on screen to say so.
func TestSetProviderClearsTheNoticeLatches(t *testing.T) {
	reply := func(provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok"}, nil
	}
	first := &fallbackProvider{fellBack: true}
	first.fn = reply
	ag, _ := newTestAgent(t, first, nil)
	notes := collectNotices(ag)

	ag.Run(context.Background(), "one")
	if !hasNotice(*notes, "openai-compatible path") {
		t.Fatalf("no downgrade notice for the first backend: %v", *notes)
	}
	before := len(*notes)
	ag.Run(context.Background(), "two")
	if len(*notes) != before {
		t.Fatalf("the same backend reported its downgrade twice: %v", *notes)
	}

	// A different backend, downgraded for its own reasons. That is news.
	second := &fallbackProvider{fellBack: true}
	second.fn = reply
	ag.SetProvider(second)
	ag.Run(context.Background(), "three")
	if len(*notes) == before {
		t.Fatalf("a downgrade on a newly selected backend was silent: %v", *notes)
	}
}

// TestSetProviderClearsTheEvictionLatch: the same rule for "model is not
// loaded", which is equally a fact about one backend.
func TestSetProviderClearsTheEvictionLatch(t *testing.T) {
	reply := func(provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok"}, nil
	}
	first := &statusProvider{funcProvider: &funcProvider{fn: reply}, window: 32768, loaded: false}
	ag, _ := newTestAgent(t, first, nil)
	ag.ApplyWindow(32768)
	notes := collectNotices(ag)

	ag.Run(context.Background(), "one")
	if !hasNotice(*notes, "not loaded") {
		t.Fatalf("no eviction notice: %v", *notes)
	}
	before := len(*notes)

	second := &statusProvider{funcProvider: &funcProvider{fn: reply}, window: 32768, loaded: false}
	ag.SetProvider(second)
	ag.Run(context.Background(), "two")
	if len(*notes) == before {
		t.Fatalf("an eviction on a newly selected backend was silent: %v", *notes)
	}
}

// TestSetModelReserveSurvivesAnUnsetContextTokens: context_tokens is now
// routinely unset (it means "derive from the window"), so a SetModel before
// any window is known must reserve against the budget rather than against a
// literal 0 — which would drop a thinking model from 4096 tokens of
// generation headroom to the 1024 floor and truncate its reasoning.
func TestSetModelReserveSurvivesAnUnsetContextTokens(t *testing.T) {
	p := &funcProvider{fn: func(provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok"}, nil
	}}
	ag, _ := newTestAgent(t, p, func(c *config.Config) { c.ContextTokens = 0 })
	if ag.Window() != 0 {
		t.Fatalf("this test is about the no-window-yet case; window %d", ag.Window())
	}
	budget := ag.History.Budget
	if budget <= 0 {
		t.Fatalf("an unset context_tokens must still leave a usable budget; got %d", budget)
	}
	ag.SetModel("qwen3:8b") // a thinking model: a third of the budget, floored at 4096
	if got, want := ag.History.Reserve, ag.reserveFor(budget); got != want {
		t.Fatalf("reserve %d; with no window known it follows the budget (%d)", got, want)
	}
	if ag.History.Reserve <= 1024 {
		t.Fatalf("reserve collapsed to the floor: %d", ag.History.Reserve)
	}
}
