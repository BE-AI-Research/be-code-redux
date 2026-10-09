package engine

import (
	"strings"
	"testing"
)

// The engine already hashes every document it loads and writes so an outside
// edit is recognisable (this package's own comment). The work ledger needs
// the same hash, and must get the engine's rather than computing its own —
// two hashes of one file that could disagree is worse than one.
//
// The fixture goes through the real write path: plan a task (which writes a
// document), flush, then reopen the same workspace so loadDocs reads it.
func TestLoadDocsReportsItsHashes(t *testing.T) {
	ws, dot := t.TempDir(), t.TempDir()
	first, err := OpenAt(ws, dot, "s1", false, Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768})
	if err != nil {
		t.Fatal(err)
	}
	first.Plan("fix the parser", []string{"find the bug"})
	if err := first.Flush(); err != nil {
		t.Fatal(err)
	}

	seen := map[string]string{}
	saved := OnDocHash
	OnDocHash = func(path, hash string) { seen[path] = hash }
	t.Cleanup(func() { OnDocHash = saved })

	if _, err := OpenAt(ws, dot, "s2", true, Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768}); err != nil {
		t.Fatal(err)
	}
	if len(seen) == 0 {
		t.Fatal("loadDocs reported no hashes; the work ledger would treat every document as unseen")
	}
	for path, hash := range seen {
		if hash == "" {
			t.Errorf("%s was reported with an empty hash", path)
		}
		if !strings.Contains(path, ".be-code") || !strings.HasSuffix(path, ".md") {
			t.Errorf("the path should be the document's own: %q", path)
		}
	}
}

// A nil hook is the behaviour before this feature.
func TestLoadDocsWithNoHookIsFine(t *testing.T) {
	saved := OnDocHash
	OnDocHash = nil
	t.Cleanup(func() { OnDocHash = saved })
	s := testStore(t)
	s.Plan("a task", []string{"a step"})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
}
