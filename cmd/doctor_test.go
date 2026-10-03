package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
