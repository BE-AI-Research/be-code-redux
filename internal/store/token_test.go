package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewTokenHasThePrefixAndLength(t *testing.T) {
	tok := NewToken()
	if !strings.HasPrefix(tok, "sess_1_") {
		t.Fatalf("prefix: %q", tok)
	}
	// 16 bytes base64url unpadded is 22 characters.
	if n := len(strings.TrimPrefix(tok, "sess_1_")); n != 22 {
		t.Fatalf("want 22 encoded chars, got %d (%q)", n, tok)
	}
	if NewToken() == tok {
		t.Fatal("two tokens must differ")
	}
}

func TestAccessTokenMintsOnceAndAccepts(t *testing.T) {
	s := &Session{ID: "20260101-000000.000"}
	first := s.AccessToken()
	if first == "" || s.AccessToken() != first {
		t.Fatal("AccessToken must mint once and then return the same token")
	}
	if !s.Accepts(first) {
		t.Fatal("the minted token must be accepted")
	}
	if s.Accepts("sess_1_"+strings.Repeat("A", 22)) || s.Accepts("") {
		t.Fatal("another token must not be accepted")
	}
}

func TestRotateWithGraceHonoursTheOldTokenUntilExpiry(t *testing.T) {
	s := &Session{}
	old := s.AccessToken()
	if err := s.Rotate(time.Hour); err != nil {
		t.Fatal(err)
	}
	if s.Token == old || s.PrevToken != old || s.TokenRotatedAt.IsZero() {
		t.Fatalf("rotate must mint a new token and keep the old in grace: %+v", s)
	}
	if !s.Accepts(old) || !s.Accepts(s.Token) {
		t.Fatal("both tokens are accepted during grace")
	}
	s.PrevUntil = time.Now().Add(-time.Second)
	if s.Accepts(old) {
		t.Fatal("an expired grace token must be refused")
	}
	if s.PrevToken != "" {
		t.Fatal("an expired grace token is cleared on the way")
	}
}

func TestRotateWithZeroGraceCutsAtOnce(t *testing.T) {
	s := &Session{}
	old := s.AccessToken()
	if err := s.Rotate(0); err != nil {
		t.Fatal(err)
	}
	if s.Accepts(old) || s.PrevToken != "" || !s.PrevUntil.IsZero() {
		t.Fatalf("zero grace clears the old token: %+v", s)
	}
}

func TestRotateRefusesAGraceOverADay(t *testing.T) {
	s := &Session{}
	s.AccessToken()
	if err := s.Rotate(25 * time.Hour); err == nil || !strings.Contains(err.Error(), "24h") {
		t.Fatalf("want a refusal naming 24h, got %v", err)
	}
}

// A session file written before tokens existed loads, and the first access
// mints a token that the next save persists.
func TestLegacySessionFileGainsATokenOnSave(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows
	if err := os.MkdirAll(filepath.Join(home, ".be-code", "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	id := "20260101-000000.000"
	legacy := `{"id":"` + id + `","title":"old","messages":[]}`
	if err := os.WriteFile(filepath.Join(home, ".be-code", "sessions", id+".json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if s.Token != "" {
		t.Fatal("a legacy file has no token until one is asked for")
	}
	tok := s.AccessToken()
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(home, ".be-code", "sessions", id+".json"))
	var on map[string]any
	_ = json.Unmarshal(raw, &on)
	if on["token"] != tok {
		t.Fatalf("the token must be persisted: %s", raw)
	}
	if _, has := on["prev_token"]; has {
		t.Fatal("empty token fields are omitted")
	}
}
