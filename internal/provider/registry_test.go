package provider

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/credentials"
)

func TestFromConfigUsesStoredKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, err := credentials.Open(filepath.Join(home, ".be-code"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("groq", credentials.Credential{Kind: credentials.KindAPIKey, Key: "stored-key", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Providers["groq"] = config.ProviderConfig{Type: "openai", BaseURL: "https://x/v1", APIKeyEnv: "BE_TEST_GROQ_KEY"}
	t.Setenv("BE_TEST_GROQ_KEY", "")
	p, err := FromConfig(cfg, "groq")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.(*OpenAICompat).APIKey; got != "stored-key" {
		t.Fatalf("provider key %q", got)
	}
}
