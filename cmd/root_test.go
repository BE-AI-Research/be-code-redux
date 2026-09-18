package cmd

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

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

	if ag.Window != 32768 {
		t.Fatalf("window %d", ag.Window)
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

	if ag.Window != 8192 {
		t.Fatalf("window %d; an unasked reload is a change to somebody else's server", ag.Window)
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
	if ag.Window != 8192 {
		t.Fatalf("window %d before an approver existed", ag.Window)
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

	applyModelParams(cfg, p, reg, ag, "m")

	if ag.Window != 4096 {
		t.Fatalf("window %d; the Modelfile num_ctx is the answer here", ag.Window)
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

	if ag.Window != 0 || ag.History.Budget != before {
		t.Fatalf("window %d budget %d", ag.Window, ag.History.Budget)
	}
}
