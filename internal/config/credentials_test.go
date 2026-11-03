package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/credentials"
)

func storeKey(t *testing.T, home, provider, key string) {
	t.Helper()
	s, err := credentials.Open(filepath.Join(home, ".be-code"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(provider, credentials.Credential{Kind: credentials.KindAPIKey, Key: key, Source: "pasted", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func TestProviderKeyPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := Default()
	cfg.Providers["groq"] = ProviderConfig{Type: "openai", BaseURL: "https://api.groq.com/openai/v1", APIKeyEnv: "BE_TEST_GROQ_KEY", Online: true}
	t.Setenv("BE_TEST_GROQ_KEY", "")
	if got := cfg.ProviderKey("groq"); got != "" {
		t.Fatalf("nothing set: %q", got)
	}
	storeKey(t, home, "groq", "stored-key")
	if got := cfg.ProviderKey("groq"); got != "stored-key" {
		t.Fatalf("stored: %q", got)
	}
	if src, _ := cfg.KeySource("groq"); src != "stored" {
		t.Fatalf("source %q", src)
	}
	t.Setenv("BE_TEST_GROQ_KEY", "env-key")
	if got := cfg.ProviderKey("groq"); got != "env-key" {
		t.Fatalf("env must win: %q", got)
	}
	if src, _ := cfg.KeySource("groq"); src != "env" {
		t.Fatalf("source %q", src)
	}
	if got := cfg.ProviderKey("nope"); got != "" {
		t.Fatalf("unknown provider: %q", got)
	}
}

func TestProviderKeyLooseModeIsEmptyNotFatal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".be-code")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"version":1,"providers":{"groq":{"kind":"api_key","key":"k","created":"2026-11-02T00:00:00Z"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	cfg.Providers["groq"] = ProviderConfig{Type: "openai", BaseURL: "https://x/v1", APIKeyEnv: "BE_TEST_GROQ_KEY"}
	t.Setenv("BE_TEST_GROQ_KEY", "")
	if got := cfg.ProviderKey("groq"); got != "" {
		t.Fatalf("a loose file must yield nothing: %q", got)
	}
}
