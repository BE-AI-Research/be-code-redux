package engine

import (
	"strings"
	"testing"
)

const doc = `# 001 — fix the parser

- [x] 1. fix the parser
  - [x] 1.1. find the bug
    - files: lexer.go (lines 1–120)
    - cmds: go test ./... — failed
    - error: FAIL: TestLex
  - [>] 1.2. fix and verify
  - [-] 1.3. rewrite the scanner — dropped: not needed after all
`

// TestRoundTrip: parse a document and render it back byte-identical, so
// the engine never churns a file it did not mean to change.
func TestRoundTrip(t *testing.T) {
	tr, _, err := ParseDoc(doc)
	if err != nil {
		t.Fatal(err)
	}
	out := RenderDoc("001", "fix the parser", tr.Roots[0])
	if out != doc {
		t.Fatalf("round trip differs:\n--- got ---\n%s\n--- want ---\n%s", out, doc)
	}
}

// TestStatusMarks: every status has exactly one mark, and a hand-written
// mark is read back as that status.
func TestStatusMarks(t *testing.T) {
	tr, _, err := ParseDoc(doc)
	if err != nil {
		t.Fatal(err)
	}
	get := func(id string) *Node { return tr.Find(id) }
	if get("1").Status != StatusDone || get("1.2").Status != StatusDoing {
		t.Fatalf("statuses: %s %s", get("1").Status, get("1.2").Status)
	}
	if n := get("1.3"); n.Status != StatusDropped || n.Reason != "not needed after all" {
		t.Fatalf("dropped node: %s %q", n.Status, n.Reason)
	}
}

// TestUnknownLinesArePreserved: a human's own prose survives a round trip,
// because the engine is a guest in this file.
func TestUnknownLinesArePreserved(t *testing.T) {
	edited := doc + "\n> note to self: the scanner is the real culprit\n"
	tr, extra, err := ParseDoc(edited)
	if err != nil {
		t.Fatal(err)
	}
	out := RenderDocWithExtra("001", "fix the parser", tr.Roots[0], extra)
	if !strings.Contains(out, "note to self: the scanner is the real culprit") {
		t.Fatal("hand-written line was lost")
	}
}

// TestHandEditedStatusWins: the user marked a step done in their editor;
// the engine must accept that, not overwrite it.
func TestHandEditedStatusWins(t *testing.T) {
	tr, _, err := ParseDoc(strings.Replace(doc, "- [>] 1.2.", "- [x] 1.2.", 1))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Find("1.2").Status != StatusDone {
		t.Fatal("hand edit ignored")
	}
}

// TestMalformedDocumentIsAnError: a document we cannot understand is an
// error the caller quarantines, never something we half-parse and rewrite.
func TestMalformedDocumentIsAnError(t *testing.T) {
	if _, _, err := ParseDoc("# 001 — x\n\n- [?] banana\n  - [x] 1.1. orphan child\n"); err == nil {
		t.Fatal("expected an error")
	}
}

// TestAHandWrittenChecklistIsNotATask: ruling T3-c. Only a top-level line
// carrying an id, or the first root of a document with none, is a task;
// a checklist someone keeps in the file is their text, preserved.
func TestAHandWrittenChecklistIsNotATask(t *testing.T) {
	tr, extra, err := ParseDoc(doc + "\n- [ ] buy milk\n  - [ ] and bread\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Roots) != 1 {
		t.Fatalf("a checklist became a task: %d roots", len(tr.Roots))
	}
	out := RenderDocWithExtra("001", "fix the parser", tr.Roots[0], extra)
	if !strings.Contains(out, "- [ ] buy milk") || !strings.Contains(out, "- [ ] and bread") {
		t.Fatalf("the checklist was lost:\n%s", out)
	}
	// A top-level line that does carry an id is a task.
	tr2, _, err := ParseDoc("# 001 — x\n\n- [ ] 1. alpha\n- [ ] 2. beta\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(tr2.Roots) != 2 || tr2.Roots[1].ID != "2" {
		t.Fatalf("an id-carrying top-level line must be a task: %+v", tr2.Roots)
	}
}

// TestEvidenceUnderAChecklistStaysInTheChecklist: an evidence key written
// under a declined top-level bullet belongs to that list, not to the task
// above it. Adopting it would invent a fact and move the line out of the
// user's own list on the next write.
func TestEvidenceUnderAChecklistStaysInTheChecklist(t *testing.T) {
	tr, extra, err := ParseDoc(doc + "\n- [ ] buy milk\n  - note: the corner shop shuts at six\n")
	if err != nil {
		t.Fatal(err)
	}
	stolen := false
	tr.Walk(func(n *Node, _ int) {
		for _, nt := range n.Evidence.Notes {
			if strings.Contains(nt.Text, "corner shop") {
				stolen = true
			}
		}
	})
	if stolen {
		t.Fatal("a line from the user's checklist was adopted as a task's evidence")
	}
	out := RenderDocWithExtra("001", "fix the parser", tr.Roots[0], extra)
	if !strings.Contains(out, "- [ ] buy milk\n  - note: the corner shop shuts at six") {
		t.Fatalf("the line was relocated out of the checklist:\n%s", out)
	}
}
