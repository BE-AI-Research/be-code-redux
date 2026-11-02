package api

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newReg(t *testing.T) *Registry {
	t.Helper()
	return NewRegistry(filepath.Join(t.TempDir(), "apps"))
}

func TestKeyShapeAndHash(t *testing.T) {
	k := NewKey()
	if !strings.HasPrefix(k, "bek_1_") || len(strings.TrimPrefix(k, "bek_1_")) != 32 {
		t.Fatalf("key shape: %q", k)
	}
	h := HashKey(k)
	if !strings.HasPrefix(h, "sha256:") || len(h) != len("sha256:")+64 {
		t.Fatalf("hash shape: %q", h)
	}
	if HashKey(k) != h || HashKey(NewKey()) == h {
		t.Fatal("hash must be deterministic and differ per key")
	}
}

func TestValidName(t *testing.T) {
	for name, ok := range map[string]bool{"ue5-helper": true, "ab": true, "a": false, "Ue5": false, "ue5 helper": false, "-x": false, strings.Repeat("a", 33): false} {
		if ValidName(name) != ok {
			t.Errorf("ValidName(%q) = %v", name, !ok)
		}
	}
}

func TestAddStoresOnlyTheHashAndPrintsTheKeyOnce(t *testing.T) {
	r := newReg(t)
	key, err := r.Add("ue5-helper", Profile{Tiers: []string{"query"}, Sessions: "named"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(r.Dir, "ue5-helper.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), key) {
		t.Fatal("the key must never be stored")
	}
	var on map[string]any
	_ = json.Unmarshal(raw, &on)
	if on["key_hash"] != HashKey(key) {
		t.Fatalf("key_hash: %v", on["key_hash"])
	}
	st, _ := os.Stat(filepath.Join(r.Dir, "ue5-helper.json"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	if _, err := r.Add("ue5-helper", Profile{Tiers: []string{"query"}}); !errors.Is(err, ErrExists) {
		t.Fatalf("a second add must refuse with ErrExists, got %v", err)
	}
}

func TestAddFillsDefaultsAndValidates(t *testing.T) {
	r := newReg(t)
	if _, err := r.Add("ok", Profile{Tiers: []string{"query"}}); err != nil {
		t.Fatal(err)
	}
	a, _ := r.Get("ok")
	if a.Profile.Sessions != "none" || a.Profile.Budget.Tokens != DefaultTokens || a.Profile.Budget.PerHour != DefaultPerHour {
		t.Fatalf("defaults: %+v", a.Profile)
	}
	for _, bad := range []Profile{
		{Tiers: []string{"run"}},
		{Tiers: []string{"query"}, Sessions: "create"},
		{Tiers: []string{"query"}, Budget: Budget{Tokens: -1}},
	} {
		if _, err := r.Add("bad", bad); err == nil {
			t.Errorf("profile %+v must be refused", bad)
		}
	}
	if _, err := r.Add("Bad Name", Profile{Tiers: []string{"query"}}); err == nil {
		t.Error("a bad name must be refused")
	}
}

func TestLookupRotateRevokeRemove(t *testing.T) {
	r := newReg(t)
	key, _ := r.Add("app", Profile{Tiers: []string{"query"}})
	if a, ok := r.Lookup(key); !ok || a.Name != "app" {
		t.Fatal("the key must look up its app")
	}
	if _, ok := r.Lookup("bek_1_" + strings.Repeat("A", 32)); ok {
		t.Fatal("an unknown key must not look up")
	}
	if _, ok := r.Lookup("nonsense"); ok {
		t.Fatal("garbage must not look up")
	}
	key2, err := r.Rotate("app")
	if err != nil || key2 == key {
		t.Fatalf("rotate: %v", err)
	}
	if _, ok := r.Lookup(key); ok {
		t.Fatal("the old key stops at once")
	}
	if _, ok := r.Lookup(key2); !ok {
		t.Fatal("the new key works")
	}
	if err := r.Revoke("app"); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Lookup(key2); ok {
		t.Fatal("a revoked app's key must not look up")
	}
	if a, _ := r.Get("app"); a.Revoked == nil {
		t.Fatal("revoked is stamped")
	}
	if err := r.Remove("app"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Get("app"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("removed: %v", err)
	}
}

// A revoke from another process takes effect on the next Lookup without a
// restart: the registry re-reads a file whose mtime changed.
func TestLookupNoticesAChangedFile(t *testing.T) {
	r := newReg(t)
	key, _ := r.Add("app", Profile{Tiers: []string{"query"}})
	if _, ok := r.Lookup(key); !ok {
		t.Fatal("setup")
	}
	other := NewRegistry(r.Dir)
	if err := other.Revoke("app"); err != nil {
		t.Fatal(err)
	}
	// Ensure the mtime differs even on a coarse filesystem clock.
	past := time.Now().Add(-2 * time.Second)
	_ = os.Chtimes(filepath.Join(r.Dir, "app.json"), past, past)
	if _, ok := r.Lookup(key); ok {
		t.Fatal("a revoke written by another process must be seen")
	}
}

// A file that will not parse makes that app absent and is reported once.
func TestABrokenFileIsAbsentAndWarnedOnce(t *testing.T) {
	r := newReg(t)
	var warns []string
	r.Warn = func(s string) { warns = append(warns, s) }
	_ = os.MkdirAll(r.Dir, 0o700)
	_ = os.WriteFile(filepath.Join(r.Dir, "broken.json"), []byte("{not json"), 0o600)
	key, _ := r.Add("good", Profile{Tiers: []string{"query"}})
	for i := 0; i < 3; i++ {
		if _, ok := r.Lookup(key); !ok {
			t.Fatal("the good app still looks up")
		}
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "broken.json") {
		t.Fatalf("one warning naming the file: %v", warns)
	}
}
