package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/api"
)

// captureAppsStdout swaps os.Stdout (the commands print with fmt.Print*) and
// returns what f wrote. Named apart from doctor_test.go's captureStdout.
func captureAppsStdout(t *testing.T, f func() error) (string, error) {
	t.Helper()
	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	err = f()
	w.Close()
	os.Stdout = old
	var b bytes.Buffer
	_, _ = b.ReadFrom(r)
	return b.String(), err
}

// appsTestHome points HOME at a scratch dir so ~/.be-code/api/apps is never
// touched, and resets the apps flags a previous test may have set.
func appsTestHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	resetAppsFlags(t)
	t.Cleanup(func() { resetAppsFlags(t) })
	return home
}

func resetAppsFlags(t *testing.T) {
	t.Helper()
	defaults := map[string]string{
		"profile":    "",
		"tiers":      "query",
		"models":     "",
		"packs":      "",
		"sessions":   "none",
		"per-hour":   "120",
		"max-tokens": "4096",
	}
	for name, v := range defaults {
		if err := appsAddCmd.Flags().Set(name, v); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAppsAddPrintsTheKeyOnceAndRefusesTwice(t *testing.T) {
	home := appsTestHome(t)
	_ = appsAddCmd.Flags().Set("tiers", "query,completions")
	_ = appsAddCmd.Flags().Set("sessions", "named")
	_ = appsAddCmd.Flags().Set("per-hour", "30")
	out, err := captureAppsStdout(t, func() error { return appsAddCmd.RunE(appsAddCmd, []string{"ue5-helper"}) })
	if err != nil {
		t.Fatal(err)
	}
	var key string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "bek_1_") {
			key = strings.TrimSpace(line)
		}
	}
	if key == "" || !strings.Contains(out, "query, completions") || !strings.Contains(out, "sessions: named") || !strings.Contains(out, "30 per hour") {
		t.Fatalf("the key and the stored profile are printed:\n%s", out)
	}
	reg := api.NewRegistry(filepath.Join(home, ".be-code", "api", "apps"))
	if a, ok := reg.Lookup(key); !ok || a.Profile.Budget.PerHour != 30 || a.Profile.Budget.Tokens != api.DefaultTokens {
		t.Fatalf("the registry has the app with the flags and defaults: %+v %v", a, ok)
	}
	out2, err := captureAppsStdout(t, func() error { return appsAddCmd.RunE(appsAddCmd, []string{"ue5-helper"}) })
	if err == nil || !strings.Contains(err.Error(), "already registered") || strings.Contains(out2, "bek_1_") {
		t.Fatalf("a second add refuses and prints no key: err=%v out=%q", err, out2)
	}
}

func TestAppsAddFromProfileFile(t *testing.T) {
	home := appsTestHome(t)
	pf := filepath.Join(home, "p.json")
	if err := os.WriteFile(pf, []byte(`{"tiers":["query"],"models":["default","big"],"packs":["ue5"],"sessions":"named","budget":{"tokens":2048,"per_hour":10}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = appsAddCmd.Flags().Set("profile", pf)
	if _, err := captureAppsStdout(t, func() error { return appsAddCmd.RunE(appsAddCmd, []string{"filed"}) }); err != nil {
		t.Fatal(err)
	}
	reg := api.NewRegistry(filepath.Join(home, ".be-code", "api", "apps"))
	a, err := reg.Get("filed")
	if err != nil || len(a.Profile.Models) != 2 || a.Profile.Budget.Tokens != 2048 || a.Profile.Packs[0] != "ue5" {
		t.Fatalf("%v %+v", err, a.Profile)
	}
}

func TestAppsListShowRotateRevokeRemove(t *testing.T) {
	appsTestHome(t)
	_ = appsAddCmd.Flags().Set("tiers", "query")
	if _, err := captureAppsStdout(t, func() error { return appsAddCmd.RunE(appsAddCmd, []string{"one"}) }); err != nil {
		t.Fatal(err)
	}
	out, _ := captureAppsStdout(t, func() error { return appsListCmd.RunE(appsListCmd, nil) })
	if !strings.Contains(out, "one") || !strings.Contains(out, "query") {
		t.Fatalf("list:\n%s", out)
	}
	out, _ = captureAppsStdout(t, func() error { return appsShowCmd.RunE(appsShowCmd, []string{"one"}) })
	if strings.Contains(out, "sha256:") {
		t.Fatalf("show never prints the hash:\n%s", out)
	}
	out, err := captureAppsStdout(t, func() error { return appsRotateCmd.RunE(appsRotateCmd, []string{"one"}) })
	if err != nil || !strings.Contains(out, "bek_1_") {
		t.Fatalf("rotate prints the new key once: %v\n%s", err, out)
	}
	if _, err := captureAppsStdout(t, func() error { return appsRevokeCmd.RunE(appsRevokeCmd, []string{"one"}) }); err != nil {
		t.Fatal(err)
	}
	out, _ = captureAppsStdout(t, func() error { return appsListCmd.RunE(appsListCmd, nil) })
	if !strings.Contains(out, "revoked") {
		t.Fatalf("list shows revoked:\n%s", out)
	}
	if _, err := captureAppsStdout(t, func() error { return appsRemoveCmd.RunE(appsRemoveCmd, []string{"one"}) }); err != nil {
		t.Fatal(err)
	}
	if _, err := captureAppsStdout(t, func() error { return appsShowCmd.RunE(appsShowCmd, []string{"one"}) }); err == nil {
		t.Fatal("show after remove is an error")
	}
}

// keyFromAppsOutput returns the bek_1_ key line a command printed, or "".
func keyFromAppsOutput(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if l := strings.TrimSpace(line); strings.HasPrefix(l, "bek_1_") {
			return l
		}
	}
	return ""
}

func TestAppsAddStoresOnlyTheHash(t *testing.T) {
	home := appsTestHome(t)
	out, err := captureAppsStdout(t, func() error { return appsAddCmd.RunE(appsAddCmd, []string{"hashonly"}) })
	if err != nil {
		t.Fatal(err)
	}
	key := keyFromAppsOutput(out)
	if key == "" {
		t.Fatalf("add prints a key:\n%s", out)
	}
	raw, err := os.ReadFile(filepath.Join(home, ".be-code", "api", "apps", "hashonly.json"))
	if err != nil {
		t.Fatal(err)
	}
	payload := strings.TrimPrefix(key, "bek_1_")
	if strings.Contains(string(raw), key) || strings.Contains(string(raw), payload) {
		t.Fatalf("the app file holds the key or its payload:\n%s", raw)
	}
	if !strings.Contains(string(raw), "sha256:") {
		t.Fatalf("the app file holds the key hash:\n%s", raw)
	}
}

func TestAppsRotateReplacesTheKey(t *testing.T) {
	home := appsTestHome(t)
	out, err := captureAppsStdout(t, func() error { return appsAddCmd.RunE(appsAddCmd, []string{"spinner"}) })
	if err != nil {
		t.Fatal(err)
	}
	oldKey := keyFromAppsOutput(out)
	out, err = captureAppsStdout(t, func() error { return appsRotateCmd.RunE(appsRotateCmd, []string{"spinner"}) })
	if err != nil {
		t.Fatal(err)
	}
	newKey := keyFromAppsOutput(out)
	if newKey == "" || newKey == oldKey {
		t.Fatalf("rotate prints a different key: old=%q new=%q", oldKey, newKey)
	}
	reg := api.NewRegistry(filepath.Join(home, ".be-code", "api", "apps"))
	if _, ok := reg.Lookup(oldKey); ok {
		t.Fatal("the old key still works after rotate")
	}
	if _, ok := reg.Lookup(newKey); !ok {
		t.Fatal("the new key does not work after rotate")
	}
}

func TestAppsRevokeThenRotateReactivates(t *testing.T) {
	appsTestHome(t)
	if _, err := captureAppsStdout(t, func() error { return appsAddCmd.RunE(appsAddCmd, []string{"bounce"}) }); err != nil {
		t.Fatal(err)
	}
	out, err := captureAppsStdout(t, func() error { return appsRevokeCmd.RunE(appsRevokeCmd, []string{"bounce"}) })
	if err != nil || !strings.Contains(out, "the key no longer works") || !strings.Contains(out, "re-activates the app") {
		t.Fatalf("revoke says what rotate does: %v\n%s", err, out)
	}
	out, err = captureAppsStdout(t, func() error { return appsRotateCmd.RunE(appsRotateCmd, []string{"bounce"}) })
	if err != nil || !strings.Contains(out, "bounce is active again") || !strings.Contains(out, "bek_1_") {
		t.Fatalf("rotate of a revoked app says it is active again: %v\n%s", err, out)
	}
	out, _ = captureAppsStdout(t, func() error { return appsListCmd.RunE(appsListCmd, nil) })
	if !strings.Contains(out, "active") || strings.Contains(out, "revoked") {
		t.Fatalf("list shows the app active after rotate:\n%s", out)
	}
}

func TestAppsHelpDoesNotPromiseUnbounded(t *testing.T) {
	for _, name := range []string{"per-hour", "max-tokens"} {
		usage := appsAddCmd.Flags().Lookup(name).Usage
		if strings.Contains(usage, "unbounded") {
			t.Fatalf("%s help promises unbounded, which apps add cannot express: %q", name, usage)
		}
	}
}
