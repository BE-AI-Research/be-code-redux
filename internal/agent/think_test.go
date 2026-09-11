package agent

import (
	"strings"
	"testing"
)

func TestStripThink(t *testing.T) {
	in := "<think>\nlet me reason...\n</think>\nThe answer is 4."
	if got := StripThink(in); got != "The answer is 4." {
		t.Fatalf("got %q", got)
	}
	// Unterminated block (ran out of tokens) is dropped.
	if got := StripThink("prefix <think>never ends"); got != "prefix" {
		t.Fatalf("got %q", got)
	}
	// Text without tags passes through.
	if got := StripThink("plain text"); got != "plain text" {
		t.Fatalf("got %q", got)
	}
}

func TestThinkFilterStreaming(t *testing.T) {
	f := &ThinkFilter{}
	var out strings.Builder
	// Tags split across chunk boundaries — the hard case.
	for _, chunk := range []string{"Hello <thi", "nk>secret reasoning", " more</think", "> world"} {
		out.WriteString(f.Feed(chunk))
	}
	out.WriteString(f.Flush())
	got := out.String()
	if strings.Contains(got, "secret") {
		t.Fatalf("thought leaked: %q", got)
	}
	if !strings.Contains(got, "Hello") || !strings.Contains(got, "world") {
		t.Fatalf("visible text lost: %q", got)
	}
}

func TestThinkFilterNoTags(t *testing.T) {
	f := &ThinkFilter{}
	got := f.Feed("just streaming normally, 2 < 3 kinds of text") + f.Flush()
	if got != "just streaming normally, 2 < 3 kinds of text" {
		t.Fatalf("got %q", got)
	}
}

func TestThinkFilterHoldbackReleased(t *testing.T) {
	f := &ThinkFilter{}
	// "<th" could start a tag; it must be held then released at flush.
	got := f.Feed("value <th") + f.Flush()
	if got != "value <th" {
		t.Fatalf("got %q", got)
	}
}
