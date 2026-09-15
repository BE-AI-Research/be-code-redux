package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTest(t *testing.T, session string, resumed bool) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(t.TempDir(), "engine")
	s, err := OpenAt(dir, root, session, resumed, 4096)
	if err != nil {
		t.Fatal(err)
	}
	return s, root
}

func TestKeyIsStableAndShort(t *testing.T) {
	a, b := Key("/tmp/x/../y"), Key("/tmp/y")
	if a != b || len(a) != 16 {
		t.Fatalf("key %q vs %q", a, b)
	}
}

func TestLedgerAndNotesRoundTrip(t *testing.T) {
	s, root := openTest(t, "s1", false)
	s.SetPlan("add a flag", []string{"parse", "wire", "test"})
	if err := s.SetStep(2, "doing"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStep(4, "done"); err == nil || err.Error() != "step 4 does not exist (3 steps)" {
		t.Fatalf("bad step error: %v", err)
	}
	if err := s.AddNote("config lives in internal/config", "", false, true); err != nil {
		t.Fatal(err)
	}
	if err := s.AddNote("", "", false, false); err == nil || err.Error() != "note needs text" {
		t.Fatalf("empty note: %v", err)
	}
	s.AddNote("use cobra", "", true, false)
	s.SetBaseline(Baseline{Head: "abc", Dirty: "d1"})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	again, err := OpenAt(s.Dir(), root, "s1", true, 4096)
	if err != nil {
		t.Fatal(err)
	}
	l := again.Ledger()
	if l.Task != "add a flag" || len(l.Steps) != 3 || l.Steps[1].Status != "doing" || l.Steps[0].Status != "todo" {
		t.Fatalf("ledger: %+v", l)
	}
	if len(l.Decisions) != 1 || l.Decisions[0] != "use cobra" || len(l.Facts) != 1 || l.Baseline.Head != "abc" {
		t.Fatalf("ledger: %+v", l)
	}
	if !strings.Contains(again.Notes(), "config lives in internal/config") {
		t.Fatalf("notes: %q", again.Notes())
	}
}

func TestSessionScopeClearsOnNewSessionButKeepsNotes(t *testing.T) {
	s, root := openTest(t, "s1", false)
	s.SetPlan("t", []string{"a"})
	s.AddNoteLine("durable fact")
	s.Flush()
	fresh, err := OpenAt(s.Dir(), root, "s2", false, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Ledger().Task != "" || len(fresh.Digests()) != 0 {
		t.Fatal("session files survived a new session")
	}
	if fresh.Notes() != "durable fact\n" {
		t.Fatalf("notes lost: %q", fresh.Notes())
	}
	// A resume of the same session keeps everything.
	s.SetPlan("t2", nil)
	s.Flush()
	kept, _ := OpenAt(s.Dir(), root, "s1", true, 4096)
	if kept.Ledger().Task != "t2" {
		t.Fatal("resume lost the ledger")
	}
}

func TestNotesCapTrimsOldestLines(t *testing.T) {
	s, _ := openTest(t, "s1", false)
	s.notesCap = 40
	s.AddNoteLine("first line that is long enough")
	s.AddNoteLine("second line that is long enough")
	n := s.Notes()
	if strings.Contains(n, "first") || !strings.Contains(n, "second") || len(n) > 40 {
		t.Fatalf("cap: %q", n)
	}
	if err := s.DropNote(1); err != nil || s.Notes() != "" {
		t.Fatalf("drop: %v %q", err, s.Notes())
	}
	if err := s.DropNote(1); err == nil {
		t.Fatal("drop past end must error")
	}
}

func TestCorruptFileIsRenamedAndStartedFresh(t *testing.T) {
	s, root := openTest(t, "s1", false)
	s.SetPlan("t", nil)
	s.Flush()
	os.WriteFile(filepath.Join(s.Dir(), "ledger.json"), []byte("{not json"), 0o600)
	again, err := OpenAt(s.Dir(), root, "s1", true, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if again.Ledger().Task != "" {
		t.Fatal("corrupt ledger was not reset")
	}
	entries, _ := os.ReadDir(s.Dir())
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "ledger.json.broken-") {
			found = true
		}
	}
	if !found {
		t.Fatal("corrupt file not preserved")
	}
}

func TestDigestEvictionKeepsMostRecentlyTouched(t *testing.T) {
	s, _ := openTest(t, "s1", false)
	for i := 0; i < maxDigests+5; i++ {
		s.putDigest(&Digest{Path: fmt.Sprintf("f%d.go", i), Turn: i})
	}
	if len(s.Digests()) != maxDigests {
		t.Fatalf("digests = %d", len(s.Digests()))
	}
	if s.Digests()[0].Turn != maxDigests+4 {
		t.Fatalf("newest first expected, got turn %d", s.Digests()[0].Turn)
	}
}

func TestTurnRestoredFromLookupsToo(t *testing.T) {
	s, root := openTest(t, "s1", false)
	s.lookups = append(s.lookups, Lookup{Tool: "search", Query: "q", Turn: 50})
	s.dirty = true
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	again, err := OpenAt(s.Dir(), root, "s1", true, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if again.Turn() != 50 {
		t.Fatalf("turn = %d, want 50", again.Turn())
	}
}

func TestAddNoteLineDedupsWholeLinesOnly(t *testing.T) {
	s, _ := openTest(t, "s1", false)
	s.AddNoteLine("increase timeout for tests")
	s.AddNoteLine("timeout for tests")
	n := s.Notes()
	lines := strings.Split(strings.TrimRight(n, "\n"), "\n")
	if len(lines) != 2 || lines[0] != "increase timeout for tests" || lines[1] != "timeout for tests" {
		t.Fatalf("notes: %q", n)
	}
}
