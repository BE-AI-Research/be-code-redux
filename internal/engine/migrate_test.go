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
