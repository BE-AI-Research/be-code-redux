package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMigrationLiftsTheFlatLedger: a 0.10.0 store becomes one task with its
func TestMigrationLiftsTheFlatLedger(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	old := `{"task":"fix the parser","steps":[{"text":"find the bug","status":"done"},
	  {"text":"fix and verify","status":"doing"}],"decisions":["quotes handled in the scanner"],
	  "facts":["lexer.go is 900 lines"],"session":"s0"}`
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "ledger.json"), []byte(old), 0o600)

	s, err := OpenAt(dir, root, "s0", true, Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768})
	if err != nil {
		t.Fatal(err)
	}
	tr := s.Tree()
	if len(tr.Roots) != 1 || tr.Roots[0].Text != "fix the parser" {
		t.Fatalf("roots: %+v", tr.Roots)
	}
	if len(tr.Roots[0].Children) != 2 || tr.Roots[0].Children[0].Status != StatusDone {
		t.Fatalf("children: %+v", tr.Roots[0].Children)
	}
	notes := tr.Roots[0].Evidence.Notes
	if len(notes) != 2 || !notes[0].Decision {
		t.Fatalf("notes: %+v", notes)
	}
	if _, err := os.Stat(filepath.Join(dir, "ledger.json")); !os.IsNotExist(err) {
		t.Fatal("the migrated ledger should be removed so migration runs once")
	}
}

// TestMigrationSurvivesAnInterruptedFirstRun: the lifted tree must be on
// disk before the 0.10.0 files are removed. A normal session does not flush
// until a request completes, so a Ctrl-C at the prompt used to take the old
// store with it.
func TestMigrationSurvivesAnInterruptedFirstRun(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ledger.json"),
		[]byte(`{"task":"lift me","steps":[{"text":"one","status":"done"}],"session":"s0"}`), 0o600)

	if _, err := OpenAt(dir, root, "s0", true, testLimits()); err != nil {
		t.Fatal(err)
	}
	// Interrupted here: nothing else ever called Flush.
	again, err := OpenAt(dir, root, "s1", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	tr := again.Tree()
	if len(tr.Roots) != 1 || tr.Roots[0].Text != "lift me" {
		t.Fatalf("the migrated task did not survive the interruption: %+v", tr.Roots)
	}
	if len(tr.Roots[0].Children) != 1 || tr.Roots[0].Children[0].Status != StatusDone {
		t.Fatalf("children: %+v", tr.Roots[0].Children)
	}
}

// TestMigrationCarriesDigestsAndLookups: digests and lookups become the
// task's evidence, all three legacy files go, and the hash and outline a
// document cannot carry come back from state.json (ruling T3-b).
func TestMigrationCarriesDigestsAndLookups(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	writeFile(t, root, "lexer.go", "package lex\nfunc Scan() {}\n")
	os.WriteFile(filepath.Join(dir, "ledger.json"), []byte(`{"task":"fix","session":"s0"}`), 0o600)
	os.WriteFile(filepath.Join(dir, "digests.json"), []byte(
		`[{"path":"lexer.go","hash":"abc","outline":["Scan"],"ranges":[{"from":1,"to":2}],"turn":7,"edited":true,"note":"the lexer"}]`), 0o600)
	os.WriteFile(filepath.Join(dir, "lookups.json"), []byte(
		`[{"tool":"search","query":"search pattern=\"Scan\";","hits":[{"file":"lexer.go","line":2,"text":"func Scan"}],"turn":5}]`), 0o600)

	s, err := OpenAt(dir, root, "s0", true, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	r := s.Tree().Roots[0]
	if len(r.Evidence.Files) != 1 || r.Evidence.Files[0].Path != "lexer.go" || !r.Evidence.Files[0].Edited {
		t.Fatalf("digests were not lifted: %+v", r.Evidence.Files)
	}
	if r.Evidence.Files[0].Note != "the lexer" {
		t.Fatalf("file note lost: %+v", r.Evidence.Files[0])
	}
	if len(r.Evidence.Lookups) != 1 || len(r.Evidence.Lookups[0].Hits) != 1 {
		t.Fatalf("lookups were not lifted: %+v", r.Evidence.Lookups)
	}
	if s.Turn() != 7 {
		t.Fatalf("turn = %d, want the highest of the lifted turns", s.Turn())
	}
	for _, name := range []string{"ledger.json", "digests.json", "lookups.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s survived the migration", name)
		}
	}

	again, err := OpenAt(dir, root, "s1", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	f := again.Tree().Roots[0].Evidence.Files[0]
	if f.Hash != "abc" || len(f.Outline) != 1 || f.Turn != 7 {
		t.Fatalf("the file's hash, outline and turn did not survive the reopen: %+v", f)
	}
}
