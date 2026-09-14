package config

import (
	"os"
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
