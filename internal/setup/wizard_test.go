package setup

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

func TestWizardOnlinePathWritesPresetAndHelper(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("OPENROUTER_API_KEY", "k")
	oldP, oldL := probeBackends, listOnline
	t.Cleanup(func() { probeBackends, listOnline = oldP, oldL })
	probeBackends = func(context.Context, []Candidate, time.Duration) []Found {
		return []Found{{Candidate: Candidate{Name: "ollama", Cfg: config.ProviderConfig{Type: "ollama", BaseURL: "http://localhost:11434/v1"}},
			Models: []provider.ModelInfo{{ID: "qwen3:8b"}}}}
	}
	listOnline = func(_ context.Context, p provider.Preset) ([]provider.ModelInfo, error) {
		if p.Name != "openrouter" {
			t.Errorf("preset = %s", p.Name)
		}
		return []provider.ModelInfo{{ID: "a/one"}, {ID: "b/two"}}, nil
	}
	var out strings.Builder
	// backend 2 = online; provider 1 = openrouter; model 2.
	cfg, err := Wizard(context.Background(), bufio.NewReader(strings.NewReader("2\n1\n2\n")), &out)
	if err != nil || cfg == nil {
		t.Fatalf("cfg=%v err=%v\n%s", cfg, err, out.String())
	}
	pc := cfg.Providers["openrouter"]
	if !pc.Online || pc.APIKeyEnv != "OPENROUTER_API_KEY" || cfg.DefaultProvider != "openrouter" || cfg.Model != "b/two" {
		t.Fatalf("config = %+v %q %q", pc, cfg.DefaultProvider, cfg.Model)
	}
	if cfg.LocalHelper.Provider != "ollama" || cfg.LocalHelper.Model != "qwen3:8b" {
		t.Fatalf("helper = %+v", cfg.LocalHelper)
	}
	data, err := os.ReadFile(filepath.Join(home, ".be-code", "config.json"))
	if err != nil || strings.Contains(string(data), "\"k\"") {
		t.Fatalf("saved config: %v %s", err, data)
	}
}

func TestWizardOnlineMissingKeySavesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("OPENROUTER_API_KEY", "")
	oldP, oldL := probeBackends, listOnline
	t.Cleanup(func() { probeBackends, listOnline = oldP, oldL })
	probeBackends = func(context.Context, []Candidate, time.Duration) []Found {
		return []Found{{Candidate: Candidate{Name: "ollama"}}}
	}
	listOnline = func(context.Context, provider.Preset) ([]provider.ModelInfo, error) {
		t.Error("must not list without a key")
		return nil, nil
	}
	var out strings.Builder
	cfg, err := Wizard(context.Background(), bufio.NewReader(strings.NewReader("2\n1\n")), &out)
	// Final fix 11: the one message, as an error both `be-code setup` and
	// the first launch (loadOrWizard) print.
	var mk *MissingKeyError
	if cfg != nil || !errors.As(err, &mk) || err.Error() != "set OPENROUTER_API_KEY in your shell, then run be-code setup again" {
		t.Fatalf("cfg=%v err=%v", cfg, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".be-code", "config.json")); err == nil {
		t.Fatal("config was written")
	}
}

func TestWizardOnlineWithNoLocalBackend(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("OPENROUTER_API_KEY", "k")
	oldP, oldL := probeBackends, listOnline
	t.Cleanup(func() { probeBackends, listOnline = oldP, oldL })
	probeBackends = func(context.Context, []Candidate, time.Duration) []Found { return nil }
	listOnline = func(context.Context, provider.Preset) ([]provider.ModelInfo, error) {
		return []provider.ModelInfo{{ID: "a/one"}}, nil
	}
	var out strings.Builder
	cfg, err := Wizard(context.Background(), bufio.NewReader(strings.NewReader("2\n1\n1\n")), &out)
	if err != nil || cfg == nil {
		t.Fatalf("cfg=%v err=%v\n%s", cfg, err, out.String())
	}
	if !cfg.Providers["openrouter"].Online || cfg.DefaultProvider != "openrouter" || cfg.Model != "a/one" {
		t.Fatalf("config = %+v %q %q", cfg.Providers["openrouter"], cfg.DefaultProvider, cfg.Model)
	}
	if cfg.LocalHelper != (config.LocalHelperConfig{}) {
		t.Fatalf("local_helper must stay unset: %+v", cfg.LocalHelper)
	}
}
