package engine

import (
	"fmt"
	"strings"
	"testing"
	"time"
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

// TestReportSurfacesDroppedEvidence: a node cap that discards raw items
// (record.go's capNode) must not let the finished report read as complete —
// it says how many were dropped.
func TestReportSurfacesDroppedEvidence(t *testing.T) {
	s, err := OpenAt(t.TempDir(), t.TempDir(), "s1", false,
		Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 40})
	if err != nil {
		t.Fatal(err)
	}
	id := s.Plan("noisy task", []string{"step"})
	s.SetStatus(id+".1", StatusDoing, "")
	for i := 0; i < 5; i++ {
		s.Observe(Event{Tool: "shell", Args: map[string]any{"command": fmt.Sprintf("echo %d", i)},
			Content: strings.Repeat("x", 20)})
	}
	s.SetStatus(id+".1", StatusDone, "")
	s.SetStatus(id, StatusDone, "")
	rep := s.Report(id)
	if !strings.Contains(rep, "item(s) dropped") {
		t.Fatalf("dropped evidence not surfaced:\n%s", rep)
	}
}

func TestStatusLineOwnerSuffixes(t *testing.T) {
	n := &Node{ID: "3.2", Text: "port", Status: StatusTodo, Owner: "big", Scope: []string{"internal/scan"}}
	if got := statusLine(n); got != "3.2. port — todo @big" {
		t.Fatalf("open assigned: %q", got)
	}
	n.OwnerPinned = true
	if got := statusLine(n); got != "3.2. port — todo @big!" {
		t.Fatalf("pinned: %q", got)
	}
	n.Dispatched, n.DispatchedAt, n.Calls = true, "3.2.1", 9
	if got := statusLine(n); got != "3.2. port — todo @big running (at 3.2.1, 9 tool calls)" {
		t.Fatalf("running: %q", got)
	}
	start := time.Now().Add(-14 * time.Minute)
	d := &Node{ID: "3.2", Text: "port", Status: StatusDone, DoneBy: "big", Started: start, Closed: time.Now(), Calls: 22}
	if got := statusLine(d); got != "3.2. port — done by big (14m, 22 tool calls)" {
		t.Fatalf("done by: %q", got)
	}
	b := &Node{ID: "3.2", Text: "port", Status: StatusBlocked, DoneBy: "big", Reason: "turn cap of 40 reached"}
	if got := statusLine(b); got != "3.2. port — blocked by big: turn cap of 40 reached" {
		t.Fatalf("blocked by: %q", got)
	}
}
