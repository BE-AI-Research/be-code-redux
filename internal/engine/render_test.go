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
