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
