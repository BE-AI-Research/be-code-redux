package cmd

import (
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/credentials"
)

// I1 (final review): a stored key is a key. A preset with a stored
// credential and no env var starts with no "is not set" warning, reads the
// listing (window and prices from it), and names the stored key — not the
// unset variable — as what a rejection refers to.
func TestResolveOnlineUsesAStoredKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OPENROUTER_API_KEY", "")
	dir, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := credentials.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Put("openrouter", credentials.Credential{Kind: credentials.KindAPIKey, Key: "sk-or-stored", Source: "pasted", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var hits int32
	srv := onlineTestServer(t, &hits)
	cfg := config.Default()
	cfg.DefaultProvider = "openrouter"
	cfg.Model = "vendor/m"
	cfg.Providers = map[string]config.ProviderConfig{
		"openrouter": {Type: "openai", BaseURL: srv.URL, APIKeyEnv: "OPENROUTER_API_KEY", Online: true},
	}
	var out string
	out = captureStderr(t, func() { _ = callBuildAgent(t, cfg) })
	if strings.Contains(out, "is not set") {
		t.Fatalf("a stored key was reported missing: %q", out)
	}
	if strings.Contains(out, "sk-or-stored") {
		t.Fatal("the key reached stderr")
	}
	if atomic.LoadInt32(&hits) == 0 {
		t.Fatal("the listing was never read with the stored key")
	}
	ag := callBuildAgent(t, cfg)
	if !ag.Pricing().Known {
		t.Fatalf("prices not taken from the listing: %+v", ag.Pricing())
	}
	if got := ag.KeyEnv(); got != "stored key for openrouter" {
		t.Fatalf("KeyEnv %q", got)
	}
	// The env var still wins and is still what the message names.
	t.Setenv("OPENROUTER_API_KEY", "from-env")
	if got := callBuildAgent(t, cfg).KeyEnv(); got != "OPENROUTER_API_KEY" {
		t.Fatalf("with the env var set, KeyEnv %q", got)
	}
}
