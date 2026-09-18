package engine

import (
	"strings"
	"testing"
)

func newRec(item, node int) *recorder { return &recorder{lim: Limits{ItemCap: item, NodeCap: node}} }

// TestRawIsKeptVerbatimWhileDoing: the doing node holds exactly what the
// tool returned, because that is the lossless half of the design.
func TestRawIsKeptVerbatimWhileDoing(t *testing.T) {
	r := newRec(4096, 32768)
	n := &Node{ID: "1", Status: StatusDoing}
	r.record(n, Event{Tool: "shell", Args: map[string]any{"command": "go test ./..."},
		Content: "--- FAIL: TestLex (0.00s)\n    lex_test.go:42: want 3 got 4\nFAIL\n"}, 1)
	if len(n.Evidence.Raw) != 1 {
		t.Fatalf("raw: %+v", n.Evidence.Raw)
	}
	if !strings.Contains(n.Evidence.Raw[0].Out, "lex_test.go:42: want 3 got 4") {
		t.Fatalf("not verbatim: %q", n.Evidence.Raw[0].Out)
	}
}

// TestPerItemAndPerNodeCaps: one huge read cannot swallow the buffer, and
// when a cap bites the node says so rather than pretending to be complete.
func TestPerItemAndPerNodeCaps(t *testing.T) {
	r := newRec(64, 200)
	n := &Node{ID: "1", Status: StatusDoing}
	r.record(n, Event{Tool: "read_file", Args: map[string]any{"path": "big.txt"},
		Content: strings.Repeat("x", 5000)}, 1)
	if len(n.Evidence.Raw[0].Out) > 64+len("\n… (truncated)") {
		t.Fatalf("item cap not applied: %d bytes", len(n.Evidence.Raw[0].Out))
	}
	for i := 0; i < 20; i++ {
		r.record(n, Event{Tool: "shell", Args: map[string]any{"command": "echo hi"},
			Content: strings.Repeat("y", 60)}, i+2)
	}
	var total int
	for _, it := range n.Evidence.Raw {
		total += len(it.Out)
	}
	if total > 200 {
		t.Fatalf("node cap not applied: %d bytes in %d items", total, len(n.Evidence.Raw))
	}
	if n.Evidence.Dropped == 0 {
		t.Fatal("dropped count not recorded; the record must not claim to be complete")
	}
}

// TestDistillKeepsTheFactsAndDropsTheBulk: closing a node turns raw output
// into the distilled record, derived from the buffer and not the transcript.
func TestDistillKeepsTheFactsAndDropsTheBulk(t *testing.T) {
	r := newRec(4096, 32768)
	n := &Node{ID: "1", Status: StatusDoing}
	r.record(n, Event{Tool: "read_file", Args: map[string]any{"path": "lexer.go"},
		Content: "     1\tpackage lex\n     2\tfunc Scan() {}\n"}, 1)
	r.record(n, Event{Tool: "shell", Args: map[string]any{"command": "go build ./..."},
		Content: "ok\n"}, 2)
	r.record(n, Event{Tool: "shell", Args: map[string]any{"command": "go test ./..."},
		Content: "FAIL: TestLex\nmore\n", IsError: true}, 3)
	r.distill(n)
	if len(n.Evidence.Raw) != 0 {
		t.Fatalf("raw survived distillation: %+v", n.Evidence.Raw)
	}
	if len(n.Evidence.Files) != 1 || n.Evidence.Files[0].Path != "lexer.go" {
		t.Fatalf("files: %+v", n.Evidence.Files)
	}
	if len(n.Evidence.Cmds) != 2 || n.Evidence.Cmds[0].Cmd != "go build ./..." || !n.Evidence.Cmds[0].OK {
		t.Fatalf("cmds: %+v", n.Evidence.Cmds)
	}
	if n.Evidence.Cmds[1].OK {
		t.Fatal("a failing command must be recorded as failing")
	}
	if len(n.Evidence.Errors) != 1 || n.Evidence.Errors[0] != "go test ./...: FAIL: TestLex" {
		t.Fatalf("errors: %+v", n.Evidence.Errors) // "<args>: <first line>"
	}
}

// TestFailedWriteLeavesFileRefUntouched: a failed write/edit told the model
// nothing about the file's real content, so it must not be merged in —
// neither marking the file edited nor stretching its ranges over the error
// text.
func TestFailedWriteLeavesFileRefUntouched(t *testing.T) {
	r := newRec(4096, 32768)
	n := &Node{ID: "1", Status: StatusDoing}
	r.record(n, Event{Tool: "read_file", Args: map[string]any{"path": "a.go"},
		Content: "     1\tpackage a\n     2\tvar X = 1\n"}, 1)
	r.record(n, Event{Tool: "write_file", Args: map[string]any{"path": "a.go"},
		Content: "permission denied", IsError: true}, 2)
	r.distill(n)
	if len(n.Evidence.Files) != 1 {
		t.Fatalf("files: %+v", n.Evidence.Files)
	}
	f := n.Evidence.Files[0]
	if f.Edited {
		t.Fatal("a failed write must not mark the file edited")
	}
	if len(f.Ranges) != 1 || f.Ranges[0] != (Range{From: 1, To: 2}) {
		t.Fatalf("ranges corrupted by the failed write: %+v", f.Ranges)
	}
	if len(n.Evidence.Errors) != 1 || n.Evidence.Errors[0] != "a.go: permission denied" {
		t.Fatalf("errors: %+v", n.Evidence.Errors)
	}
}

// TestFailedCallWithNoOutputStillRecordsAnError: a failing tool call with
// empty output must not vanish from Errors — that would be total loss of
// the failure for the file tools, which carry no per-file OK flag.
func TestFailedCallWithNoOutputStillRecordsAnError(t *testing.T) {
	r := newRec(4096, 32768)
	n := &Node{ID: "1", Status: StatusDoing}
	r.record(n, Event{Tool: "write_file", Args: map[string]any{"path": "a.go"},
		Content: "", IsError: true}, 1)
	r.distill(n)
	if len(n.Evidence.Errors) != 1 || n.Evidence.Errors[0] != "a.go: (no output)" {
		t.Fatalf("errors: %+v", n.Evidence.Errors)
	}
}

// TestRedundantReadFooterSurvivesTheRewrite: the 0.10.0 behaviour the model
// depends on — a second read of an unchanged range is answered with a
// footer, and the content is still returned in full.
func TestRedundantReadFooterSurvivesTheRewrite(t *testing.T) {
	r := newRec(4096, 32768)
	n := &Node{ID: "1", Status: StatusDoing}
	ev := Event{Tool: "read_file", Args: map[string]any{"path": "a.go"},
		Content: "     1\tpackage a\n     2\tvar X = 1\n"}
	if f := r.record(n, ev, 1); f != "" {
		t.Fatalf("first read returned a footer: %q", f)
	}
	f := r.record(n, ev, 2)
	if !strings.Contains(f, "already read at turn 1") {
		t.Fatalf("footer: %q", f)
	}
}

// TestEvidenceFollowsTheDoingNode: two nodes, two sets of evidence, no
// leakage — this is what makes a report exact.
func TestEvidenceFollowsTheDoingNode(t *testing.T) {
	r := newRec(4096, 32768)
	a := &Node{ID: "1", Status: StatusDoing}
	b := &Node{ID: "2", Status: StatusDoing}
	r.record(a, Event{Tool: "shell", Args: map[string]any{"command": "one"}, Content: "ok"}, 1)
	r.record(b, Event{Tool: "shell", Args: map[string]any{"command": "two"}, Content: "ok"}, 2)
	r.distill(a)
	r.distill(b)
	if len(a.Evidence.Cmds) != 1 || a.Evidence.Cmds[0].Cmd != "one" {
		t.Fatalf("a: %+v", a.Evidence.Cmds)
	}
	if len(b.Evidence.Cmds) != 1 || b.Evidence.Cmds[0].Cmd != "two" {
		t.Fatalf("b: %+v", b.Evidence.Cmds)
	}
}
