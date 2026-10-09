package workledger

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openFor(t *testing.T, ws string) *Ledger {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // never the real ~/.be-code
	l, err := Open(ws, Options{Enabled: true, KeepOverwritten: time.Hour, PID: 4242, Session: "ABC123", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestOpenWritesThisSessionsRecordAndCloseRemovesIt(t *testing.T) {
	ws := t.TempDir()
	l := openFor(t, ws)

	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d records, want this session's one: %+v", len(entries), entries)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	entries, _ = os.ReadDir(dir)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".seen.json") {
			t.Errorf("Close left %s behind", e.Name())
		}
	}
}

// Disabled means the behaviour before this feature: no record, no detection.
func TestDisabledWritesNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	l, err := Open(ws, Options{Enabled: false, PID: 4242})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if dir, derr := Dir(); derr == nil {
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("a disabled ledger wrote %d files", len(entries))
		}
	}
	f := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(f, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.OnRead(f, []byte("v1\n"))
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keepMine, err := l.OnConflict(context.Background(), f, nil, false, nil)
	if err != nil || !keepMine {
		t.Errorf("a disabled ledger must never block a write: %v %v", keepMine, err)
	}
}

// The notice says what the other session is doing, not merely that it exists:
// it competes with every other startup notice, and "a session exists" is not
// worth reading.
func TestNoticeLineNamesWhatTheOtherSessionIsDoing(t *testing.T) {
	ws := t.TempDir()
	l := openFor(t, ws)
	dir, _ := Dir()
	stubAlive(t, map[int]bool{4242: true, 99: true})
	other := Record{
		PID: 99, Workspace: ws, Branch: "feat/thing", Task: "003",
		Started: time.Now().UTC().Format(time.RFC3339),
		Updated: time.Now().UTC().Format(time.RFC3339),
	}
	if err := Save(dir, other); err != nil {
		t.Fatal(err)
	}

	line := l.NoticeLine()
	for _, want := range []string{"99", "feat/thing", "003"} {
		if !strings.Contains(line, want) {
			t.Errorf("the notice should mention %q: %q", want, line)
		}
	}
}

// A session alone says nothing at all: a notice on every single session would
// be noise that teaches people to skip notices.
func TestNoticeLineIsEmptyWhenAlone(t *testing.T) {
	l := openFor(t, t.TempDir())
	stubAlive(t, map[int]bool{4242: true})
	if line := l.NoticeLine(); line != "" {
		t.Errorf("a session alone said %q", line)
	}
}

// End to end through the Ledger: read, somebody else writes, our write asks.
func TestConflictEndToEnd(t *testing.T) {
	ws := gitRepo(t)
	l := openFor(t, ws)
	f := filepath.Join(ws, "doc.md")
	if err := os.WriteFile(f, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.OnRead(f, []byte("v1\n"))
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	asked := 0
	keepMine, err := l.OnConflict(context.Background(), f,
		func(string, string) bool { asked++; return true }, false, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if asked != 1 {
		t.Errorf("asked %d times, want 1", asked)
	}
	if !keepMine {
		t.Error("a yes should keep mine")
	}
}

// An unchanged file is never a conflict and never a question.
func TestNoConflictAsksNothing(t *testing.T) {
	ws := gitRepo(t)
	l := openFor(t, ws)
	f := filepath.Join(ws, "doc.md")
	if err := os.WriteFile(f, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.OnRead(f, []byte("v1\n"))

	keepMine, err := l.OnConflict(context.Background(), f,
		func(string, string) bool { t.Fatal("asked about a file nobody changed"); return false },
		false, nil)
	if err != nil || !keepMine {
		t.Errorf("an unchanged file must pass: %v %v", keepMine, err)
	}
}
