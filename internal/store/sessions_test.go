package store

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/provider"
)

func TestSaveLoadListResume(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir()) // windows

	s := NewSession("ollama", "qwen3:8b", "/tmp/ws")
	s.Title = TitleFrom("fix the flaky websocket reconnect test in internal/client")
	s.Messages = []provider.Message{
		{Role: provider.RoleUser, Content: "fix the flaky test"},
		{Role: provider.RoleAssistant, Content: "done"},
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Title != s.Title || len(loaded.Messages) != 2 || loaded.Model != "qwen3:8b" {
		t.Fatalf("round trip mismatch: %+v", loaded)
	}

	// "last" resolves to the newest session.
	s2 := NewSession("ollama", "qwen3:8b", "/tmp/ws")
	s2.ID = s.ID + "z" // ensure distinct
	s2.Title = "newer"
	s2.Messages = []provider.Message{{Role: provider.RoleUser, Content: "hi"}}
	if err := s2.Save(); err != nil {
		t.Fatal(err)
	}
	last, err := Load("last")
	if err != nil {
		t.Fatal(err)
	}
	if last.Title != "newer" {
		t.Fatalf("last = %q", last.Title)
	}

	metas, err := List()
	if err != nil || len(metas) != 2 {
		t.Fatalf("list: %v %d", err, len(metas))
	}
	if metas[0].Title != "newer" {
		t.Fatal("list not newest-first")
	}
	if metas[1].Turns != 1 {
		t.Fatalf("turn count = %d", metas[1].Turns)
	}

	if err := Delete(s.ID); err != nil {
		t.Fatal(err)
	}
	if metas, _ = List(); len(metas) != 1 {
		t.Fatal("delete failed")
	}
}

func TestTitleFrom(t *testing.T) {
	long := TitleFrom("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa bbb")
	if len(long) > 64 {
		t.Fatalf("title too long: %d", len(long))
	}
	if TitleFrom("  spaced   out  ") != "spaced out" {
		t.Fatal("whitespace not normalized")
	}
}

// Every session gets a short resume code, shown in listings and accepted by
// Load, so users can resume without copying a timestamp ID.
func TestSessionResumeCode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())

	s := NewSession("ollama", "m", "/ws")
	if len(s.Code) != 6 {
		t.Fatalf("code = %q, want 6 chars", s.Code)
	}
	s.Messages = []provider.Message{{Role: provider.RoleUser, Content: "hi"}}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load(strings.ToLower(s.Code))
	if err != nil || got.ID != s.ID {
		t.Fatalf("Load by code: %v %+v", err, got)
	}
	metas, _ := List()
	if len(metas) != 1 || metas[0].Code != s.Code {
		t.Fatalf("listing lacks code: %+v", metas)
	}
}

// Sessions saved by older versions have no code; they get a stable derived
// one so they can still be resumed by code.
func TestLegacySessionGetsDerivedCode(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	s := NewSession("ollama", "m", "/ws")
	s.Code = ""
	s.Messages = []provider.Message{{Role: provider.RoleUser, Content: "hi"}}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	metas, _ := List()
	if len(metas) != 1 || metas[0].Code == "" {
		t.Fatalf("legacy session has no derived code: %+v", metas)
	}
	if _, err := Load(metas[0].Code); err != nil {
		t.Fatalf("Load by derived code: %v", err)
	}
}

// The handoff note written on exit must survive save/load so the next
// session can be brought up to speed.
func TestSessionHandoffRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	s := NewSession("ollama", "m", "/ws")
	s.Handoff = "Task: build widget. Requirement: no CGO. Changed: a.go"
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load(s.ID)
	if err != nil || got.Handoff != s.Handoff {
		t.Fatalf("handoff lost: %v %q", err, got.Handoff)
	}
}

// The chat room round-trips with the session, and a session that never used
// chat serialises with no "chat" key at all (older sessions, and every
// session that never opened the room, stay exactly as small as before).
func TestChatRoundTripsAndIsAbsentWhenEmpty(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	s := &Session{ID: "20260921-000000-001", Title: "t", Chat: []ChatLine{{TS: time.Unix(1, 0), User: "alice", Text: "hi", Kind: ""}}}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load(s.ID)
	if err != nil || len(got.Chat) != 1 || got.Chat[0].User != "alice" {
		t.Fatalf("%+v %v", got, err)
	}
	b, _ := json.Marshal(&Session{ID: "x"})
	if strings.Contains(string(b), `"chat"`) {
		t.Fatalf("empty chat serialised: %s", b)
	}
}

// A hosted program stamps its pid on the session file so a second program
// can tell that the file already has a live owner (see the save guard in
// internal/agent). The stamp is worthless unless it survives a round trip.
func TestSessionHostPIDRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	s := NewSession("ollama", "m", "/ws")
	s.HostPID = 4242
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	got, err := Load(s.ID)
	if err != nil || got.HostPID != 4242 {
		t.Fatalf("host pid lost: %v %d", err, got.HostPID)
	}
	// Sessions written before the stamp existed load as unowned, not as
	// owned-by-pid-zero.
	s.HostPID = 0
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if got, err = Load(s.ID); err != nil || got.HostPID != 0 {
		t.Fatalf("unstamped session: %v %d", err, got.HostPID)
	}
}
