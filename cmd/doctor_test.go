package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/brown-enterprises/be-code/internal/browser/browsertest"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// captureStdout runs fn with os.Stdout redirected and returns what it wrote.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = orig
	w.Close()
	b, _ := io.ReadAll(r)
	r.Close()
	return string(b)
}

// doctor's job on this line is to answer "am I running in the window I
// think I am". That is context_window against what the model is loaded
// with — the comparison that decides whether a request reloads somebody
// else's model. context_tokens is only a cap on our own budget and answers
// a different question, so it must not be the one being compared.
func TestDoctorComparesTheConfiguredWindowNotContextTokens(t *testing.T) {
	stub := newOllamaProbeStub(t, `{"models":[{"name":"m","model":"m","context_length":8192}]}`)
	cfg := config.Default()
	cfg.ContextTokens = 16384
	cfg.ReloadOnMismatch = "ask"
	cfg.Providers["lan"] = config.ProviderConfig{Type: "ollama", BaseURL: stub.srv.URL, DefaultModel: "m"}
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	p := provider.NewOllama("lan", stub.srv.URL, "")

	out := captureStdout(t, func() { reportWindow(context.Background(), p, cfg, "lan") })

	if !strings.Contains(out, "context_window=32768") {
		t.Fatalf("doctor never mentions the number the harness actually sends:\n%s", out)
	}
	if !strings.Contains(out, "MISMATCH") || !strings.Contains(out, "evicts") {
		t.Fatalf("the reload that a mismatch causes is not reported:\n%s", out)
	}
	if !strings.Contains(out, "reload_on_mismatch=ask") {
		t.Fatalf("the setting that decides what happens is not named:\n%s", out)
	}
	if !strings.Contains(out, "context_tokens=16384") {
		t.Fatalf("a budget cap the user set is still worth a line:\n%s", out)
	}
}

// Nothing configured and nothing loaded: the Modelfile is all there is, and
// there is no mismatch to report.
func TestDoctorReportsTheModelfileWhenNothingIsLoaded(t *testing.T) {
	stub := newOllamaProbeStub(t, `{"models":[]}`)
	cfg := config.Default()
	cfg.ContextTokens = 0
	cfg.Providers["lan"] = config.ProviderConfig{Type: "ollama", BaseURL: stub.srv.URL, DefaultModel: "m"}
	p := provider.NewOllama("lan", stub.srv.URL, "")

	out := captureStdout(t, func() { reportWindow(context.Background(), p, cfg, "lan") })

	if !strings.Contains(out, "context_window unset") {
		t.Fatalf("an unset window is worth saying plainly:\n%s", out)
	}
	if !strings.Contains(out, "Modelfile says 4096") {
		t.Fatalf("the fallback window was not reported:\n%s", out)
	}
	if strings.Contains(out, "MISMATCH") || strings.Contains(out, "context_tokens") {
		t.Fatalf("nothing is wrong here:\n%s", out)
	}
}

func TestBrowserDoctorLine(t *testing.T) {
	cfg := config.Default()
	if got := browserDoctorLine(cfg); got != "browser: off (set browser.enabled in config to enable)" {
		t.Fatalf("off: %q", got)
	}
	cfg.Browser.Enabled = true
	fb := browsertest.New(t)
	cfg.Browser.Address = fb.Addr()
	if got := browserDoctorLine(cfg); got != "browser: FakeChrome/1.0 listening at "+fb.Addr() {
		t.Fatalf("listening: %q", got)
	}
	cfg.Browser.Address, cfg.Browser.Launch = "127.0.0.1:1", false
	if got := browserDoctorLine(cfg); got != "browser: nothing at 127.0.0.1:1, and browser.launch is off" {
		t.Fatalf("launch off: %q", got)
	}
}

func TestBrowserDoctorLineMyChrome(t *testing.T) {
	cfg := config.Default()
	cfg.Browser.Enabled, cfg.Browser.UseMyChrome = true, true
	dir := t.TempDir()
	cfg.Browser.ChromeUserDataDir = dir
	want := "browser: your Chrome (" + dir + ") — no DevToolsActivePort in " + dir + "; turn remote debugging on at chrome://inspect/#remote-debugging"
	if got := browserDoctorLine(cfg); got != want {
		t.Fatalf("missing:\n got %q\nwant %q", got, want)
	}
	os.WriteFile(filepath.Join(dir, "DevToolsActivePort"), []byte("junk"), 0o600)
	if got := browserDoctorLine(cfg); !strings.HasPrefix(got, "browser: your Chrome ("+dir+") — DevToolsActivePort in "+dir+" is unreadable") ||
		!strings.Contains(got, "chrome://inspect/#remote-debugging") {
		t.Fatalf("unreadable: %q", got)
	}
	// A port file is reported, never connected to: connecting would raise
	// Chrome's "Allow remote debugging?" prompt from a health check.
	fb := browsertest.New(t)
	fb.SetWSOnly(true)
	fb.WritePortFile(dir)
	want = fmt.Sprintf("browser: your Chrome (%s) — DevToolsActivePort names port %d (not connected: Chrome asks you to allow the first browser call)", dir, fb.Port())
	if got := browserDoctorLine(cfg); got != want {
		t.Fatalf("present:\n got %q\nwant %q", got, want)
	}
	if len(fb.Calls("")) != 0 || fb.HTTPHits() != 0 {
		t.Fatal("doctor connected to the person's Chrome")
	}
}

func TestHeadlessApproverBrowserActions(t *testing.T) {
	cfg := config.Default()
	cfg.AutoApproveBrowser, cfg.AutoApproveShell = true, true
	ap := headlessApprover(cfg)
	if !ap("browser", "act on acme.test?") {
		t.Fatal("-y must allow a default-tier site")
	}
	if ap("browser_watch", "act on github.com? (watched: every action asks)") {
		t.Fatal("-y allowed a watched site with nobody watching")
	}
	if ap("shell_after_web", "echo hi") {
		t.Fatal("-y ran shell after an untrusted page")
	}
}

// TestHeadlessApproverBrowserWatchMessageWithoutDashY is fix round 1's Minor
// 4: without -y, browser_watch and shell_after_web still deny (as every
// action does non-interactively), but the message must not promise that -y
// would have helped, since it never approves either one.
func TestHeadlessApproverBrowserWatchMessageWithoutDashY(t *testing.T) {
	oldTTY := stdinIsTTY
	stdinIsTTY = func() bool { return false }
	t.Cleanup(func() { stdinIsTTY = oldTTY })
	cfg := config.Default() // AutoApproveBrowser and AutoApproveShell both off
	ap := headlessApprover(cfg)
	for _, action := range []string{"browser_watch", "shell_after_web"} {
		out := captureStderr(t, func() { ap(action, "detail") })
		if !strings.Contains(out, "this action always asks a person") {
			t.Fatalf("%s: message still promises -y would help:\n%s", action, out)
		}
		if strings.Contains(out, "use -y to auto-approve") {
			t.Fatalf("%s: message wrongly kept the -y hint:\n%s", action, out)
		}
	}
}

func TestHeadlessApproverRefusesToolCall(t *testing.T) {
	cfg := config.Default()
	cfg.AutoApproveBrowser, cfg.AutoApproveShell = true, true
	if headlessApprover(cfg)("tool_call", "A scheduled event wants to call web_fetch") {
		t.Fatal("-y approved a tool_call")
	}
	oldTTY := stdinIsTTY
	stdinIsTTY = func() bool { return false }
	t.Cleanup(func() { stdinIsTTY = oldTTY })
	out := captureStderr(t, func() {
		if headlessApprover(config.Default())("tool_call", "x") {
			t.Error("non-interactive approved a tool_call")
		}
	})
	if !strings.Contains(out, "this action always asks a person") {
		t.Fatalf("message:\n%s", out)
	}
}

// A max_tokens at least half the context reserves most of it for one reply;
// doctor says so, with the advice the session itself gives.
func TestDoctorMaxTokensLine(t *testing.T) {
	cfg := config.Default()
	cfg.MaxTokens, cfg.ContextTokens = 32768, 32768
	got := maxTokensDoctorLine(cfg, 32768)
	if !strings.Contains(got, "max_tokens 32768 leaves no room for the conversation in a 32768-token window; reserving 16384 instead") ||
		!strings.Contains(got, "Change max_tokens in config (0 lets the server decide)") {
		t.Fatalf("line: %q", got)
	}
	// Judged against the known window when context_tokens is unset.
	cfg.MaxTokens, cfg.ContextTokens = 16384, 0
	if got := maxTokensDoctorLine(cfg, 32768); !strings.Contains(got, "max_tokens 16384") || !strings.Contains(got, "Change max_tokens") {
		t.Fatalf("half the window: %q", got)
	}
	// Nothing to say for a modest value, an unset one, or no known context.
	cfg.MaxTokens, cfg.ContextTokens = 4096, 32768
	if got := maxTokensDoctorLine(cfg, 32768); got != "" {
		t.Fatalf("modest: %q", got)
	}
	cfg.MaxTokens = 0
	if got := maxTokensDoctorLine(cfg, 32768); got != "" {
		t.Fatalf("unset: %q", got)
	}
	cfg.MaxTokens, cfg.ContextTokens = 32768, 0
	if got := maxTokensDoctorLine(cfg, 0); got != "" {
		t.Fatalf("unknown context: %q", got)
	}
}

func onlineTestServer(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(hits, 1)
		fmt.Fprint(w, `{"data":[{"id":"vendor/m","context_length":131072,"pricing":{"prompt":"0.000003","completion":"0.000015"}}]}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func onlineTestConfig(url string) *config.Config {
	cfg := config.Default()
	cfg.DefaultProvider = "or"
	cfg.Model = "vendor/m"
	cfg.Providers = map[string]config.ProviderConfig{
		"or": {Type: "openai", BaseURL: url, APIKeyEnv: "BE_TEST_ONLINE_KEY", Online: true},
	}
	return cfg
}

func TestDoctorOnlineLine(t *testing.T) {
	var hits int32
	srv := onlineTestServer(t, &hits)
	cfg := onlineTestConfig(srv.URL)

	t.Setenv("BE_TEST_ONLINE_KEY", "")
	got := onlineDoctorLine(context.Background(), cfg)
	if !strings.Contains(got, "missing") || atomic.LoadInt32(&hits) != 0 {
		t.Fatalf("missing key must say so and not connect: %q hits=%d", got, hits)
	}

	t.Setenv("BE_TEST_ONLINE_KEY", "k")
	got = onlineDoctorLine(context.Background(), cfg)
	for _, want := range []string{"online: or · vendor/m", "BE_TEST_ONLINE_KEY set", "reachable", "window 131072", "$3.00/$15.00 per Mtok"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in %q", want, got)
		}
	}

	// Final fix 4: an unpriced model with max_spend_usd set says the cap
	// cannot apply.
	noPrice := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"vendor/m","context_length":131072}]}`)
	}))
	defer noPrice.Close()
	cfg2 := onlineTestConfig(noPrice.URL)
	if got := onlineDoctorLine(context.Background(), cfg2); !strings.HasSuffix(got, "· prices unknown") {
		t.Fatalf("unpriced, no cap: %q", got)
	}
	cfg2.MaxSpendUSD = 5
	if got := onlineDoctorLine(context.Background(), cfg2); !strings.Contains(got, "prices unknown (spend is not tracked for this model; max_spend_usd cannot apply)") {
		t.Fatalf("unpriced with cap: %q", got)
	}

	cfg.Providers = map[string]config.ProviderConfig{"or": {Type: "openai", BaseURL: "http://127.0.0.1:1/v1"}}
	if got := onlineDoctorLine(context.Background(), cfg); got != "" {
		t.Fatalf("local default should print nothing, got %q", got)
	}
}
