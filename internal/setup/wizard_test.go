package setup

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/credentials"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/signin"
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

// With no key in the environment the wizard now asks for one; input that
// ends at that question saves nothing and is not mistaken for a choice
// (an EOF must never start a browser sign-in).
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
	ov, os_ := onlineValidate, wizardSignIn
	t.Cleanup(func() { onlineValidate, wizardSignIn = ov, os_ })
	onlineValidate = func(context.Context, Choice, string) ([]provider.ModelInfo, error) {
		t.Error("must not validate without a key")
		return nil, nil
	}
	wizardSignIn = func(context.Context, signin.Options) (string, error) {
		t.Error("an EOF must not start a sign-in")
		return "", signin.ErrCancelled
	}
	var out strings.Builder
	cfg, err := Wizard(context.Background(), bufio.NewReader(strings.NewReader("2\n1\n")), &out)
	if cfg != nil || err == nil || !strings.Contains(err.Error(), "nothing was saved") {
		t.Fatalf("cfg=%v err=%v", cfg, err)
	}
	if _, err := os.Stat(filepath.Join(home, ".be-code", "config.json")); err == nil {
		t.Fatal("config was written")
	}
	if _, err := os.Stat(filepath.Join(home, ".be-code", credentials.FileName)); err == nil {
		t.Fatal("credentials were written")
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

// The wizard's online path with a pasted key: a rejected key goes back to
// the key step, the good one is validated, stored (not echoed, not in
// config.json) and the provider made the default.
func TestWizardOnlineAcceptsAPastedKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("GROQ_API_KEY", "")
	oldP, oldL := probeBackends, listOnline
	t.Cleanup(func() { probeBackends, listOnline = oldP, oldL })
	probeBackends = func(context.Context, []Candidate, time.Duration) []Found { return nil }
	listOnline = func(context.Context, provider.Preset) ([]provider.ModelInfo, error) {
		return nil, errors.New("listOnline must not be used on the pasted-key path")
	}
	ov := onlineValidate
	onlineValidate = func(ctx context.Context, ch Choice, key string) ([]provider.ModelInfo, error) {
		if ch.Name != "groq" || !ch.Preset {
			t.Errorf("choice = %+v", ch)
		}
		if key != "good" {
			return nil, ErrKeyRejected
		}
		return []provider.ModelInfo{{ID: "m1"}, {ID: "m2"}}, nil
	}
	t.Cleanup(func() { onlineValidate = ov })
	// [2] online (no backend found); provider 5 = groq (after the four
	// quick sign-in rows); a rejected key, then the good one; model 2.
	in := bufio.NewReader(strings.NewReader("2\n5\nbad\ngood\n2\n"))
	var out bytes.Buffer
	cfg, err := Wizard(context.Background(), in, &out)
	if err != nil || cfg == nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "good") || strings.Contains(out.String(), "bad") {
		t.Fatalf("the key was echoed:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "(the key is not echoed on a terminal)") || !strings.Contains(out.String(), "key rejected") {
		t.Fatalf("output:\n%s", out.String())
	}
	if cfg.DefaultProvider != "groq" || cfg.Model != "m2" || cfg.Providers["groq"].DefaultModel != "m2" || cfg.ProviderKey("groq") != "good" {
		t.Fatalf("%+v key=%q", cfg, cfg.ProviderKey("groq"))
	}
	if src, c := cfg.KeySource("groq"); src != "stored" || c.Source != "pasted" {
		t.Fatalf("source = %q %+v", src, c.Source)
	}
	data, err := os.ReadFile(filepath.Join(home, ".be-code", "config.json"))
	if err != nil || strings.Contains(string(data), "good") {
		t.Fatalf("saved config: %v %s", err, data)
	}
	reloaded, err := config.Load()
	if err != nil || reloaded.DefaultProvider != "groq" || reloaded.Model != "m2" {
		t.Fatalf("reloaded: %v %+v", err, reloaded)
	}
}

func TestWizardAnthropicOpensItsKeysPageBeforeThePastedKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("ANTHROPIC_API_KEY", "")
	oldP, oldL, oldO := probeBackends, listOnline, wizardOpenPage
	t.Cleanup(func() { probeBackends, listOnline, wizardOpenPage = oldP, oldL, oldO })
	probeBackends = func(context.Context, []Candidate, time.Duration) []Found { return nil }
	listOnline = func(context.Context, provider.Preset) ([]provider.ModelInfo, error) { return nil, errors.New("unused") }
	opened := ""
	wizardOpenPage = func(u string) bool { opened = u; return false }
	ov := onlineValidate
	onlineValidate = func(ctx context.Context, ch Choice, key string) ([]provider.ModelInfo, error) {
		return []provider.ModelInfo{{ID: "m1"}}, nil
	}
	t.Cleanup(func() { onlineValidate = ov })
	// [2] online; provider 3 = anthropic; the key; model 1.
	in := bufio.NewReader(strings.NewReader("2\n3\ngood\n1\n"))
	var out bytes.Buffer
	cfg, err := Wizard(context.Background(), in, &out)
	if err != nil || cfg == nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if opened != "https://console.anthropic.com/settings/keys" {
		t.Fatalf("opened %q", opened)
	}
	if !strings.Contains(out.String(), "create a key at https://console.anthropic.com/settings/keys and paste it below") {
		t.Fatalf("output:\n%s", out.String())
	}
	if cfg.DefaultProvider != "anthropic" || cfg.ProviderKey("anthropic") != "good" {
		t.Fatalf("%+v", cfg)
	}
}
