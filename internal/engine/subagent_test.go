package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/subagent"
)

func cardsForTest() map[string]subagent.Card {
	return map[string]subagent.Card{
		"big":    {Name: "big", SubAgent: true},
		"claude": {Name: "claude", SubAgent: true, MaxScope: []string{"docs"}},
	}
}

func planOwned(t *testing.T) (*Store, string) {
	t.Helper()
	s := testStore(t)
	s.SetCards(cardsForTest())
	root := s.Plan("port the scanner", []string{"list the call sites", "port internal/scan", "write the note"})
	if err := s.SetStatus(root+".1", StatusDoing, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOwner(root+".2", "big", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetScope(root+".2", []string{"internal/scan"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(root+".2", "replace the token loop"); err != nil {
		t.Fatal(err)
	}
	return s, root
}

func TestOwnerAndScopeRules(t *testing.T) {
	s, root := planOwned(t)
	if err := s.SetOwner(root+".3", "claude", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOwner(root+".3", "big", false); err == nil || !strings.Contains(err.Error(), "assigned by the operator") {
		t.Fatalf("pinned owner changed by the model: %v", err)
	}
	if err := s.SetOwner(root+".3", "big", true); err != nil {
		t.Fatalf("the operator may re-pin: %v", err)
	}
	if err := s.SetOwner(root+".3", "claude", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetScope(root+".3", []string{"cmd"}); err == nil || !strings.Contains(err.Error(), "may only own paths under docs") {
		t.Fatalf("max_scope not enforced: %v", err)
	}
	if err := s.SetScope(root+".3", []string{"docs/x.md"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOwner(root+".2.1", "claude", false); err == nil {
		t.Fatal("a child of an assigned subtree cannot take its own owner")
	}
	if err := s.SetOwner(root+".1", "nobody", false); err == nil || !strings.Contains(err.Error(), "not a sub-agent") {
		t.Fatalf("unknown owner accepted: %v", err)
	}
	if s.tree.Find(root+".2").Owner != "big" || s.tree.OwnerOf(root+".2.1") != "big" {
		t.Fatal("owner lost")
	}
}

// TestSetOwnerRefusesANestedPen: the direct-children check alone lets a pen
// nest inside another pen two or more levels down — assign the grandchild
// first, so the parent's own check (only its immediate children) would
// otherwise miss it entirely when the grandparent is assigned second.
func TestSetOwnerRefusesANestedPen(t *testing.T) {
	s := testStore(t)
	s.SetCards(cardsForTest())
	root := s.Plan("port the scanner", []string{"top step"})
	level2, err := s.Add(root+".1", "level two")
	if err != nil {
		t.Fatal(err)
	}
	grandchild, err := s.Add(level2, "level three")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetOwner(grandchild, "big", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOwner(root+".1", "claude", false); err == nil || !strings.Contains(err.Error(), "one subtree, one pen") {
		t.Fatalf("a pen nested two levels down was accepted: %v", err)
	}
}

// TestSetOwnerUnassignAndDispatchedRefusal covers two brief-stated
// behaviours that had no assertion: owner "" unassigns (clearing the pin
// only through the operator's own form of the call), and a dispatched node
// refuses reassignment until it is stopped.
func TestSetOwnerUnassignAndDispatchedRefusal(t *testing.T) {
	s, root := planOwned(t)
	// An unpinned unassign needs no special authority.
	if err := s.SetOwner(root+".2", "", false); err != nil {
		t.Fatal(err)
	}
	if n := s.tree.Find(root + ".2"); n.Owner != "" || n.OwnerPinned {
		t.Fatalf("unassign left an owner behind: %+v", n)
	}
	if s.tree.OwnerOf(root+".2.1") != "" {
		t.Fatal("a child still inherited the cleared owner")
	}
	// Re-assign and pin it, the operator's own form.
	if err := s.SetOwner(root+".2", "big", true); err != nil {
		t.Fatal(err)
	}
	// The model may not unassign a pinned owner...
	if err := s.SetOwner(root+".2", "", false); err == nil || !strings.Contains(err.Error(), "assigned by the operator") {
		t.Fatalf("a pinned owner was removed without operator authority: %v", err)
	}
	// ...but the operator's own unassign clears the owner and the pin together.
	if err := s.SetOwner(root+".2", "", true); err != nil {
		t.Fatal(err)
	}
	if n := s.tree.Find(root + ".2"); n.Owner != "" || n.OwnerPinned {
		t.Fatalf("the operator's unassign left the pin set: %+v", n)
	}
	// A dispatched node cannot be reassigned until it is stopped.
	if err := s.SetOwner(root+".2", "big", false); err != nil {
		t.Fatal(err)
	}
	s.SetDispatched(root+".2", true)
	if err := s.SetOwner(root+".2", "claude", false); err == nil || !strings.Contains(err.Error(), "stop it first") {
		t.Fatalf("owner changed on a dispatched node: %v", err)
	}
}

// TestSetScopeSucceedsWhenTheOwnerHasNoCard: a node can carry an owner the
// store has no card for — a document hand-edited with "@ghost" before
// SetCards ever ran, or a coworker later removed from config. There is no
// max_scope to violate for an owner the store does not recognise, so
// SetScope must not refuse on that account alone (it is warnOwners' job to
// flag the unknown owner, not SetScope's to block the scope).
func TestSetScopeSucceedsWhenTheOwnerHasNoCard(t *testing.T) {
	s := testStore(t)
	root := s.Plan("port the scanner", []string{"step"})
	s.tree.Find(root + ".1").Owner = "ghost"
	if err := s.SetScope(root+".1", []string{"anything/at/all"}); err != nil {
		t.Fatalf("SetScope refused an owner with no card: %v", err)
	}
}

// TestObserveForRecordsNothingWhenThePenRootIsGone: activeNodeForLocked
// used to fall back to the main model's own pen when a dispatched root
// could not be found (a hand-edited document, or a stale pen id after the
// node was removed some other way) — exactly the "confidently wrong"
// misfiling its own comment warns against. It must record nothing instead.
func TestObserveForRecordsNothingWhenThePenRootIsGone(t *testing.T) {
	s, root := planOwned(t)
	s.SetDispatched(root+".2", true)
	s.tree.Remove(s.tree.Find(root + ".2"))
	footer := s.ObserveFor(root+".2", Event{Tool: "shell", Args: map[string]any{"command": "go test ./..."}, Content: "ok"})
	if footer != "" {
		t.Fatalf("unexpected footer: %q", footer)
	}
	if n := s.tree.Find(root + ".1"); len(n.Evidence.Raw) != 0 {
		t.Fatalf("evidence was misfiled onto the main model's pen: %+v", n.Evidence)
	}
}

func TestOneDoingPerPen(t *testing.T) {
	s, root := planOwned(t)
	s.SetDispatched(root+".2", true)
	if err := s.SetStatus(root+".2.1", StatusDoing, ""); err != nil {
		t.Fatal(err)
	}
	if d := s.tree.Doing(); d == nil || d.ID != root+".1" {
		t.Fatalf("the main model's doing node was disturbed: %+v", d)
	}
	if d := s.tree.DoingUnder(root + ".2"); d == nil || d.ID != root+".2.1" {
		t.Fatalf("sub-agent's doing node: %+v", d)
	}
	if _, err := s.Add(root+".2", "update the tests"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus(root+".2.2", StatusDoing, ""); err != nil {
		t.Fatal(err)
	}
	if s.tree.Find(root+".2.1").Status != StatusTodo || s.tree.DoingUnder(root+".2").ID != root+".2.2" {
		t.Fatal("a new doing node under the pen must send the previous one back to todo")
	}
	if s.tree.Find(root+".1").Status != StatusDoing {
		t.Fatal("the main model's doing node changed")
	}
	// Evidence filed for the pen lands on its own doing node, and the main
	// model's evidence on its own.
	s.ObserveFor(root+".2", Event{Tool: "shell", Args: map[string]any{"command": "go test ./..."}, Content: "ok"})
	if n := s.tree.Find(root + ".2.2"); len(n.Evidence.Raw) != 1 || n.Calls != 1 {
		t.Fatalf("sub-agent evidence: %+v", n.Evidence)
	}
	s.Observe(Event{Tool: "shell", Args: map[string]any{"command": "ls"}, Content: "ok"})
	if n := s.tree.Find(root + ".1"); len(n.Evidence.Raw) != 1 {
		t.Fatalf("main evidence: %+v", n.Evidence)
	}
}

func TestUnfiledPerPen(t *testing.T) {
	s, root := planOwned(t)
	s.SetDispatched(root+".2", true)
	s.ObserveFor(root+".2", Event{Tool: "read_file", Args: map[string]any{"path": "x.go"}, Content: "..."})
	sub := s.tree.Find(root + ".2")
	var unfiled *Node
	for _, c := range sub.Children {
		if c.Text == unfiledText {
			unfiled = c
		}
	}
	if unfiled == nil || unfiled.Status != StatusDoing {
		t.Fatalf("unfiled node under the pen missing: %+v", sub.Children)
	}
	if err := s.SetStatus(root+".2.1", StatusDoing, ""); err != nil {
		t.Fatal(err)
	}
	if n := s.tree.Find(root + ".2.1"); n.Calls != 1 {
		t.Fatalf("unfiled evidence not adopted by the pen's step: %+v", n)
	}
	if s.tree.Find(root+".1").Calls != 0 {
		t.Fatal("the main model's step adopted the sub-agent's unfiled evidence")
	}
}

func TestCloseAsAndInterrupt(t *testing.T) {
	s, root := planOwned(t)
	s.SetDispatched(root+".2", true)
	_ = s.SetStatus(root+".2.1", StatusDoing, "")
	s.ObserveFor(root+".2", Event{Tool: "write_file", Args: map[string]any{"path": "internal/scan/a.go", "content": "x"}, Content: "wrote"})
	if got := s.Touched(root + ".2"); len(got) != 1 || got[0] != "internal/scan/a.go" {
		t.Fatalf("Touched: %q", got)
	}
	at := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	if err := s.Interrupt(root+".2", at, 1, []string{"internal/scan/a.go"}); err != nil {
		t.Fatal(err)
	}
	if s.tree.Find(root+".2.1").Status != StatusTodo || s.tree.DoingUnder(root+".2") != nil {
		t.Fatal("Interrupt must send the pen's doing node back to todo")
	}
	steps := s.Steps()
	sub := steps[0].Children[1]
	if !sub.Interrupted || len(sub.Touched) != 1 || sub.Touched[0] != "internal/scan/a.go" {
		t.Fatalf("Steps must surface the interruption: %+v", sub)
	}
	if len(s.Dispatched()) != 0 {
		t.Fatal("Interrupt must clear the dispatched mark")
	}
	s.SetDispatched(root+".2", true)
	_ = s.SetStatus(root+".2.1", StatusDoing, "")
	if err := s.CloseAs(root+".2", "big", "done", ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{root + ".2", root + ".2.1"} {
		n := s.tree.Find(id)
		if n.Status != StatusDone || n.DoneBy != "big" {
			t.Fatalf("%s after CloseAs: %+v", id, n)
		}
	}
	if err := s.CloseAs(root+".3", "big", "blocked", "turn cap of 40 reached"); err != nil {
		t.Fatal(err)
	}
	if n := s.tree.Find(root + ".3"); n.Status != StatusBlocked || n.Reason != "turn cap of 40 reached" {
		t.Fatalf("blocked: %+v", n)
	}
}

// TestCloseAsMustNotSkipASiblingWhenRemovingASpentUnfiledChild is a
// regression test for a bug in CloseAs's close closure: it ranged over
// x.Children while a recursive call could remove an element from that same
// slice (a spent, childless "unfiled" node). Removing element i shifts the
// slice's tail left in the backing array; the outer range loop, still
// working from the header it captured before the mutation, then reads
// stale/shifted data for every index after i. With only one real sibling
// after the removed node the shift happens to leave a harmless duplicate
// (the loop still visits that one sibling, just via a second stale slot),
// so the shape needs two real siblings after the unfiled one to actually
// expose it: the first of the two is skipped outright — left open inside a
// subtree CloseAs reports as closed — and the second is visited twice
// (harmless on its own, since a second close of an already-terminal node is
// a no-op, but it is the tell that a sibling went unvisited).
func TestCloseAsMustNotSkipASiblingWhenRemovingASpentUnfiledChild(t *testing.T) {
	s := testStore(t)
	s.SetCards(cardsForTest())
	root := s.Plan("port the scanner", nil)
	if err := s.SetOwner(root, "big", false); err != nil {
		t.Fatal(err)
	}
	first, err := s.Add(root, "first step")
	if err != nil {
		t.Fatal(err)
	}
	// Built the way the store itself opens an unfiled node: no raw
	// evidence, no children, so CloseAs removes it rather than closing it.
	s.tree.Add(root, unfiledText)
	second, err := s.Add(root, "second step")
	if err != nil {
		t.Fatal(err)
	}
	third, err := s.Add(root, "third step")
	if err != nil {
		t.Fatal(err)
	}
	s.SetDispatched(root, true)
	if err := s.CloseAs(root, "big", "done", ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{first, second, third} {
		n := s.tree.Find(id)
		if n == nil || n.Status != StatusDone || n.DoneBy != "big" {
			t.Fatalf("%s after CloseAs: %+v", id, n)
		}
	}
	for _, c := range s.tree.Find(root).Children {
		if c.Text == unfiledText {
			t.Fatalf("the spent unfiled node must be gone: %+v", c)
		}
	}
}

// TestCloseAsMustNotRemoveARoot: adoptUnfiledLocked refuses to remove a
// root-level "unfiled" node even when it is spent, because s.files and
// s.extra are index-parallel to Roots — taking one out shifts every later
// task onto the previous one's document. CloseAs's own removal guard did
// not carry that same check, so an unfiled root that gets assigned and
// dispatched (activeNodeForLocked opens one at the root when no task is
// open yet) and then closed while still empty would corrupt that parity on
// the next load. It must be closed like any other node instead.
func TestCloseAsMustNotRemoveARoot(t *testing.T) {
	s := testStore(t)
	s.SetCards(cardsForTest())
	// Nothing planned yet: Observe opens a root-level unfiled node the way
	// a session with no task open does.
	s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "x.go"}, Content: "..."})
	if len(s.tree.Roots) != 1 || s.tree.Roots[0].Text != unfiledText {
		t.Fatalf("expected a root-level unfiled node: %+v", s.tree.Roots)
	}
	root := s.tree.Roots[0].ID
	if err := s.SetOwner(root, "big", false); err != nil {
		t.Fatal(err)
	}
	s.SetDispatched(root, true)
	// Empty it back out, so CloseAs sees no raw evidence and no children —
	// the same "spent" shape adoptUnfiledLocked already refuses to remove
	// at the root.
	s.tree.Find(root).Evidence = Evidence{}
	if err := s.CloseAs(root, "big", "done", ""); err != nil {
		t.Fatal(err)
	}
	if len(s.tree.Roots) != 1 || s.tree.Roots[0].ID != root {
		t.Fatalf("CloseAs removed the root instead of closing it: %+v", s.tree.Roots)
	}
	if n := s.tree.Roots[0]; n.Status != StatusDone || n.DoneBy != "big" {
		t.Fatalf("root after CloseAs: %+v", n)
	}
}

func TestStepsAndDispatchContext(t *testing.T) {
	s, root := planOwned(t)
	steps := s.Steps()
	if len(steps) != 1 || steps[0].ID != root || steps[0].Children[1].Owner != "big" || steps[0].Children[1].Children[0].Owner != "big" {
		t.Fatalf("Steps: %+v", steps)
	}
	text, children, ctx := s.DispatchContext(root + ".2")
	if text != "port internal/scan" || len(children) != 1 || !strings.HasPrefix(children[0], root+".2.1 ") {
		t.Fatalf("DispatchContext: %q %q", text, children)
	}
	if !strings.Contains(ctx, "port the scanner") {
		t.Fatalf("context lacks the parent task: %q", ctx)
	}
}

func TestRenderShowsADispatchedSubtreeAsOneLine(t *testing.T) {
	s, root := planOwned(t)
	s.SetDispatched(root+".2", true)
	_ = s.SetStatus(root+".2.1", StatusDoing, "")
	s.ObserveFor(root+".2", Event{Tool: "read_file", Args: map[string]any{"path": "a.go"}, Content: strings.Repeat("secret-sub-agent-output\n", 20)})
	block := s.Render(8000, func(string) bool { return false })
	if !strings.Contains(block, "@big running (at "+root+".2.1, 1 tool call)") {
		t.Fatalf("no running line:\n%s", block)
	}
	if strings.Contains(block, "secret-sub-agent-output") {
		t.Fatal("the sub-agent's raw buffer must not render in the main model's block")
	}
}

func TestLoadWarnsOnUnknownOwnerAndScopeOutsideMax(t *testing.T) {
	s, root := planOwned(t)
	_ = s.SetOwner(root+".3", "claude", true)
	_ = s.SetScope(root+".3", []string{"docs/x.md"})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	// Reopen with a config where claude may only own README.md and big is gone.
	s2, err := OpenAt(s.dir, s.root, "s1", true, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	s2.SetCards(map[string]subagent.Card{"claude": {Name: "claude", SubAgent: true, MaxScope: []string{"README.md"}}})
	warns := s2.TakeWarnings()
	joined := strings.Join(warns, "\n")
	if !strings.Contains(joined, "big is not a sub-agent") || !strings.Contains(joined, "claude may only own paths under README.md") {
		t.Fatalf("warnings: %q", warns)
	}
}
