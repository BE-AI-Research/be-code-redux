package loader

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// stub serves /api/ps and /api/show and counts what was asked of it.
func stub(t *testing.T, ps, show string) (*httptest.Server, *int32) {
	t.Helper()
	var shows int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			w.Write([]byte(ps))
		case "/api/show":
			atomic.AddInt32(&shows, 1)
			w.Write([]byte(show))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &shows
}

const psLoaded8k = `{"models":[{"name":"m","model":"m","context_length":8192}]}`
const psEmpty = `{"models":[]}`

// TestExplicitWindowSkipsTheProbe: the user configured a window, so the
// loader must not ask the server what it should be — probing costs minutes
// when it has to load the model to find out.
func TestExplicitWindowSkipsTheProbe(t *testing.T) {
	srv, shows := stub(t, psEmpty, `{"parameters":"num_ctx 4096"}`)
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	l := New(provider.NewOllama("t", srv.URL, ""), cfg, refuse(t), func(string) {})
	w, err := l.Apply(context.Background(), "m")
	if err != nil || w != 32768 {
		t.Fatalf("window %d err %v", w, err)
	}
	if atomic.LoadInt32(shows) != 0 {
		t.Fatalf("probed %d times with an explicit window", *shows)
	}
}

// TestNotResidentNeedsNoConsent: nothing is holding the model, so loading it
// at our window evicts nobody and must not interrupt the user.
func TestNotResidentNeedsNoConsent(t *testing.T) {
	srv, _ := stub(t, psEmpty, `{}`)
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	l := New(provider.NewOllama("t", srv.URL, ""), cfg, refuse(t), func(string) {})
	if w, _ := l.Apply(context.Background(), "m"); w != 32768 {
		t.Fatalf("window %d", w)
	}
}

// TestMismatchAsksBeforeChangingASharedServer: the model is loaded at 8192
// and config wants 32768. Reloading would evict other applications, so the
// loader asks; a refusal keeps the server's window.
func TestMismatchAsksBeforeChangingASharedServer(t *testing.T) {
	srv, _ := stub(t, psLoaded8k, `{}`)
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	var asked string
	l := New(provider.NewOllama("t", srv.URL, ""), cfg,
		func(action, detail string) bool { asked = action + "|" + detail; return false },
		func(string) {})
	w, _ := l.Apply(context.Background(), "m")
	if !strings.HasPrefix(asked, "model_reload|") {
		t.Fatalf("did not ask: %q", asked)
	}
	if !strings.Contains(asked, "evicts") {
		t.Fatalf("the prompt must say what it costs: %q", asked)
	}
	if w != 8192 {
		t.Fatalf("a refusal must keep the server's window, got %d", w)
	}
}

// TestNonInteractiveNeverPrompts: a headless run has no one to ask, and a
// missing approver is a refusal, never a silent yes.
func TestNonInteractiveNeverPrompts(t *testing.T) {
	srv, _ := stub(t, psLoaded8k, `{}`)
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	l := New(provider.NewOllama("t", srv.URL, ""), cfg, nil, func(string) {})
	if w, _ := l.Apply(context.Background(), "m"); w != 8192 {
		t.Fatalf("window %d; a nil approver must mean no change", w)
	}
}

// TestUnsetWindowFitsTheServer: with nothing configured the server is the
// authority, which is 0.10.0's behaviour, now in one place.
func TestUnsetWindowFitsTheServer(t *testing.T) {
	srv, _ := stub(t, psLoaded8k, `{}`)
	l := New(provider.NewOllama("t", srv.URL, ""), config.Default(), refuse(t), func(string) {})
	if w, _ := l.Apply(context.Background(), "m"); w != 8192 {
		t.Fatalf("window %d", w)
	}
}

// TestPerModelOverridesTheProvider: the models map wins over the provider
// block, because parameters belong to the model.
func TestPerModelOverridesTheProvider(t *testing.T) {
	srv, _ := stub(t, psEmpty, `{}`)
	cfg := config.Default()
	cfg.Providers["ollama"] = config.ProviderConfig{Type: "ollama", BaseURL: srv.URL, ContextWindow: 16384}
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	l := New(provider.NewOllama("t", srv.URL, ""), cfg, refuse(t), func(string) {})
	if w, _ := l.Apply(context.Background(), "m"); w != 32768 {
		t.Fatalf("window %d", w)
	}
}

// TestWindowChangedByAnotherClientIsAdaptedTo: another client reloaded the
// model. We re-derive our budget and leave theirs alone — a reload war
// between two clients is the worst outcome available.
func TestWindowChangedByAnotherClientIsAdaptedTo(t *testing.T) {
	srv, _ := stub(t, psLoaded8k, `{}`)
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	var reloads int
	l := New(provider.NewOllama("t", srv.URL, ""), cfg,
		func(string, string) bool { reloads++; return true }, func(string) {})
	l.OnWindowChanged("m", 4096)
	if reloads != 0 {
		t.Fatalf("asked to reload %d times after someone else changed the window", reloads)
	}
}

// refuse fails the test if consent is ever requested.
func refuse(t *testing.T) tools.ApproveFunc {
	return func(action, detail string) bool {
		t.Fatalf("consent asked for when none was needed: %s %s", action, detail)
		return false
	}
}

// ---- beyond the brief ------------------------------------------------------

// TestProviderBlockWindowIsUsedWhenNoModelEntry: the provider block is the
// fallback, looked up by the provider's configured name.
func TestProviderBlockWindowIsUsedWhenNoModelEntry(t *testing.T) {
	srv, shows := stub(t, psEmpty, `{"parameters":"num_ctx 4096"}`)
	cfg := config.Default()
	cfg.Providers["lan"] = config.ProviderConfig{Type: "ollama", BaseURL: srv.URL, ContextWindow: 16384}
	l := New(provider.NewOllama("lan", srv.URL, ""), cfg, refuse(t), func(string) {})
	if w, _ := l.Apply(context.Background(), "m"); w != 16384 {
		t.Fatalf("window %d", w)
	}
	if atomic.LoadInt32(shows) != 0 {
		t.Fatalf("probed with an explicit provider window")
	}
}

// TestAlwaysReloadsWithoutAsking: the user has already given standing
// consent for this server, so a mismatch is simply applied.
func TestAlwaysReloadsWithoutAsking(t *testing.T) {
	srv, _ := stub(t, psLoaded8k, `{}`)
	cfg := config.Default()
	cfg.ReloadOnMismatch = "always"
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("t", srv.URL, "")
	l := New(p, cfg, refuse(t), func(string) {})
	if w, _ := l.Apply(context.Background(), "m"); w != 32768 {
		t.Fatalf("window %d", w)
	}
	if p.Options().NumCtx != 32768 {
		t.Fatalf("num_ctx %d", p.Options().NumCtx)
	}
}

// TestNeverKeepsTheServerWindowWithoutAsking: "never" is a standing no.
func TestNeverKeepsTheServerWindowWithoutAsking(t *testing.T) {
	srv, _ := stub(t, psLoaded8k, `{}`)
	cfg := config.Default()
	cfg.ReloadOnMismatch = "never"
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("t", srv.URL, "")
	var notes []string
	l := New(p, cfg, refuse(t), func(s string) { notes = append(notes, s) })
	if w, _ := l.Apply(context.Background(), "m"); w != 8192 {
		t.Fatalf("window %d", w)
	}
	// Our requests must ask for the window the model already has, or every
	// one of them would reload it behind the user's back.
	if p.Options().NumCtx != 8192 {
		t.Fatalf("num_ctx %d; a declined reload must not be smuggled onto the wire", p.Options().NumCtx)
	}
	if len(notes) == 0 {
		t.Fatalf("a silently clamped session is not acceptable")
	}
}

// TestConsentIsRememberedForTheSession: saying yes once is not a reason to
// ask again for the same model.
func TestConsentIsRememberedForTheSession(t *testing.T) {
	srv, _ := stub(t, psLoaded8k, `{}`)
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	asks := 0
	l := New(provider.NewOllama("t", srv.URL, ""), cfg,
		func(string, string) bool { asks++; return true }, func(string) {})
	for i := 0; i < 3; i++ {
		if w, _ := l.Apply(context.Background(), "m"); w != 32768 {
			t.Fatalf("window %d", w)
		}
	}
	if asks != 1 {
		t.Fatalf("asked %d times for one model", asks)
	}
}

// TestRefusalIsRememberedForTheSession: so is saying no.
func TestRefusalIsRememberedForTheSession(t *testing.T) {
	srv, _ := stub(t, psLoaded8k, `{}`)
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	asks := 0
	l := New(provider.NewOllama("t", srv.URL, ""), cfg,
		func(string, string) bool { asks++; return false }, func(string) {})
	for i := 0; i < 3; i++ {
		if w, _ := l.Apply(context.Background(), "m"); w != 8192 {
			t.Fatalf("window %d", w)
		}
	}
	if asks != 1 {
		t.Fatalf("asked %d times after a no", asks)
	}
}

// TestKeepAliveAndOptionsReachTheProvider: the loader is the one place a
// model's parameters are decided, so keep_alive and the passthrough map
// travel with the window.
func TestKeepAliveAndOptionsReachTheProvider(t *testing.T) {
	srv, _ := stub(t, psEmpty, `{}`)
	cfg := config.Default()
	cfg.Providers["lan"] = config.ProviderConfig{
		Type: "ollama", BaseURL: srv.URL,
		KeepAlive: "10m", Options: map[string]any{"top_k": 40, "top_p": 0.9},
	}
	cfg.Models = map[string]config.ModelConfig{"m": {
		ContextWindow: 32768, KeepAlive: "30m", Options: map[string]any{"top_p": 0.95},
	}}
	p := provider.NewOllama("lan", srv.URL, "")
	l := New(p, cfg, refuse(t), func(string) {})
	if _, err := l.Apply(context.Background(), "m"); err != nil {
		t.Fatal(err)
	}
	if got := l.Params("m").KeepAlive; got != 30*time.Minute {
		t.Fatalf("keep_alive %v; the model entry must win", got)
	}
	o := p.Options()
	if o.Extra["top_k"] != 40 {
		t.Fatalf("provider option lost: %v", o.Extra)
	}
	if o.Extra["top_p"] != 0.95 {
		t.Fatalf("model option must override the provider's: %v", o.Extra)
	}
}

// TestNonOllamaProviderIsLeftAlone: only Ollama has a window to set.
func TestNonOllamaProviderIsLeftAlone(t *testing.T) {
	cfg := config.Default()
	l := New(provider.NewOpenAICompat("x", "http://127.0.0.1:1/v1", ""), cfg, refuse(t), func(string) {})
	if w, err := l.Apply(context.Background(), "m"); w != 0 || err != nil {
		t.Fatalf("window %d err %v", w, err)
	}
}

// TestEvictionNeedsNoConsent: nothing is holding the model, so the reload
// the next request causes anyway carries our window.
func TestEvictionNeedsNoConsent(t *testing.T) {
	srv, _ := stub(t, psEmpty, `{}`)
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("t", srv.URL, "")
	l := New(p, cfg, refuse(t), func(string) {})
	l.OnEvicted(context.Background(), "m")
	if p.Options().NumCtx != 32768 {
		t.Fatalf("num_ctx %d after an eviction", p.Options().NumCtx)
	}
}

// TestWindowChangedAdaptsOurOwnRequests: after another client's reload our
// requests must carry their window, not ours, or every request reloads the
// model back and the two clients fight.
func TestWindowChangedAdaptsOurOwnRequests(t *testing.T) {
	srv, _ := stub(t, psLoaded8k, `{}`)
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("t", srv.URL, "")
	l := New(p, cfg, refuse(t), func(string) {})
	l.OnWindowChanged("m", 4096)
	if p.Options().NumCtx != 4096 {
		t.Fatalf("num_ctx %d; we must run inside the window they chose", p.Options().NumCtx)
	}
}

// TestWindowChangedWithStandingConsentReapplies: "always" is the one place
// consent already exists, so our window goes back on the wire.
func TestWindowChangedWithStandingConsentReapplies(t *testing.T) {
	srv, _ := stub(t, psLoaded8k, `{}`)
	cfg := config.Default()
	cfg.ReloadOnMismatch = "always"
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("t", srv.URL, "")
	l := New(p, cfg, refuse(t), func(string) {})
	l.OnWindowChanged("m", 4096)
	if p.Options().NumCtx != 32768 {
		t.Fatalf("num_ctx %d", p.Options().NumCtx)
	}
}

// TestUnreachableServerIsNotFatal: a backend that is not there yet leaves
// the window unknown; the first real request reports the failure.
func TestUnreachableServerIsNotFatal(t *testing.T) {
	cfg := config.Default()
	p := provider.NewOllama("t", "http://127.0.0.1:1", "")
	l := New(p, cfg, refuse(t), func(string) {})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if w, _ := l.Apply(ctx, "m"); w != 0 {
		t.Fatalf("window %d from an unreachable server", w)
	}
}

// TestResidentWithAnUnreportedWindowIsNotReloaded: an older /api/ps lists
// the model without a context_length. We cannot tell whether our num_ctx
// would reload it, and guessing wrong evicts somebody, so nothing is sent
// and nothing is asked.
func TestResidentWithAnUnreportedWindowIsNotReloaded(t *testing.T) {
	srv, _ := stub(t, `{"models":[{"name":"m","model":"m"}]}`, `{}`)
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768, Options: map[string]any{"top_k": 40}}}
	p := provider.NewOllama("t", srv.URL, "")
	var notes []string
	l := New(p, cfg, refuse(t), func(s string) { notes = append(notes, s) })
	w, err := l.Apply(context.Background(), "m")
	if w != 0 || err != nil {
		t.Fatalf("window %d err %v; an unknown window is unknown, not ours to set", w, err)
	}
	if p.Options().NumCtx != 0 {
		t.Fatalf("num_ctx %d; sending one could reload a model somebody else is using", p.Options().NumCtx)
	}
	if p.Options().Extra["top_k"] != 40 {
		t.Fatalf("passthrough options lost: %v", p.Options().Extra)
	}
	if len(notes) == 0 {
		t.Fatal("no notice")
	}
}

// TestPassthroughOptionsSurviveAnUnknownWindow: the window may be the
// server's business, but the options map is ours either way.
func TestPassthroughOptionsSurviveAnUnknownWindow(t *testing.T) {
	srv, _ := stub(t, psEmpty, `{}`)
	cfg := config.Default()
	cfg.Providers["lan"] = config.ProviderConfig{
		Type: "ollama", BaseURL: srv.URL, Options: map[string]any{"top_k": 40},
	}
	p := provider.NewOllama("lan", srv.URL, "")
	l := New(p, cfg, refuse(t), func(string) {})
	if w, _ := l.Apply(context.Background(), "m"); w != 0 {
		t.Fatalf("window %d", w)
	}
	if p.Options().Extra["top_k"] != 40 {
		t.Fatalf("passthrough options lost: %v", p.Options().Extra)
	}
}

// ---- fix round 1 -----------------------------------------------------------

// TestUnreadablePsIsNotALicenceToReload: /api/ps could not be read, so it is
// unknown whether another application is holding this model. An unknown is
// not consent. Assuming the configured window here would evict somebody on
// the user's shared box with nobody asked, which is the one thing this
// package exists to prevent.
func TestUnreadablePsIsNotALicenceToReload(t *testing.T) {
	// Shape one: a proxy in front of the server answers with an error page,
	// which is not JSON and so never yields a model list.
	t.Run("proxy error page", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte("<html><head><title>502 Bad Gateway</title></head></html>"))
		}))
		defer srv.Close()
		assertNoWindowSentWithoutConsent(t, srv.URL, context.Background())
	})
	// Shape two: the probe simply does not come back in time.
	t.Run("probe timeout", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(2 * time.Second)
			w.Write([]byte(psLoaded8k))
		}))
		defer srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		assertNoWindowSentWithoutConsent(t, srv.URL, ctx)
	})
}

func assertNoWindowSentWithoutConsent(t *testing.T, url string, ctx context.Context) {
	t.Helper()
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768, Options: map[string]any{"top_k": 40}}}
	p := provider.NewOllama("t", url, "")
	var notes []string
	l := New(p, cfg, refuse(t), func(s string) { notes = append(notes, s) })

	w, _ := l.Apply(ctx, "m")
	if w != 0 {
		t.Fatalf("window %d; an unreadable server is an unknown, not an answer", w)
	}
	if got := p.Options().NumCtx; got != 0 {
		t.Fatalf("num_ctx %d went on the wire unasked; that reloads and evicts whoever else holds the model", got)
	}
	if p.Options().Extra["top_k"] != 40 {
		t.Fatalf("passthrough options lost: %v", p.Options().Extra)
	}
	if len(notes) == 0 {
		t.Fatal("a session quietly running without its configured window is not acceptable")
	}
	// Nothing was decided, so nothing may be latched: once the server answers
	// again the question must still be live.
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.asked["m"] || l.agreed["m"] {
		t.Fatal("a failed probe latched a decision nobody made")
	}
}

// TestConcurrentApplyAsksOnceAndAgrees: a startup Apply, a model switch and
// a recovery can all want the same model at once. Two modals for one
// question is bad; two different answers reaching two callers while a third
// value sits on the wire is worse.
func TestConcurrentApplyAsksOnceAndAgrees(t *testing.T) {
	srv, _ := stub(t, psLoaded8k, `{}`)
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("t", srv.URL, "")

	var asks int32
	var l *Loader
	l = New(p, cfg, func(string, string) bool {
		// Yes to the first caller, no to any other — the shape that used to
		// leave one caller holding 32768 while the wire ended at 8192.
		first := atomic.AddInt32(&asks, 1) == 1
		time.Sleep(20 * time.Millisecond) // a person reading the prompt
		return first
	}, func(string) {})

	const callers = 8
	got := make([]int, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], _ = l.Apply(context.Background(), "m")
		}(i)
	}
	wg.Wait()

	if n := atomic.LoadInt32(&asks); n != 1 {
		t.Fatalf("asked %d times for one model", n)
	}
	for i, w := range got {
		if w != got[0] {
			t.Fatalf("caller %d got %d, caller 0 got %d; one question has one answer", i, w, got[0])
		}
	}
	if got[0] != p.Options().NumCtx {
		t.Fatalf("callers were told %d but %d reached the wire", got[0], p.Options().NumCtx)
	}
	if got[0] != 32768 {
		t.Fatalf("the answer given was yes; window %d", got[0])
	}
}

// ---- fix round 2 -----------------------------------------------------------

// TestUnconfiguredWindowNeverAdoptsTheModelfileOverALoadedModel: the model is
// resident but /api/ps does not say at what size, so ContextLength falls back
// to the Modelfile. A Modelfile describes the load that *would* happen, not
// the one that already has: sending its 4096 for a model somebody else loaded
// at something else reloads and evicts it, with nobody asked. With no
// configured window there is not even a preference to put to the user, so the
// only correct move is to leave the running model alone.
func TestUnconfiguredWindowNeverAdoptsTheModelfileOverALoadedModel(t *testing.T) {
	srv, shows := stub(t, `{"models":[{"name":"m","model":"m"}]}`, `{"parameters":"num_ctx 4096"}`)
	cfg := config.Default() // no models entry, no provider context_window
	p := provider.NewOllama("t", srv.URL, "")
	var notes []string
	l := New(p, cfg, refuse(t), func(s string) { notes = append(notes, s) })

	w, err := l.Apply(context.Background(), "m")
	if w != 0 || err != nil {
		t.Fatalf("window %d err %v; a loaded model's window is not the Modelfile's", w, err)
	}
	if got := p.Options().NumCtx; got != 0 {
		t.Fatalf("num_ctx %d went on the wire for a model that is loaded at something else", got)
	}
	if atomic.LoadInt32(shows) != 0 {
		t.Fatalf("asked the Modelfile %d times about a model that is already loaded", *shows)
	}
	if len(notes) == 0 {
		t.Fatal("no notice")
	}
}

// TestUnconfiguredWindowStillAdoptsTheModelfileWhenNothingIsLoaded: nothing
// is holding the model, so the Modelfile describes the load that is about to
// happen and is safe to adopt. This is 0.10.0's behaviour and must survive.
func TestUnconfiguredWindowStillAdoptsTheModelfileWhenNothingIsLoaded(t *testing.T) {
	srv, _ := stub(t, psEmpty, `{"parameters":"num_ctx 4096"}`)
	p := provider.NewOllama("t", srv.URL, "")
	l := New(p, config.Default(), refuse(t), func(string) {})
	if w, _ := l.Apply(context.Background(), "m"); w != 4096 {
		t.Fatalf("window %d", w)
	}
	if p.Options().NumCtx != 4096 {
		t.Fatalf("num_ctx %d", p.Options().NumCtx)
	}
}

// TestUnconfiguredWindowWithAnUnreadableServerSendsNothing: the same rule as
// the configured arm — an unknown is not a licence.
func TestUnconfiguredWindowWithAnUnreadableServerSendsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte("<html>502</html>"))
	}))
	defer srv.Close()
	p := provider.NewOllama("t", srv.URL, "")
	var notes []string
	l := New(p, config.Default(), refuse(t), func(s string) { notes = append(notes, s) })
	if w, _ := l.Apply(context.Background(), "m"); w != 0 {
		t.Fatalf("window %d", w)
	}
	if p.Options().NumCtx != 0 {
		t.Fatalf("num_ctx %d", p.Options().NumCtx)
	}
	if len(notes) == 0 {
		t.Fatal("no notice")
	}
}

// TestWaitingForSomeoneElsesAnswerIsCancellable: an ask takes as long as a
// person takes to read it. A caller parked behind one must stay cancellable,
// or "somebody is being asked" becomes a hang with no way out — including
// the shape that used to be a hard deadlock, a second Apply for the same
// model raised from inside the approver.
func TestWaitingForSomeoneElsesAnswerIsCancellable(t *testing.T) {
	srv, _ := stub(t, psLoaded8k, `{}`)
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("t", srv.URL, "")

	asking := make(chan struct{})
	release := make(chan struct{})
	var reentrant int
	var reentrantWindow int
	var l *Loader
	l = New(p, cfg, func(string, string) bool {
		// While this ask is open, a second caller wants the same model and
		// gives up rather than waiting forever.
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		reentrantWindow, _ = l2Apply(l, ctx, "m")
		reentrant++
		close(asking)
		<-release
		return true
	}, func(string) {})

	go func() { <-asking; close(release) }()
	w, _ := l.Apply(context.Background(), "m")

	if reentrant != 1 {
		t.Fatalf("the nested call did not run (%d)", reentrant)
	}
	if reentrantWindow != 8192 {
		t.Fatalf("a caller that gave up waiting got %d; it must keep what the server has", reentrantWindow)
	}
	if w != 32768 {
		t.Fatalf("the answered call got %d", w)
	}
}

// l2Apply exists only to make the nested call above read as what it is.
func l2Apply(l *Loader, ctx context.Context, model string) (int, error) {
	return l.Apply(ctx, model)
}

// A caller that gives up waiting for somebody else's answer must budget
// against the server's window but put nothing on the wire. The claimer is
// releasing at that very instant, so a write here lands *after* the
// consented window and silently undoes it — always downward, so never an
// eviction, but the next caller is then truncated with nothing said.
func TestACancelledWaiterPutsNothingOnTheWire(t *testing.T) {
	const sentinel = 12345
	srv, _ := stub(t, psLoaded8k, `{}`)
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("t", srv.URL, "")

	asking := make(chan struct{})
	release := make(chan struct{})
	type waited struct {
		window  int
		onWire  int
		problem string
	}
	result := make(chan waited, 1)

	var l *Loader
	l = New(p, cfg, func(string, string) bool {
		close(asking)
		<-release
		return true
	}, func(string) {})

	go func() {
		<-asking
		// A window nothing in this test would ever choose, so anything the
		// waiter writes is visible.
		p.SetOptions(provider.Options{NumCtx: sentinel})
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		w, err := l.Apply(ctx, "m")
		r := waited{window: w, onWire: p.Options().NumCtx}
		if err != nil {
			r.problem = err.Error()
		}
		result <- r
		close(release)
	}()

	w, err := l.Apply(context.Background(), "m")
	if err != nil || w != 32768 {
		t.Fatalf("the answered call got %d (%v)", w, err)
	}
	r := <-result
	if r.problem != "" {
		t.Fatalf("the waiter failed: %s", r.problem)
	}
	if r.window != 8192 {
		t.Fatalf("a caller that gave up waiting got %d; it budgets against what the server has", r.window)
	}
	if r.onWire != sentinel {
		t.Fatalf("the cancelled waiter wrote %d to the provider; it must write nothing", r.onWire)
	}
	if p.Options().NumCtx != 32768 {
		t.Fatalf("the consented window did not survive: %d", p.Options().NumCtx)
	}
}

// The "nobody to ask yet" notice describes a state an interactive session
// leaves within moments: it re-runs Apply once its UI has wired an
// approver. It must not share a notice key with the real answer, or a
// denial a moment later explains itself to nobody.
func TestTheDeferredNoticeDoesNotSwallowTheRealOne(t *testing.T) {
	srv, _ := stub(t, psLoaded8k, `{}`)
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	var notes []string
	l := New(provider.NewOllama("t", srv.URL, ""), cfg, nil, func(s string) { notes = append(notes, s) })

	if w, _ := l.Apply(context.Background(), "m"); w != 8192 {
		t.Fatalf("no approver is a refusal; got %d", w)
	}
	// The UI comes up and the user says no.
	l.SetApprover(func() tools.ApproveFunc { return func(string, string) bool { return false } })
	if w, _ := l.Apply(context.Background(), "m"); w != 8192 {
		t.Fatalf("a denial keeps the server's window; got %d", w)
	}
	if len(notes) != 2 {
		t.Fatalf("expected the deferred line and then the denial, got %d: %v", len(notes), notes)
	}
	if !strings.Contains(notes[1], "clamped") {
		t.Fatalf("the denial did not explain itself: %q", notes[1])
	}
}

// A UI that can withdraw its own prompt is preferred, and it is handed this
// caller's context so it can. A UI that cannot is still asked, exactly as
// before — the plain approver is wrapped, not ignored.
func TestTheContextAwareApproverIsPreferredAndGetsTheCallersContext(t *testing.T) {
	srv, _ := stub(t, psLoaded8k, `{}`)
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}

	var plainAsked, ctxAsked int
	var seen context.Context
	l := New(provider.NewOllama("t", srv.URL, ""), cfg, nil, func(string) {})
	l.SetApprover(func() tools.ApproveFunc {
		return func(string, string) bool { plainAsked++; return true }
	})
	l.SetApproverCtx(func() tools.ApproveCtxFunc {
		return func(ctx context.Context, _, _ string) bool { ctxAsked++; seen = ctx; return true }
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if w, err := l.Apply(ctx, "m"); err != nil || w != 32768 {
		t.Fatalf("window %d err %v", w, err)
	}
	if ctxAsked != 1 || plainAsked != 0 {
		t.Fatalf("asked ctx=%d plain=%d", ctxAsked, plainAsked)
	}
	if seen == nil {
		t.Fatal("no context reached the approver")
	}
	if _, ok := seen.Deadline(); !ok {
		t.Fatal("the approver got a context that can never expire; it could not withdraw its own prompt")
	}
}

// With nothing context-aware wired, the plain approver still decides.
func TestThePlainApproverStillDecidesWhenThereIsNoContextAwareOne(t *testing.T) {
	srv, _ := stub(t, psLoaded8k, `{}`)
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	asked := 0
	l := New(provider.NewOllama("t", srv.URL, ""), cfg, nil, func(string) {})
	l.SetApprover(func() tools.ApproveFunc {
		return func(string, string) bool { asked++; return false }
	})
	l.SetApproverCtx(func() tools.ApproveCtxFunc { return nil })

	if w, _ := l.Apply(context.Background(), "m"); w != 8192 {
		t.Fatalf("a denial keeps the server's window; got %d", w)
	}
	if asked != 1 {
		t.Fatalf("asked %d times", asked)
	}
}
