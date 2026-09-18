package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIDEDefaults(t *testing.T) {
	c := Default()
	if !c.IDE.Enabled || !c.IDE.AutoContext {
		t.Fatalf("ide defaults = %+v", c.IDE)
	}
}

func TestStallNoticeSecondsDefault(t *testing.T) {
	if got := Default().StallNoticeSeconds; got != 45 {
		t.Fatalf("stall_notice_seconds default = %d, want 45", got)
	}
}

// TestClientThemesRoundTrip saves a config with a per-device theme and
// reloads it, confirming client_themes survives the JSON round trip.
func TestClientThemesRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	cfg := Default()
	if cfg.ClientThemes == nil {
		t.Fatal("Default().ClientThemes is nil")
	}
	cfg.ClientThemes["desk"] = "nord"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.ClientThemes["desk"]; got != "nord" {
		t.Fatalf("client_themes[desk] = %q after round trip", got)
	}
}

// TestClientThemesNilSafeWithoutKey loads an on-disk config predating
// client_themes and confirms the field comes back as a non-nil map, so a
// write to it never panics.
func TestClientThemesNilSafeWithoutKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	p, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"theme":"dark"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClientThemes == nil {
		t.Fatal("ClientThemes is nil after loading a config file without the key")
	}
	cfg.ClientThemes["x"] = "y" // must not panic
}

func TestCoworkDefaults(t *testing.T) {
	c := Default()
	if !c.Cowork.Auto || c.Cowork.MaxConsultsPerRun != 3 || c.Cowork.ConsultTurns != 12 || c.Cowork.ConsultTimeout != 300 {
		t.Fatalf("cowork defaults = %+v", c.Cowork)
	}
	if len(c.Coworkers) != 0 {
		t.Fatalf("default coworkers = %+v", c.Coworkers)
	}
}

func TestValidCoworkersDropsBrokenEntries(t *testing.T) {
	c := Default()
	c.Coworkers = []CoworkerConfig{
		{Name: "claude", Provider: "anthropic", Model: "claude-opus-5", Online: true}, // provider not configured
		{Name: "big", Provider: "ollama", Model: "qwen3:32b", Skills: "long reads"},
		{Name: "", Provider: "ollama", Model: "x"},
		{Name: "big", Provider: "ollama", Model: "dup"},
		{Name: "nomodel", Provider: "ollama"},
	}
	ok, warns := c.ValidCoworkers()
	if len(ok) != 1 || ok[0].Name != "big" || ok[0].Model != "qwen3:32b" {
		t.Fatalf("valid = %+v", ok)
	}
	want := []string{
		`coworker "claude": provider "anthropic" is not configured`,
		`coworker "": name is empty`,
		`coworker "big": duplicate name`,
		`coworker "nomodel": model is empty`,
	}
	if len(warns) != len(want) {
		t.Fatalf("warnings = %q", warns)
	}
	for i := range want {
		if warns[i] != want[i] {
			t.Errorf("warning %d = %q, want %q", i, warns[i], want[i])
		}
	}
}

func TestCoworkLoadFillsMissingValues(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	p, _ := Path()
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte(`{"theme":"dark","cowork":{"max_consults_per_run":5}}`), 0o600)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.Cowork.Auto || c.Cowork.MaxConsultsPerRun != 5 || c.Cowork.ConsultTurns != 12 || c.Cowork.ConsultTimeout != 300 {
		t.Fatalf("loaded cowork = %+v", c.Cowork)
	}
	os.WriteFile(p, []byte(`{"cowork":{"auto":false}}`), 0o600)
	c, _ = Load()
	if c.Cowork.Auto {
		t.Fatal("explicit auto:false was overridden")
	}
	// A file with no cowork block at all leaves Default()'s auto alone:
	// encoding/json never zeroes a field the document does not mention,
	// which is why Load needs no probe for it.
	os.WriteFile(p, []byte(`{"theme":"dark"}`), 0o600)
	c, _ = Load()
	if !c.Cowork.Auto || c.Cowork.ConsultTimeout != 300 {
		t.Fatalf("a file without the cowork block = %+v", c.Cowork)
	}
}

// AutoApproveConsult is -y's alone: it must never be settable from the file
// (and never written into one), or "always run shell commands" would ship
// code to an online co-worker.
func TestAutoApproveConsultIsNotAConfigKey(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	p, _ := Path()
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte(`{"auto_approve_shell":true,"AutoApproveConsult":true,"auto_approve_consult":true}`), 0o600)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.AutoApproveShell {
		t.Fatal("auto_approve_shell did not load")
	}
	if c.AutoApproveConsult {
		t.Fatal("the config file set AutoApproveConsult")
	}
	c.AutoApproveConsult = true
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	for _, key := range []string{"AutoApproveConsult", "auto_approve_consult"} {
		if strings.Contains(string(b), key) {
			t.Fatalf("AutoApproveConsult was persisted as %q:\n%s", key, b)
		}
	}
}

func TestEngineDefaultsAndLoadFill(t *testing.T) {
	d := Default()
	if !d.Engine.Enabled || d.Engine.Budget != 6144 || d.Engine.NotesCap != 4096 || d.Engine.Tools != "full" {
		t.Fatalf("defaults: %+v", d.Engine)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".be-code"), 0o755)
	os.WriteFile(filepath.Join(home, ".be-code", "config.json"), []byte(`{"engine":{"enabled":false,"budget":0,"tools":""}}`), 0o600)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Engine.Enabled || cfg.Engine.Budget != 6144 || cfg.Engine.NotesCap != 4096 || cfg.Engine.Tools != "full" {
		t.Fatalf("load fill: %+v", cfg.Engine)
	}
}

func TestEngineCapsDefaultAndLoadFill(t *testing.T) {
	d := Default()
	if d.Engine.ItemCap != 4096 || d.Engine.NodeCap != 32768 {
		t.Fatalf("defaults: %+v", d.Engine)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".be-code"), 0o755)
	os.WriteFile(filepath.Join(home, ".be-code", "config.json"),
		[]byte(`{"engine":{"item_cap":0,"node_cap":0}}`), 0o600)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Engine.ItemCap != 4096 || cfg.Engine.NodeCap != 32768 {
		t.Fatalf("load fill: %+v", cfg.Engine)
	}
}

// TestReloadOnMismatchDefaultsToAsk: the gate that protects a shared server
// must never come up missing. An older config file has no such key, and the
// absent value has to mean "ask", not "do it".
func TestReloadOnMismatchDefaultsToAsk(t *testing.T) {
	if got := Default().ReloadOnMismatch; got != "ask" {
		t.Fatalf("default %q", got)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".be-code"), 0o755)
	os.WriteFile(filepath.Join(home, ".be-code", "config.json"),
		[]byte(`{"model":"m","reload_on_mismatch":""}`), 0o600)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReloadOnMismatch != "ask" {
		t.Fatalf("load fill: %q", cfg.ReloadOnMismatch)
	}
}

// TestModelAndProviderParametersRoundTrip: the shapes the loader reads must
// survive a save/load cycle, keys and all.
func TestModelAndProviderParametersRoundTrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := Default()
	cfg.Providers["lan"] = ProviderConfig{
		Type: "ollama", BaseURL: "http://192.168.1.150:11434",
		ContextWindow: 32768, KeepAlive: "30m",
		Options: map[string]any{"temperature": 0.6},
	}
	cfg.Models = map[string]ModelConfig{
		"qwen3:8b": {ContextWindow: 16384, KeepAlive: "10m", Options: map[string]any{"top_k": float64(40)}},
	}
	cfg.ReloadOnMismatch = "always"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	pc := got.Providers["lan"]
	if pc.ContextWindow != 32768 || pc.KeepAlive != "30m" || pc.Options["temperature"] != 0.6 {
		t.Fatalf("provider block: %+v", pc)
	}
	mc := got.Models["qwen3:8b"]
	if mc.ContextWindow != 16384 || mc.KeepAlive != "10m" || mc.Options["top_k"] != float64(40) {
		t.Fatalf("model block: %+v", mc)
	}
	if got.ReloadOnMismatch != "always" {
		t.Fatalf("reload_on_mismatch: %q", got.ReloadOnMismatch)
	}
	b, err := os.ReadFile(filepath.Join(home, ".be-code", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"context_window", "keep_alive", "options", "models", "reload_on_mismatch"} {
		if !strings.Contains(string(b), key) {
			t.Fatalf("%q missing from the written config:\n%s", key, b)
		}
	}
}

// TestContextTokensAbsentMeansDerive: a literal default here silently capped
// every window larger than it — a configured 32768-token window ran on a
// 16384-token budget with nothing printed. Absent must mean "derive from the
// model's window", which is the 0 the agent already understands.
func TestContextTokensAbsentMeansDerive(t *testing.T) {
	if got := Default().ContextTokens; got != 0 {
		t.Fatalf("Default().ContextTokens = %d; a guessed budget caps real windows", got)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".be-code"), 0o755)
	os.WriteFile(filepath.Join(home, ".be-code", "config.json"), []byte(`{"model":"m"}`), 0o600)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ContextTokens != 0 {
		t.Fatalf("absent key loaded as %d", cfg.ContextTokens)
	}
}

// TestContextTokensSetStillWins: every config file written before this
// change carries a literal, and those sessions must keep behaving.
func TestContextTokensSetStillWins(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".be-code"), 0o755)
	os.WriteFile(filepath.Join(home, ".be-code", "config.json"), []byte(`{"context_tokens":16384}`), 0o600)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ContextTokens != 16384 {
		t.Fatalf("explicit key loaded as %d", cfg.ContextTokens)
	}
}

// TestContextTokensZeroIsNotACap: a file that says 0 (this version's own
// Save used to write one) means derive, not "a budget of nothing".
func TestContextTokensZeroIsNotACap(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".be-code"), 0o755)
	os.WriteFile(filepath.Join(home, ".be-code", "config.json"), []byte(`{"context_tokens":0}`), 0o600)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ContextTokens != 0 {
		t.Fatalf("context_tokens = %d", cfg.ContextTokens)
	}
	// And a derived budget is not persisted back as a cap.
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(home, ".be-code", "config.json"))
	if strings.Contains(string(b), "context_tokens") {
		t.Fatalf("a derived budget was written back as an explicit key:\n%s", b)
	}
}
