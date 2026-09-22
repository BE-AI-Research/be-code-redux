package inbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidID(t *testing.T) {
	for _, ok := range []string{"alice", "Bob", "a.b-c_9", strings.Repeat("x", 32)} {
		if _, err := ValidID(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	if id, _ := ValidID("Bob"); id != "bob" {
		t.Fatalf("not lower-cased: %q", id)
	}
	for _, bad := range []string{"", "agent", "Agent", "has space", "über", strings.Repeat("x", 33), "a/b"} {
		if _, err := ValidID(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSendThreadAndThreads(t *testing.T) {
	dir := t.TempDir()
	if _, err := Send(dir, "alice", "bob", "hi bob"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := Send(dir, "bob", "alice", "hi alice"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := Send(dir, "carol", "alice", "lunch?"); err != nil {
		t.Fatal(err)
	}
	th, err := Thread(dir, "alice", "bob")
	if err != nil || len(th) != 2 || th[0].Text != "hi bob" || th[1].Text != "hi alice" || th[1].From != "bob" {
		t.Fatalf("thread: %+v %v", th, err)
	}
	// Both sides see the same thread.
	th2, _ := Thread(dir, "bob", "alice")
	if len(th2) != 2 || th2[0].Text != "hi bob" {
		t.Fatalf("bob's view: %+v", th2)
	}
	ts, err := Threads(dir, "alice")
	if err != nil || len(ts) != 2 || ts[0].With != "carol" || ts[1].With != "bob" {
		t.Fatalf("threads newest first: %+v %v", ts, err)
	}
	if ts[0].Unread != 1 || ts[1].Unread != 1 {
		t.Fatalf("unread: %+v", ts)
	}
	if ts[0].Latest.Text != "lunch?" {
		t.Fatalf("latest: %+v", ts[0].Latest)
	}
	// Reading up to carol's message clears both (bob's is older).
	if err := MarkRead(dir, "alice", ts[0].Latest.TS); err != nil {
		t.Fatal(err)
	}
	ts, _ = Threads(dir, "alice")
	if ts[0].Unread != 0 || ts[1].Unread != 0 {
		t.Fatalf("still unread after MarkRead: %+v", ts)
	}
	// Only received messages count as unread, never one's own.
	Send(dir, "alice", "bob", "one more from me")
	ts, _ = Threads(dir, "alice")
	if ts[0].With != "bob" || ts[0].Unread != 0 {
		t.Fatalf("own message counted as unread: %+v", ts)
	}
}

func TestFilesAreAtomicPrivateAndNamedByTime(t *testing.T) {
	dir := t.TempDir()
	m, err := Send(dir, "alice", "bob", "x")
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "bob"))
	if len(entries) != 1 {
		t.Fatalf("expected one file, got %d", len(entries))
	}
	name := entries[0].Name()
	if !strings.HasSuffix(name, "-alice.json") || strings.Contains(name, ".tmp") {
		t.Fatalf("name %q", name)
	}
	info, _ := entries[0].Info()
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	dinfo, _ := os.Stat(filepath.Join(dir, "bob"))
	if dinfo.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", dinfo.Mode())
	}
	if m.TS.IsZero() || m.From != "alice" || m.To != "bob" {
		t.Fatalf("message %+v", m)
	}
}

func TestSendToAnUnknownIDCreatesTheInbox(t *testing.T) {
	dir := t.TempDir()
	if _, err := Send(dir, "alice", "nobody-yet", "waiting for you"); err != nil {
		t.Fatal(err)
	}
	th, _ := Thread(dir, "nobody-yet", "alice")
	if len(th) != 1 {
		t.Fatalf("thread: %+v", th)
	}
}

func TestSendRejectsBadIDsAndEmptyText(t *testing.T) {
	dir := t.TempDir()
	if _, err := Send(dir, "alice", "agent", "hi"); err == nil {
		t.Fatal("sent to the reserved id")
	}
	if _, err := Send(dir, "alice", "bob", "   "); err == nil {
		t.Fatal("sent empty text")
	}
	if _, err := Send(dir, "a b", "bob", "hi"); err == nil {
		t.Fatal("sent from a bad id")
	}
}

func TestACorruptMessageFileIsSkipped(t *testing.T) {
	dir := t.TempDir()
	Send(dir, "alice", "bob", "good")
	os.WriteFile(filepath.Join(dir, "bob", "1-alice.json"), []byte("{not json"), 0o600)
	th, err := Thread(dir, "bob", "alice")
	if err != nil || len(th) != 1 || th[0].Text != "good" {
		t.Fatalf("thread %+v %v", th, err)
	}
}

// IDs may contain '-', and so does the file name's own separator: a filter on
// the suffix "-ob.json" matched a message from "b-ob". The sender is what
// follows the first '-' (the nanosecond prefix is all digits), exactly.
func TestHyphenatedIDsNeverShareAThread(t *testing.T) {
	dir := t.TempDir()
	Send(dir, "b-ob", "alice", "from b-ob")
	Send(dir, "ob", "alice", "from ob")
	th, _ := Thread(dir, "alice", "ob")
	if len(th) != 1 || th[0].From != "ob" {
		t.Fatalf("ob's thread: %+v", th)
	}
	th, _ = Thread(dir, "alice", "b-ob")
	if len(th) != 1 || th[0].From != "b-ob" {
		t.Fatalf("b-ob's thread: %+v", th)
	}
	ts, _ := Threads(dir, "ob")
	if len(ts) != 1 || ts[0].With != "alice" || ts[0].Latest.From != "ob" {
		t.Fatalf("ob's threads misattribute b-ob's message: %+v", ts)
	}
}
