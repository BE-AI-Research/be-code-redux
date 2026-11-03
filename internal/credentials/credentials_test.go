// internal/credentials/credentials_test.go
package credentials

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRoundTripAnd0600(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Key("groq"); ok {
		t.Fatal("empty store has a key")
	}
	c := Credential{Kind: KindAPIKey, Key: "gsk_test", Source: "pasted", Created: time.Date(2026, 11, 2, 20, 0, 0, 0, time.UTC)}
	if err := s.Put("groq", c); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o, want 0600", fi.Mode().Perm())
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Key("groq")
	if !ok || got.Kind != c.Kind || got.Source != c.Source || !got.Created.Equal(c.Created) || got.Key != c.Key {
		t.Fatalf("round trip mismatch: ok=%v Kind=%q Source=%q Created=%v", ok, got.Kind, got.Source, got.Created)
	}
}

func TestPutCreatesDotdir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dot")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("groq", Credential{Kind: KindAPIKey, Key: "k", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dotdir: %v mode %v", err, fi.Mode())
	}
}

func TestLooseModeRefusedBothWays(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(p, []byte(`{"version":1,"providers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	if !errors.Is(err, ErrLooseMode) || !strings.Contains(err.Error(), "chmod 600 "+p) {
		t.Fatalf("open: %v", err)
	}
}

func TestCorruptReadsEmptyWriteRefused(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(p, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Key("x"); ok {
		t.Fatal("corrupt file yielded a key")
	}
	err = s.Put("x", Credential{Kind: KindAPIKey, Key: "k", Created: time.Now()})
	if err == nil || !strings.Contains(err.Error(), p) {
		t.Fatalf("put on corrupt file: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != `{not json` {
		t.Fatal("corrupt file was overwritten")
	}
}

func TestUnknownKindKeptAndReported(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "credentials.json")
	raw := `{"version":1,"providers":{"future":{"kind":"oauth_token","access":"a","refresh":"r"},"groq":{"kind":"api_key","key":"k","created":"2026-11-02T20:00:00Z"}}}`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Unsupported(); len(got) != 1 || got[0] != "future" {
		t.Fatalf("unsupported: %v", got)
	}
	if _, ok := s.Key("future"); ok {
		t.Fatal("an unknown kind must not yield a key")
	}
	if err := s.Revoke("groq"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	provs := m["providers"].(map[string]any)
	if _, ok := provs["groq"]; ok {
		t.Fatal("groq not revoked")
	}
	f := provs["future"].(map[string]any)
	if f["refresh"] != "r" {
		t.Fatalf("unknown kind not kept verbatim: %v", f)
	}
}

func TestRevokeMissingIsNoop(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke("nobody"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Fatal("revoke of a missing entry created the file")
	}
}

func TestLooseModeRefusedOnWriteAndRevoke(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not enforce modes on credentials.json")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "credentials.json")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("groq", Credential{Kind: KindAPIKey, Key: "k", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(p)
	if err := s.Put("other", Credential{Kind: KindAPIKey, Key: "k2", Created: time.Now()}); !errors.Is(err, ErrLooseMode) {
		t.Fatalf("put on loose file: %v", err)
	}
	if err := s.Revoke("groq"); !errors.Is(err, ErrLooseMode) {
		t.Fatalf("revoke on loose file: %v", err)
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("loose file was rewritten")
	}
}

func TestRevokeCorruptReportsParseError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(p, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Revoke("groq")
	if err == nil || !strings.Contains(err.Error(), "does not parse") || !strings.Contains(err.Error(), p) {
		t.Fatalf("revoke on corrupt file: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != `{not json` {
		t.Fatal("corrupt file was overwritten")
	}
}
