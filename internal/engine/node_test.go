package engine

import "testing"

// TestIDsAreDottedPathsAndSurviveSiblingChanges: a child's id is its
// parent's id plus its 1-based position, and adding a child to one node
// never renumbers another branch.
func TestIDsAreDottedPathsAndSurviveSiblingChanges(t *testing.T) {
	tr := &Tree{}
	a := tr.Add("", "fix the parser")
	b := tr.Add("", "add the cache")
	if a.ID != "1" || b.ID != "2" {
		t.Fatalf("roots: %q %q", a.ID, b.ID)
	}
	c1 := tr.Add(a.ID, "find the bug")
	c2 := tr.Add(a.ID, "fix and verify")
	if c1.ID != "1.1" || c2.ID != "1.2" {
		t.Fatalf("children: %q %q", c1.ID, c2.ID)
	}
	deep := tr.Add(c1.ID, "read lexer.go")
	if deep.ID != "1.1.1" {
		t.Fatalf("grandchild: %q", deep.ID)
	}
	// Adding under 1 must not touch 2.
	if b.ID != "2" {
		t.Fatalf("sibling renumbered: %q", b.ID)
	}
}

// TestFindAndWalk: every node is reachable by id, and Walk visits parents
// before children in document order.
func TestFindAndWalk(t *testing.T) {
	tr := &Tree{}
	a := tr.Add("", "a")
	tr.Add(a.ID, "a1")
	tr.Add(a.ID, "a2")
	tr.Add("", "b")
	if n := tr.Find("1.2"); n == nil || n.Text != "a2" {
		t.Fatalf("find 1.2: %+v", n)
	}
	if tr.Find("9.9") != nil {
		t.Fatal("found a node that does not exist")
	}
	var order []string
	tr.Walk(func(n *Node, depth int) { order = append(order, n.ID) })
	want := []string{"1", "1.1", "1.2", "2"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("walk order %v, want %v", order, want)
		}
	}
}

// TestOneDoingAtATime: marking a node doing clears any other doing node,
// and closing a node stamps Closed exactly once.
func TestOneDoingAtATime(t *testing.T) {
	tr := &Tree{}
	a := tr.Add("", "a")
	b := tr.Add("", "b")
	tr.SetStatus(a.ID, StatusDoing, "")
	tr.SetStatus(b.ID, StatusDoing, "")
	if a.Status != StatusTodo {
		t.Fatalf("a still %s; doing must be exclusive", a.Status)
	}
	if got := tr.Doing(); got == nil || got.ID != b.ID {
		t.Fatalf("doing: %+v", got)
	}
	tr.SetStatus(b.ID, StatusDone, "")
	if b.Closed.IsZero() {
		t.Fatal("Closed not stamped")
	}
	first := b.Closed
	tr.SetStatus(b.ID, StatusDone, "")
	if !b.Closed.Equal(first) {
		t.Fatal("Closed re-stamped on a second close")
	}
	if tr.Doing() != nil {
		t.Fatal("a closed node is still doing")
	}
}

// TestBlockedAndDroppedKeepTheirReason: the two terminal statuses that are
// not success carry why, because a report six turns later reads very
// differently for "could not" and "decided not to".
func TestBlockedAndDroppedKeepTheirReason(t *testing.T) {
	tr := &Tree{}
	a := tr.Add("", "a")
	tr.SetStatus(a.ID, StatusBlocked, "needs the API key")
	if a.Reason != "needs the API key" {
		t.Fatalf("reason: %q", a.Reason)
	}
	tr.SetStatus(a.ID, StatusDropped, "not needed after all")
	if a.Status != StatusDropped || a.Reason != "not needed after all" {
		t.Fatalf("%s / %q", a.Status, a.Reason)
	}
}

// TestTerminalAndActiveBranch: a root is terminal when it and every
// descendant are done, blocked or dropped; ActiveBranch is the path from a
// root down to the doing node.
func TestTerminalAndActiveBranch(t *testing.T) {
	tr := &Tree{}
	a := tr.Add("", "a")
	a1 := tr.Add(a.ID, "a1")
	a2 := tr.Add(a.ID, "a2")
	if tr.Terminal(a) {
		t.Fatal("todo children are not terminal")
	}
	tr.SetStatus(a1.ID, StatusDone, "")
	tr.SetStatus(a2.ID, StatusDropped, "not needed")
	tr.SetStatus(a.ID, StatusDone, "")
	if !tr.Terminal(a) {
		t.Fatal("all-terminal children should be terminal")
	}
	b := tr.Add("", "b")
	b1 := tr.Add(b.ID, "b1")
	tr.SetStatus(b1.ID, StatusDoing, "")
	branch := tr.ActiveBranch()
	if len(branch) != 2 || branch[0].ID != b.ID || branch[1].ID != b1.ID {
		t.Fatalf("branch %v", branch)
	}
}
