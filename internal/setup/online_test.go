package setup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/credentials"
)

// listingServer answers /models: 401 unless the bearer is "good"; with
// html set, 200 with an HTML body; with refuse set, 404.
func listingServer(t *testing.T, html, refuse bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(401)
			return
		}
		if refuse {
			w.WriteHeader(404)
			return
		}
		if html {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>login</html>"))
			return
		}
		w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
	}))
}

func customChoice(t *testing.T, url string) Choice {
	t.Helper()
	ch, _, err := ChoiceFor(config.Default(), "mine", url+"/v1")
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

func TestValidateOutcomes(t *testing.T) {
	srv := listingServer(t, false, false)
	defer srv.Close()
	ch := customChoice(t, srv.URL)
	if _, err := Validate(context.Background(), ch, "bad"); !errors.Is(err, ErrKeyRejected) {
		t.Fatalf("bad key: %v", err)
	}
	models, err := Validate(context.Background(), ch, "good")
	if err != nil || len(models) != 2 {
		t.Fatalf("good key: %v %v", models, err)
	}
	down := customChoice(t, "http://127.0.0.1:1")
	if _, err := Validate(context.Background(), down, "good"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("down: %v", err)
	}
}

func TestValidateHTMLIsUnreachable(t *testing.T) {
	srv := listingServer(t, true, false)
	defer srv.Close()
	_, err := Validate(context.Background(), customChoice(t, srv.URL), "good")
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("html body: %v", err)
	}
}

func TestValidateRefusedListingIsEmptyNotError(t *testing.T) {
	srv := listingServer(t, false, true)
	defer srv.Close()
	models, err := Validate(context.Background(), customChoice(t, srv.URL), "good")
	if err != nil || len(models) != 0 {
		t.Fatalf("404 listing: %v %v", models, err)
	}
}

func TestValidateTrimsKey(t *testing.T) {
	srv := listingServer(t, false, false)
	defer srv.Close()
	if _, err := Validate(context.Background(), customChoice(t, srv.URL), "  good\n"); err != nil {
		t.Fatalf("trimmed key: %v", err)
	}
}

func TestChoiceForRules(t *testing.T) {
	cfg := config.Default()
	cfg.Providers["openrouter"] = config.ProviderConfig{Type: "openai", BaseURL: "https://elsewhere/v1", APIKeyEnv: "OPENROUTER_API_KEY"}
	ch, note, err := ChoiceFor(cfg, "openrouter", "")
	if err != nil || !ch.Preset || ch.BaseURL != "https://openrouter.ai/api/v1" || !strings.Contains(note, "openrouter is a preset; its URL is https://openrouter.ai/api/v1") {
		t.Fatalf("preset: %+v note=%q err=%v", ch, note, err)
	}
	if _, _, err := ChoiceFor(cfg, "groq", "https://custom/v1"); err == nil {
		t.Fatal("a custom entry named like a preset must be refused")
	}
	if _, _, err := ChoiceFor(cfg, "mine", ""); err == nil {
		t.Fatal("a custom entry needs a URL")
	}
	ch, _, err = ChoiceFor(cfg, "mine", "http://10.0.0.5:8000/v1")
	if err != nil || ch.Preset || ch.KeyEnv != "" {
		t.Fatalf("custom: %+v %v", ch, err)
	}
}

func TestAddOnlineWritesCredentialThenConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.Default()
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	store, err := credentials.Open(filepath.Join(home, ".be-code"))
	if err != nil {
		t.Fatal(err)
	}
	ch, _, _ := ChoiceFor(cfg, "groq", "")
	t.Setenv("GROQ_API_KEY", "")
	added, err := AddOnline(cfg, store, ch, "gsk_1\n", "pasted", "llama", false)
	if err != nil || added.Model != "llama" || added.Default || added.Note != "" {
		t.Fatalf("%+v %v", added, err)
	}
	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	pc := reloaded.Providers["groq"]
	if pc.BaseURL != "https://api.groq.com/openai/v1" || pc.APIKeyEnv != "GROQ_API_KEY" || !pc.Online || pc.DefaultModel != "llama" || pc.Type != "openai" {
		t.Fatalf("block: %+v", pc)
	}
	if reloaded.DefaultProvider == "groq" {
		t.Fatal("default_provider changed without being asked")
	}
	b, _ := os.ReadFile(filepath.Join(home, ".be-code", "config.json"))
	if strings.Contains(string(b), "gsk_1") {
		t.Fatal("config.json holds the key")
	}
	if got := reloaded.ProviderKey("groq"); got != "gsk_1" {
		t.Fatalf("stored key %q (must be trimmed)", got)
	}
	// Replace the key, make it the default.
	added, err = AddOnline(cfg, store, ch, "gsk_2", "oauth", "llama2", true)
	if err != nil || !added.Default {
		t.Fatalf("%+v %v", added, err)
	}
	reloaded, _ = config.Load()
	if reloaded.DefaultProvider != "groq" || reloaded.ProviderKey("groq") != "gsk_2" || reloaded.Providers["groq"].DefaultModel != "llama2" {
		t.Fatalf("replace: %+v key=%q", reloaded.Providers["groq"], reloaded.ProviderKey("groq"))
	}
	c, _ := store.Key("groq")
	if c.Source != "oauth" {
		t.Fatalf("source %q", c.Source)
	}
}

func TestAddOnlineNotesEnvPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.Default()
	store, _ := credentials.Open(filepath.Join(home, ".be-code"))
	ch, _, _ := ChoiceFor(cfg, "groq", "")
	t.Setenv("GROQ_API_KEY", "from-env")
	added, err := AddOnline(cfg, store, ch, "stored", "pasted", "m", false)
	if err != nil || added.Note != "note: $GROQ_API_KEY is set and takes precedence" {
		t.Fatalf("%+v %v", added, err)
	}
}

func TestAddOnlineCustomOnlineByEndpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.Default()
	store, _ := credentials.Open(filepath.Join(home, ".be-code"))
	local, _, _ := ChoiceFor(cfg, "lan", "http://192.168.1.50:8000/v1")
	if _, err := AddOnline(cfg, store, local, "k", "pasted", "m", false); err != nil {
		t.Fatal(err)
	}
	remote, _, _ := ChoiceFor(cfg, "far", "https://api.together.xyz/v1")
	if _, err := AddOnline(cfg, store, remote, "k", "pasted", "m", false); err != nil {
		t.Fatal(err)
	}
	re, _ := config.Load()
	if re.Providers["lan"].Online || !re.Providers["far"].Online || re.Providers["lan"].APIKeyEnv != "" {
		t.Fatalf("lan=%+v far=%+v", re.Providers["lan"], re.Providers["far"])
	}
}

// I2 (final review): a custom name may not take over an existing provider
// of another type — the default local ollama block above all.
func TestChoiceForRefusesAnExistingNonOpenAIProvider(t *testing.T) {
	cfg := config.Default()
	cfg.Providers["ollama"] = config.ProviderConfig{Type: "ollama", BaseURL: "http://localhost:11434"}
	_, _, err := ChoiceFor(cfg, "ollama", "https://api.together.xyz/v1")
	if err == nil || err.Error() != "ollama is an existing ollama provider; a custom endpoint needs another name" {
		t.Fatalf("err %v", err)
	}
	// An existing custom (openai-typed, or untyped) entry is still reusable.
	cfg.Providers["mine"] = config.ProviderConfig{Type: "openai", BaseURL: "http://10.0.0.5:8000/v1"}
	cfg.Providers["bare"] = config.ProviderConfig{BaseURL: "http://10.0.0.6:8000/v1"}
	for _, n := range []string{"mine", "bare"} {
		if _, _, err := ChoiceFor(cfg, n, "http://10.0.0.7:8000/v1"); err != nil {
			t.Fatalf("%s: %v", n, err)
		}
	}
}

// I4 (+R4, final review): re-adding a custom endpoint keeps its consent
// fields — the api_key_env a person named and a hand-set online: true on a
// LAN endpoint.
func TestAddOnlineKeepsACustomBlocksConsentFields(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.Default()
	cfg.Providers["lan"] = config.ProviderConfig{Type: "openai", BaseURL: "http://192.168.1.50:8000/v1", APIKeyEnv: "LAN_KEY", Online: true}
	store, _ := credentials.Open(filepath.Join(home, ".be-code"))
	t.Setenv("LAN_KEY", "")
	ch, _, err := ChoiceFor(cfg, "lan", "http://192.168.1.50:8000/v1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AddOnline(cfg, store, ch, "k", "pasted", "m", false); err != nil {
		t.Fatal(err)
	}
	re, _ := config.Load()
	pc := re.Providers["lan"]
	if pc.APIKeyEnv != "LAN_KEY" {
		t.Fatalf("api_key_env dropped: %+v", pc)
	}
	if !pc.Online {
		t.Fatalf("a hand-set online: true was recomputed away: %+v", pc)
	}
	// A new LAN entry still follows the endpoint.
	fresh, _, _ := ChoiceFor(cfg, "lan2", "http://192.168.1.51:8000/v1")
	if _, err := AddOnline(cfg, store, fresh, "k", "pasted", "m", false); err != nil {
		t.Fatal(err)
	}
	if re, _ := config.Load(); re.Providers["lan2"].Online {
		t.Fatal("a new LAN endpoint must not be marked online")
	}
}
