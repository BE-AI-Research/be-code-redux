package engine

import (
	"strings"
	"testing"
)

// testStore is a store on two temp dirs: a workspace root and a dotdir.
func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenAt(t.TempDir(), t.TempDir(), "s1", false,
		Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestReportIsExactAboutWhatHappened: the rollup names the files, the
// commands and their outcomes, the decisions, and what was left undone.
func TestReportIsExactAboutWhatHappened(t *testing.T) {
	s := testStore(t)
	id := s.Plan("fix the parser", []string{"find the bug", "fix and verify", "rewrite the scanner"})
	s.SetStatus(id+".1", StatusDoing, "")
	s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "lexer.go"}, Content: "     1\tpackage lex\n"})
	s.Observe(Event{Tool: "shell", Args: map[string]any{"command": "go test ./..."}, Content: "FAIL: TestLex\n", IsError: true})
	s.Note(id+".1", "the scanner eats the quote", "lexer.go", true, false)
	s.SetStatus(id+".1", StatusDone, "")
	s.SetStatus(id+".2", StatusDone, "")
	s.SetStatus(id+".3", StatusDropped, "not needed after all")
	s.SetStatus(id, StatusDone, "")

	rep := s.Report(id)
	for _, want := range []string{
		"fix the parser", "lexer.go", "go test ./...", "FAIL: TestLex",
		"the scanner eats the quote", "dropped: not needed after all",
	} {
		if !strings.Contains(rep, want) {
			t.Fatalf("report missing %q:\n%s", want, rep)
		}
	}
}

// TestReportNeedsNoModel: the rollup is deterministic — same store, same
// bytes, twice.
func TestReportNeedsNoModel(t *testing.T) {
	s := testStore(t)
	id := s.Plan("a", []string{"one"})
	s.SetStatus(id+".1", StatusDone, "")
	s.SetStatus(id, StatusDone, "")
	if s.Report(id) != s.Report(id) {
		t.Fatal("report is not deterministic")
	}
}
