package cmd

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// ollamaProbeStub counts every management endpoint startup might reach for.
// /api/generate is the one that matters most: calling it is what "loading
// the model to read its window" was, and it is the multi-minute stall this
// wiring exists to delete.
type ollamaProbeStub struct {
	srv                *httptest.Server
	ps, show, generate int32
	psBody             string
}

func newOllamaProbeStub(t *testing.T, psBody string) *ollamaProbeStub {
	t.Helper()
	s := &ollamaProbeStub{psBody: psBody}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			atomic.AddInt32(&s.ps, 1)
			w.Write([]byte(s.psBody))
		case "/api/show":
			atomic.AddInt32(&s.show, 1)
			w.Write([]byte(`{"parameters":"num_ctx 4096"}`))
		case "/api/generate":
			atomic.AddInt32(&s.generate, 1)
			w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func testAgentFor(t *testing.T, cfg *config.Config, p provider.Provider, model string) (*agent.Agent, *tools.Registry) {
	t.Helper()
	reg, err := tools.NewRegistry(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return agent.New(cfg, p, model, reg, ""), reg
}

// TestStartupAppliesAConfiguredWindowWithoutProbing: the whole point of
// context_window is that the harness stops asking. No /api/show, and above
// all no /api/generate — loading a 27B model to read a number the user
// already wrote down is minutes of nothing.
func TestStartupAppliesAConfiguredWindowWithoutProbing(t *testing.T) {
	stub := newOllamaProbeStub(t, `{"models":[]}`)
	cfg := config.Default()
	cfg.ContextTokens = 0 // unset: the window is the budget
	cfg.Providers["lan"] = config.ProviderConfig{Type: "ollama", BaseURL: stub.srv.URL}
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("lan", stub.srv.URL, "")
	ag, reg := testAgentFor(t, cfg, p, "m")

	applyModelParams(cfg, p, reg, ag, "m")

	if ag.Window() != 32768 {
		t.Fatalf("window %d", ag.Window())
	}
	if p.Options().NumCtx != 32768 {
		t.Fatalf("num_ctx %d never reached the provider", p.Options().NumCtx)
	}
	if atomic.LoadInt32(&stub.show) != 0 {
		t.Fatalf("probed the Modelfile %d times with an explicit window", stub.show)
	}
	if atomic.LoadInt32(&stub.generate) != 0 {
		t.Fatal("startup loaded the model to read its window; that stall is what this replaced")
	}
}

// TestStartupNeverReloadsAModelNobodyAskedAbout: the model is resident at
// 8192 for somebody else and config wants 32768. Nothing has wired an
// approver during buildAgent, so there is no one to ask — and no one to ask
// means no, not yes. The session runs inside the window it found.
func TestStartupNeverReloadsAModelNobodyAskedAbout(t *testing.T) {
	stub := newOllamaProbeStub(t, `{"models":[{"name":"m","model":"m","context_length":8192}]}`)
	cfg := config.Default()
	cfg.ContextTokens = 0
	cfg.Providers["lan"] = config.ProviderConfig{Type: "ollama", BaseURL: stub.srv.URL}
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("lan", stub.srv.URL, "")
	ag, reg := testAgentFor(t, cfg, p, "m")

	applyModelParams(cfg, p, reg, ag, "m")

	if ag.Window() != 8192 {
		t.Fatalf("window %d; an unasked reload is a change to somebody else's server", ag.Window())
	}
	if p.Options().NumCtx != 8192 {
		t.Fatalf("num_ctx %d; our own requests must not reload it either", p.Options().NumCtx)
	}
	if atomic.LoadInt32(&stub.generate) != 0 {
		t.Fatal("the model was loaded at startup")
	}
}

// TestStartupConsentIsDeferredNotDenied: the registry's approver is the seam
// the UI wires *after* buildAgent, and the loader must read it when it asks
// rather than capture the nil it saw at startup. Otherwise a model switch
// later in the session could never ask either.
func TestStartupConsentIsDeferredNotDenied(t *testing.T) {
	stub := newOllamaProbeStub(t, `{"models":[{"name":"m","model":"m","context_length":8192}]}`)
	cfg := config.Default()
	cfg.ContextTokens = 0
	cfg.Providers["lan"] = config.ProviderConfig{Type: "ollama", BaseURL: stub.srv.URL}
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("lan", stub.srv.URL, "")
	ag, reg := testAgentFor(t, cfg, p, "m")

	applyModelParams(cfg, p, reg, ag, "m")
	if ag.Window() != 8192 {
		t.Fatalf("window %d before an approver existed", ag.Window())
	}

	// The UI comes up and wires its approver, exactly as tui/ui do.
	var asked string
	reg.Approve = func(action, detail string) bool { asked = action; return true }
	if w, err := sessionLoader.Apply(t.Context(), "m"); err != nil || w != 32768 {
		t.Fatalf("window %d err %v; the question must still be askable", w, err)
	}
	if asked != "model_reload" {
		t.Fatalf("asked %q", asked)
	}
}

// TestStartupFitsTheServerWhenNothingIsConfigured: 0.10.0's behaviour, which
// still has to work — and the Modelfile is the fallback when /api/ps has
// nothing, which is the one probe that stays.
func TestStartupFitsTheServerWhenNothingIsConfigured(t *testing.T) {
	stub := newOllamaProbeStub(t, `{"models":[]}`)
	cfg := config.Default()
	cfg.ContextTokens = 0
	cfg.Providers["lan"] = config.ProviderConfig{Type: "ollama", BaseURL: stub.srv.URL}
	p := provider.NewOllama("lan", stub.srv.URL, "")
	ag, reg := testAgentFor(t, cfg, p, "m")

	before := ag.History.Budget
	out := captureStderr(t, func() { applyModelParams(cfg, p, reg, ag, "m") })

	if ag.Window() != 4096 {
		t.Fatalf("window %d; the Modelfile num_ctx is the answer here", ag.Window())
	}
	// The advice must name the budget the session was going to use, not the
	// one it has just been cut down to: "clamped to 4096 ... start the server
	// with OLLAMA_CONTEXT_LENGTH=4096" tells the user to ask for what they
	// already have.
	if !strings.Contains(out, fmt.Sprintf("OLLAMA_CONTEXT_LENGTH=%d", before)) {
		t.Fatalf("advice does not name a window worth asking for (budget was %d):\n%s", before, out)
	}
	if atomic.LoadInt32(&stub.generate) != 0 {
		t.Fatal("an unknown window must not be resolved by loading the model")
	}
}

// TestStartupLeavesNonOllamaBackendsAlone: there is no window to set and no
// budget to clamp on an OpenAI-compatible endpoint.
func TestStartupLeavesNonOllamaBackendsAlone(t *testing.T) {
	cfg := config.Default()
	p := provider.NewOpenAICompat("x", "http://127.0.0.1:1/v1", "")
	ag, reg := testAgentFor(t, cfg, p, "m")
	before := ag.History.Budget

	applyModelParams(cfg, p, reg, ag, "m")

	if ag.Window() != 0 || ag.History.Budget != before {
		t.Fatalf("window %d budget %d", ag.Window(), ag.History.Budget)
	}
}

// TestConfiguredWindowAboveContextTokensIsNotLostSilently: the complaint that
// started this work. context_tokens caps the budget below the window, and
// because the budget was already under the window ApplyWindow reports no
// clamp — so before this, half a deliberately configured 32768-token window
// vanished with nothing printed at all. Note that this test does *not* zero
// cfg.ContextTokens: every existing config file on disk carries a literal,
// and that is exactly the case that was invisible.
func TestConfiguredWindowAboveContextTokensIsNotLostSilently(t *testing.T) {
	stub := newOllamaProbeStub(t, `{"models":[]}`)
	cfg := config.Default()
	cfg.ContextTokens = 16384 // what every pre-existing config.json says
	cfg.Providers["lan"] = config.ProviderConfig{Type: "ollama", BaseURL: stub.srv.URL}
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("lan", stub.srv.URL, "")
	ag, reg := testAgentFor(t, cfg, p, "m")

	out := captureStderr(t, func() { applyModelParams(cfg, p, reg, ag, "m") })

	if ag.Window() != 32768 {
		t.Fatalf("window %d", ag.Window())
	}
	if ag.History.Budget != 16384 {
		t.Fatalf("budget %d; an explicit context_tokens still wins", ag.History.Budget)
	}
	if !strings.Contains(out, "context_tokens=16384") || !strings.Contains(out, "go unused") {
		t.Fatalf("the loss was not reported:\n%s", out)
	}
}

// TestDerivedBudgetUsesTheWholeWindowQuietly: with no context_tokens the
// window is the budget, and there is nothing to warn about.
func TestDerivedBudgetUsesTheWholeWindowQuietly(t *testing.T) {
	stub := newOllamaProbeStub(t, `{"models":[]}`)
	cfg := config.Default() // ContextTokens is 0 here by design now
	cfg.Providers["lan"] = config.ProviderConfig{Type: "ollama", BaseURL: stub.srv.URL}
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("lan", stub.srv.URL, "")
	ag, reg := testAgentFor(t, cfg, p, "m")

	out := captureStderr(t, func() { applyModelParams(cfg, p, reg, ag, "m") })

	if ag.History.Budget != 32768 {
		t.Fatalf("budget %d; an unset context_tokens derives from the window", ag.History.Budget)
	}
	if strings.Contains(out, "warn:") {
		t.Fatalf("nothing is wrong here:\n%s", out)
	}
}

// Ruling T8-a, first half. buildAgent runs before any UI exists, so the
// consent question has nobody to put it to and is refused — correctly, but
// the session then runs at whatever window the server happened to hold,
// with the explanation on stderr, which under a TUI is wiped and in a
// hosted session is a log file. runInteractive and runSessionHost re-run
// the resolution once Registry.Approve is wired; this is that re-run.
func TestTheDeferredQuestionIsAskedOnceAUIExists(t *testing.T) {
	stub := newOllamaProbeStub(t, `{"models":[{"name":"m","model":"m","context_length":8192}]}`)
	cfg := config.Default()
	cfg.ContextTokens = 0
	cfg.Providers["lan"] = config.ProviderConfig{Type: "ollama", BaseURL: stub.srv.URL}
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("lan", stub.srv.URL, "")
	ag, reg := testAgentFor(t, cfg, p, "m")

	applyModelParams(cfg, p, reg, ag, "m")
	if ag.Window() != 8192 {
		t.Fatalf("window %d before an approver existed", ag.Window())
	}

	// The UI comes up: it wires the approval seam and re-runs the loader,
	// exactly as runInteractive and runSessionHost now do.
	asked := make(chan string, 1)
	reg.Approve = func(action, detail string) bool { asked <- action; return true }
	ag.ResolveModel()

	select {
	case action := <-asked:
		if action != "model_reload" {
			t.Fatalf("asked %q", action)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the question was never put to the UI")
	}
	deadline := time.Now().Add(5 * time.Second)
	for ag.Window() != 32768 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if ag.Window() != 32768 {
		t.Fatalf("window %d; the consented window never reached the session", ag.Window())
	}
	if p.Options().NumCtx != 32768 {
		t.Fatalf("num_ctx %d never reached the provider", p.Options().NumCtx)
	}
}

// Ruling T8-a, second half. A loader notice is only useful where it can be
// read: the transcript when a UI is up, stderr only while one is not.
func TestLoaderNoticesGoToTheTranscriptWhenThereIsOne(t *testing.T) {
	stub := newOllamaProbeStub(t, `{"models":[{"name":"m","model":"m","context_length":8192}]}`)
	cfg := config.Default()
	cfg.ContextTokens = 0
	cfg.ReloadOnMismatch = "never" // the loader explains itself and keeps 8192
	cfg.Providers["lan"] = config.ProviderConfig{Type: "ollama", BaseURL: stub.srv.URL}
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("lan", stub.srv.URL, "")
	ag, reg := testAgentFor(t, cfg, p, "m")

	var mu sync.Mutex
	var notices []string
	ag.Events.OnNotice = func(s string) { mu.Lock(); notices = append(notices, s); mu.Unlock() }

	out := captureStderr(t, func() { applyModelParams(cfg, p, reg, ag, "m") })

	mu.Lock()
	got := strings.Join(notices, "\n")
	mu.Unlock()
	if !strings.Contains(got, "reload_on_mismatch") {
		t.Fatalf("the reason never reached the transcript: %q", got)
	}
	if strings.Contains(out, "reload_on_mismatch") {
		t.Fatalf("it went to stderr as well, where a TUI wipes it:\n%s", out)
	}
}
