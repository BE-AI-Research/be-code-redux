package cmd

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

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
