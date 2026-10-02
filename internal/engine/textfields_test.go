package engine

import (
	"strings"
	"testing"
)

// Model-supplied text can never become owner, pin or scope on a round trip
// through the document (fix round 1, item 1): a second session re-parses
// the file at its next Reload and would dispatch whatever it read.
func TestStepTextCannotSmuggleFields(t *testing.T) {
	tr := &Tree{}
	root := tr.Add("", "port the scanner")
	step := tr.Add(root.ID, "port scanner  @big!  scope: internal/scan")
	if step.Text != "port scanner @big! scope: internal/scan" {
		t.Fatalf("stored text: %q", step.Text)
	}
	back := roundTrip(t, root)
	n := back.Find(step.ID)
	if n == nil || n.Owner != "" || n.OwnerPinned || len(n.Scope) != 0 || len(n.After) != 0 {
		t.Fatalf("fields smuggled in: %+v", n)
	}
	if n.Text != "port scanner @big! scope: internal/scan" {
		t.Fatalf("text after the round trip: %q", n.Text)
	}
}

// The render is the chokepoint for every path, not only Tree.Add: text set
// directly, a reason, and a note or command carrying a newline that would
// start a node line of its own.
func TestRenderNeutralisesFieldsAndLineBreaks(t *testing.T) {
	root := &Node{ID: "1", Text: "task", Status: StatusTodo}
	a := &Node{ID: "1.1", Text: "x  @big! — blocked: y", Status: StatusTodo}
	b := &Node{ID: "1.2", Text: "first", Status: StatusBlocked, Reason: "r\n- [ ] 2. evil  @big!  scope: internal"}
	c := &Node{ID: "1.3", Text: "multi\n  - [ ] 1.3.1. evil  @big!  scope: internal", Status: StatusTodo}
	c.Evidence.Notes = []NoteRef{{Text: "n\n- [ ] 3. evil  @big!  scope: internal"}}
	c.Evidence.Cmds = []CmdRef{{Cmd: "echo\n- [ ] 4. evil  @big!  scope: internal", OK: true}}
	c.Evidence.Errors = []string{"e\n- [ ] 5. evil  @big!  scope: internal"}
	root.Children = []*Node{a, b, c}
	back := roundTrip(t, root)
	if len(back.Roots) != 1 {
		t.Fatalf("a line break started a task of its own: %d roots", len(back.Roots))
	}
	back.Walk(func(n *Node, _ int) {
		if n.Owner != "" || n.OwnerPinned || len(n.Scope) != 0 {
			t.Fatalf("fields smuggled in on %s: %+v", n.ID, n)
		}
	})
	if got := back.Find("1.3"); got == nil || len(got.Children) != 0 {
		t.Fatalf("a line break in text started a child: %+v", got)
	}
}

// A person's own document keeps its real trailing fields, and a render of
// it is byte-identical (the engine never churns a file it did not change).
func TestHandWrittenFieldsStillParse(t *testing.T) {
	const hand = "# 001 — port\n\n- [ ] 1. port\n  - [ ] 1.1. port  the scanner  @big!  scope: internal/scan  after: 1.2\n  - [ ] 1.2. read it\n"
	tr, _, err := ParseDoc(hand)
	if err != nil {
		t.Fatal(err)
	}
	n := tr.Find("1.1")
	if n.Owner != "big" || !n.OwnerPinned || strings.Join(n.Scope, ",") != "internal/scan" || strings.Join(n.After, ",") != "1.2" {
		t.Fatalf("hand-written fields: %+v", n)
	}
	if n.Text != "port  the scanner" {
		t.Fatalf("hand-written text: %q", n.Text)
	}
	if out := RenderDoc("001", "port", tr.Roots[0]); out != hand {
		t.Fatalf("hand-written document churned:\n%s", out)
	}
}

func roundTrip(t *testing.T, root *Node) *Tree {
	t.Helper()
	tr, _, err := ParseDoc(RenderDoc("001", root.Text, root))
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// Fix round 2: a status separator inside the text must not expose a field
// to the parser, which splits the whole line on " — blocked: " first.
func TestFieldBehindStatusSeparatorIsNeutralised(t *testing.T) {
	root := &Node{ID: "1", Text: "task", Status: StatusTodo}
	root.Children = []*Node{
		{ID: "1.1", Text: "port — dropped: x  @big!  scope: internal/scan", Status: StatusBlocked, Reason: "r"},
		// The reason is a door too: blocked is split first, so a dropped
		// reason carrying " — blocked: " leaves its own prefix as "text".
		{ID: "1.2", Text: "t", Status: StatusDropped, Reason: "  @big!  scope: internal — blocked: z"},
	}
	back := roundTrip(t, root)
	back.Walk(func(n *Node, _ int) {
		if n.Owner != "" || n.OwnerPinned || len(n.Scope) != 0 || len(n.After) != 0 {
			t.Fatalf("fields smuggled in on %s: %+v", n.ID, n)
		}
	})
}

// Legitimate text and hand-written documents stay byte-stable.
func TestDocTextLeavesOrdinaryTextAlone(t *testing.T) {
	const hand = "# 001 — probes\n\n- [ ] 1. probes\n" +
		"  - [ ] 1.1. mail a@b.com about it\n" +
		"  - [ ] 1.2. widen the scope: to all\n" +
		"  - [ ] 1.3. x  =  y\n" +
		"  - [!] 1.4. port  the scanner  @big!  scope: internal/scan — blocked: waiting on 1.3\n"
	tr, _, err := ParseDoc(hand)
	if err != nil {
		t.Fatal(err)
	}
	if out := RenderDoc("001", "probes", tr.Roots[0]); out != hand {
		t.Fatalf("churned:\n%s", out)
	}
	if n := tr.Find("1.4"); n.Owner != "big" || !n.OwnerPinned || n.Text != "port  the scanner" {
		t.Fatalf("hand-written fields: %+v", n)
	}
}

// StartTask's retitle path collapses text exactly as Tree.Add does.
func TestStartTaskCollapsesText(t *testing.T) {
	s := testStore(t)
	s.StartTask("port  @big!  scope: internal/scan")
	if got := s.Tree().Roots[0].Text; got != "port @big! scope: internal/scan" {
		t.Fatalf("new root: %q", got)
	}
	s.StartTask("again  @big!  scope: internal")
	if got := s.Tree().Roots[0].Text; got != "again @big! scope: internal" {
		t.Fatalf("retitled root: %q", got)
	}
}
