package engine

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestActiveWorkOutranksHistory: under a tight budget the finished reports
// condense and the work in flight stays whole, because that is the work the
// model is about to continue.
func TestActiveWorkOutranksHistory(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 4; i++ {
		id := s.Plan(fmt.Sprintf("finished task %d", i), []string{"step"})
		s.SetStatus(id+".1", StatusDoing, "")
		s.Observe(Event{Tool: "shell", Args: map[string]any{"command": "go build ./..."},
			Content: strings.Repeat("noise\n", 200)})
		s.SetStatus(id+".1", StatusDone, "")
		s.SetStatus(id, StatusDone, "")
	}
	live := s.Plan("the live task", []string{"the live step"})
	s.SetStatus(live+".1", StatusDoing, "")
	s.Observe(Event{Tool: "shell", Args: map[string]any{"command": "go test ./parser"},
		Content: "--- FAIL: TestQuote\n    parser_test.go:88: unexpected EOF\n"})

	block := s.Render(1200, func(string) bool { return false })
	if len(block) > 1200 {
		t.Fatalf("over budget: %d bytes", len(block))
	}
	for _, want := range []string{"the live task", "the live step", "parser_test.go:88: unexpected EOF"} {
		if !strings.Contains(block, want) {
			t.Fatalf("live work was cut; missing %q:\n%s", want, block)
		}
	}
	if !strings.Contains(block, "finished task 0") {
		t.Fatal("an old task vanished entirely instead of condensing to a line")
	}
}

// TestBlockStartsWithReportsThenActive: order matters — history first, the
// live branch last, so the newest thing is nearest the model's attention.
func TestBlockStartsWithReportsThenActive(t *testing.T) {
	s := testStore(t)
	done := s.Plan("finished", []string{"a"})
	s.SetStatus(done+".1", StatusDone, "")
	s.SetStatus(done, StatusDone, "")
	live := s.Plan("live", []string{"b"})
	s.SetStatus(live+".1", StatusDoing, "")
	block := s.Render(4096, func(string) bool { return false })
	if strings.Index(block, "finished") > strings.Index(block, "live") {
		t.Fatalf("order wrong:\n%s", block)
	}
}

// TestEmptyStoreRendersNothing: a fresh session adds no prompt weight.
func TestEmptyStoreRendersNothing(t *testing.T) {
	if got := testStore(t).Render(4096, func(string) bool { return false }); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}

// TestPlannedTaskIsVisibleBeforeAnythingIsDoing: ruling T4-b — a task the
// model has planned but not yet started (nothing marked doing) must still
// render, because that is exactly when it most needs to see the plan it
// just made.
func TestPlannedTaskIsVisibleBeforeAnythingIsDoing(t *testing.T) {
	s := testStore(t)
	s.Plan("fix the parser", []string{"find the bug", "fix it"})
	block := s.Render(4096, func(string) bool { return false })
	for _, want := range []string{"fix the parser", "find the bug"} {
		if !strings.Contains(block, want) {
			t.Fatalf("planned task not visible; missing %q:\n%s", want, block)
		}
	}
}

// TestASingleReportIsSqueezedRatherThanDroppedWhenBudgetIsImpossible: once
// the ladder and the drop pass both bottom out, the one remaining report is
// trimmed byte-wise rather than vanishing outright, and the active branch —
// which is never touched by either pass — still survives whole.
func TestASingleReportIsSqueezedRatherThanDroppedWhenBudgetIsImpossible(t *testing.T) {
	s := testStore(t)
	id := s.Plan("finished task", []string{"step"})
	s.SetStatus(id+".1", StatusDone, "")
	s.SetStatus(id, StatusDone, "")
	live := s.Plan("the live task", []string{"the live step"})
	s.SetStatus(live+".1", StatusDoing, "")
	s.Observe(Event{Tool: "shell", Args: map[string]any{"command": "go test ./parser"},
		Content: "--- FAIL: TestQuote\n    parser_test.go:88: unexpected EOF\n"})

	tail := activeBranchText(&s.tree, func(string) bool { return false })
	budget := len(tail) + 5 // room for barely a sliver of the finished report
	block := s.Render(budget, func(string) bool { return false })
	if !strings.Contains(block, "the live task") || !strings.Contains(block, "parser_test.go:88: unexpected EOF") {
		t.Fatalf("active branch was not kept whole:\n%s", block)
	}
	if !strings.HasSuffix(block, "(reports condensed)") {
		t.Fatalf("missing the condensed marker:\n%s", block)
	}
}

// TestDroppedRawItemsAreSurfacedInTheActiveBranch: a doing node whose
// buffer was capped (record.go's capNode) must not render as if its raw
// evidence were complete.
func TestDroppedRawItemsAreSurfacedInTheActiveBranch(t *testing.T) {
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
	block := s.Render(8192, func(string) bool { return false })
	if !strings.Contains(block, "item(s) dropped") {
		t.Fatalf("dropped raw items not surfaced:\n%s", block)
	}
}

func TestTrimLinesIsUTF8Safe(t *testing.T) {
	// No newline anywhere, so trimLines must fall back to a rune-boundary
	// cut. Each "é" is 2 bytes; a byte-5 cut of 10 of them lands mid-rune.
	s := strings.Repeat("é", 10)
	out := trimLines(s, 5)
	if !utf8.ValidString(out) {
		t.Fatalf("trimmed string is not valid UTF-8: %q", out)
	}
	if len(out) > 5 {
		t.Fatalf("trimmed string exceeds budget: %q (%d bytes)", out, len(out))
	}
}
