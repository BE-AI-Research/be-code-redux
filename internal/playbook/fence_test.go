package playbook

import (
	"strings"
	"testing"
)

// One missing closing fence used to swallow every entry after it. inFence was
// never reset at a "## " heading and the heading branch requires !inFence, so
// the rest of the file became body text of the entry with the open fence —
// silently. That is the "losing a fix table" outcome the format doc and the
// loader both say to avoid, arrived at with no warning at all, while Parse
// warns about a bad regex, a missing match, a duplicate field and orphan
// fields.
//
// A heading at column zero ends the fence: a fence that swallows a heading is
// always a mistake, and recovering at the next entry loses one entry's body
// instead of all of them.
func TestUnclosedFenceDoesNotSwallowLaterEntries(t *testing.T) {
	doc := "## First\nmatch: AAA\nkind: compile\ncause: c\nfix:\n1. Write this:\n```go\nfmt.Println(1)\n" +
		"\n## Second\nmatch: BBB\nkind: compile\ncause: c2\nfix:\n1. do it\nnext: n2\n" +
		"\n## Third\nmatch: CCC\nkind: compile\ncause: c3\nfix:\n1. do it\nnext: n3\n"

	entries, warns, err := Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		var titles []string
		for _, e := range entries {
			titles = append(titles, e.Title)
		}
		t.Fatalf("an unclosed fence lost entries: got %d (%v), want 3", len(entries), titles)
	}
	if entries[1].Match == nil || !entries[1].Match.MatchString("BBB") {
		t.Error("the entry after the unclosed fence did not parse its own match")
	}
	if !strings.Contains(strings.Join(warns, "\n"), "fence") {
		t.Errorf("no warning about the unclosed fence: %v", warns)
	}
}

// A fence that is closed properly still protects its body, which is the
// reason the fence handling exists: an entry may legitimately show a line
// beginning with a field name.
func TestClosedFenceStillProtectsItsBody(t *testing.T) {
	doc := "## First\nmatch: AAA\nkind: compile\ncause: c\nfix:\n1. Write:\n```go\nmatch: not a field\n```\nnext: n\n" +
		"\n## Second\nmatch: BBB\nkind: compile\ncause: c2\nfix:\n1. x\nnext: n2\n"
	entries, warns, err := Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].Match.String() != "AAA" {
		t.Errorf("a fenced %q line rewrote the routing regex: %s", "match:", entries[0].Match)
	}
	if !strings.Contains(entries[0].Fix, "match: not a field") {
		t.Error("the fenced body was lost")
	}
	if strings.Contains(strings.Join(warns, "\n"), "fence") {
		t.Errorf("warned about a fence that was properly closed: %v", warns)
	}
}

// An empty match compiles to a regex that matches everything — a silent
// catch-all, the one thing the format doc says in bold not to write, arrived
// at by leaving a value off.
func TestEmptyMatchIsRefusedRatherThanBecomingACatchAll(t *testing.T) {
	doc := "## Empty\nmatch:\nkind: compile\ncause: c\nfix:\n1. x\nnext: n\n" +
		"\n## Real\nmatch: BBB\nkind: compile\ncause: c2\nfix:\n1. x\nnext: n2\n"
	entries, warns, err := Parse(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Title == "Empty" {
			t.Error("an entry with an empty match was kept; it matches every failure")
		}
	}
	if len(entries) != 1 || entries[0].Title != "Real" {
		t.Fatalf("the rest of the playbook did not survive: %+v", entries)
	}
	if !strings.Contains(strings.Join(warns, "\n"), "match") {
		t.Errorf("no warning about the empty match: %v", warns)
	}
}

// And the explicit catch-all is still refused, which it was before.
func TestExplicitCatchAllIsStillParsed(t *testing.T) {
	// A single "." is a legal regex and the loader's own policy decides what
	// to do with it; Parse's job is only to compile it. This pins that the
	// empty-value fix did not change that.
	entries, _, err := Parse("## Any\nmatch: .\nkind: any\ncause: c\nfix:\n1. x\nnext: n\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Match.String() != "." {
		t.Fatalf("got %+v", entries)
	}
}
