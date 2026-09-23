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

// TestAHandEditedAssignmentWinsOnReload is spec §3.1/§4.3: the assignment
// is the operator's exactly as the status mark is. mergeNode used to copy
// text, status, reason, evidence and children and nothing else, so an
// `@owner`, a `scope:` or an `after:` typed into the document never reached
// the tree — and because the merge then recorded the file's hash as known,
// the next Flush rendered the engine's tag-less tree back over the
// operator's own file with no `.edited-` copy kept.
func TestAHandEditedAssignmentWinsOnReload(t *testing.T) {
	s, root := openTest(t, "s1", false)
	s.SetCards(cardsForTest())
	id := s.Plan("port the scanner", []string{"list the call sites", "port internal/scan", "write the note"})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	name := "001-port-the-scanner.md"

	// The operator, in their editor: assigns one step and orders another.
	doc := readDoc(t, root, name)
	doc = strings.Replace(doc, "- [ ] 1.2. port internal/scan",
		"- [ ] 1.2. port internal/scan  @big  scope: ./internal/scan/, internal/scan_test.go", 1)
	doc = strings.Replace(doc, "- [ ] 1.3. write the note",
		"- [ ] 1.3. write the note  @claude!  scope: docs/scanner.md  after: 1.2", 1)
	writeDocFile(t, root, name, doc)

	s.Reload()
	n := s.tree.Find(id + ".2")
	if n.Owner != "big" || n.OwnerPinned {
		t.Fatalf("the document's owner never reached the tree: %+v", n)
	}
	// Cleaned at parse time, so the ready rule and the scoped registry see
	// one normalised form of the operator's "./internal/scan/".
	if len(n.Scope) != 2 || n.Scope[0] != "internal/scan" || n.Scope[1] != "internal/scan_test.go" {
		t.Fatalf("scope: %q", n.Scope)
	}
	if s.tree.OwnerOf(id+".2") != "big" {
		t.Fatal("OwnerOf does not see the hand-edited owner")
	}
	p := s.tree.Find(id + ".3")
	if p.Owner != "claude" || !p.OwnerPinned || len(p.After) != 1 || p.After[0] != "1.2" {
		t.Fatalf("pinned owner / after: %+v", p)
	}

	// And the next Flush keeps them: the engine must not write its own
	// render back over the operator's tags.
	s.Note(id+".1", "found them all", "", false, false)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	got := readDoc(t, root, name)
	for _, want := range []string{
		"@big  scope: internal/scan, internal/scan_test.go",
		"@claude!  scope: docs/scanner.md  after: 1.2",
		"note: found them all",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the flush lost %q:\n%s", want, got)
		}
	}

	// Removing an assignment by hand is equally the operator's intent.
	doc = strings.Replace(got, "  @big  scope: internal/scan, internal/scan_test.go", "", 1)
	writeDocFile(t, root, name, doc)
	s.Reload()
	if n := s.tree.Find(id + ".2"); n.Owner != "" || len(n.Scope) != 0 {
		t.Fatalf("a hand-removed assignment was put back: %+v", n)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := readDoc(t, root, name); strings.Contains(got, "@big") {
		t.Fatalf("the flush put @big back:\n%s", got)
	}
}

// TestScopeEntriesAreCleanedAtParseTime: a hand-written trailing slash or
// "./" prefix used to reach the scoped registry verbatim, where it matched
// nothing — every write refused while the refusal quoted the operator's own
// text back. An entry that escapes the workspace is dropped with a note,
// the way an id repair leaves one.
func TestScopeEntriesAreCleanedAtParseTime(t *testing.T) {
	tr, _, err := ParseDoc("# 001 — port\n\n- [ ] 1. port  @big  scope: ./internal/scan/, ../secrets, docs\n")
	if err != nil {
		t.Fatal(err)
	}
	n := tr.Roots[0]
	if len(n.Scope) != 2 || n.Scope[0] != "internal/scan" || n.Scope[1] != "docs" {
		t.Fatalf("scope: %q", n.Scope)
	}
	if !subagent.InScope(n.Scope, "internal/scan/token.go") {
		t.Fatal("a cleaned entry must match its own files")
	}
	var noted bool
	for _, note := range n.Evidence.Notes {
		if strings.Contains(note.Text, "../secrets") && strings.Contains(note.Text, "dropped") {
			noted = true
		}
	}
	if !noted {
		t.Fatalf("the dropped entry was not noted: %+v", n.Evidence.Notes)
	}
}

// TestDoneByIsStampedOnAnAlreadyClosedNodeAndSurvivesAReload covers two
// halves of the same report line. CloseAs skipped a node that was already
// terminal, so a sub-agent that closed its own root with its task tool
// never got DoneBy — and DoneBy was persisted nowhere, so "done by big"
// vanished at the next open while "(14m, 22 tool calls)" came back.
func TestDoneByIsStampedOnAnAlreadyClosedNodeAndSurvivesAReload(t *testing.T) {
	s, root := openTest(t, "s1", false)
	s.SetCards(cardsForTest())
	id := s.Plan("port the scanner", []string{"port internal/scan"})
	if err := s.SetOwner(id+".1", "big", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetScope(id+".1", []string{"internal/scan"}); err != nil {
		t.Fatal(err)
	}
	s.SetDispatched(id+".1", true)
	// The sub-agent marks its own root done with its task tool...
	if err := s.SetStatus(id+".1", StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	// ...and the runner then closes the subtree on its behalf.
	if err := s.CloseAs(id+".1", "big", "done", ""); err != nil {
		t.Fatal(err)
	}
	if n := s.tree.Find(id + ".1"); n.DoneBy != "big" {
		t.Fatalf("DoneBy not stamped on an already-closed node: %+v", n)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenAt(s.dir, root, "s1", true, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	n := s2.tree.Find(id + ".1")
	if n == nil || n.DoneBy != "big" {
		t.Fatalf("DoneBy did not survive the reload: %+v", n)
	}
	// docs/task-format.md: "A closed step a sub-agent did reads `done by big
	// (14m, 22 tool calls)` in its report."
	if line := statusLine(n); !strings.Contains(line, "done by big") {
		t.Fatalf("the report line lost it: %q", line)
	}
}

// TestDoingIgnoresAnOrphanedSubAgentMark: a hard kill leaves a [>] inside
// an assigned subtree, and Tree.dispatched is transient — empty at the next
// load — so penOf answered "" and Doing() handed the sub-agent's step to
// the main model, which then filed its evidence onto it.
func TestDoingIgnoresAnOrphanedSubAgentMark(t *testing.T) {
	s, root := planOwned(t)
	_ = root
	sub := s.tree.Find(s.tree.Roots[0].ID + ".2.1")
	sub.Status = StatusDoing
	// Nothing is dispatched: this is the state a fresh load sees.
	if len(s.Dispatched()) != 0 {
		t.Fatal("the fixture must have nothing dispatched")
	}
	if d := s.tree.Doing(); d == nil || d.ID != s.tree.Roots[0].ID+".1" {
		t.Fatalf("Doing returned the sub-agent's step: %+v", d)
	}
	// And with the main model's own step closed, there is simply no doing
	// node for it rather than somebody else's.
	_ = s.SetStatus(s.tree.Roots[0].ID+".1", StatusDone, "")
	if d := s.tree.Doing(); d != nil {
		t.Fatalf("Doing adopted an orphaned sub-agent mark: %+v", d)
	}
}

// TestSetScopeRefusesAnOverlapWithADispatchedNode is spec §3.4: one pen per
// path. Once a node is dispatched the ready rule can no longer stop a
// second sub-agent being pointed at the same files, so SetScope must.
func TestSetScopeRefusesAnOverlapWithADispatchedNode(t *testing.T) {
	s, root := planOwned(t)
	s.SetDispatched(root+".2", true) // big owns internal/scan
	if err := s.SetOwner(root+".3", "big", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetScope(root+".3", []string{"internal"}); err == nil ||
		!strings.Contains(err.Error(), "overlaps "+root+".2") {
		t.Fatalf("an overlapping scope was accepted: %v", err)
	}
	if err := s.SetScope(root+".3", []string{"docs"}); err != nil {
		t.Fatalf("a disjoint scope must still be allowed: %v", err)
	}
	// Widening the dispatched node's own scope is not an overlap with itself.
	if err := s.SetScope(root+".2", []string{"internal/scan", "internal/token"}); err != nil {
		t.Fatalf("a node may still widen its own scope: %v", err)
	}
}

// TestACrashedSubtreeIsSeenAsInterrupted: a crash writes no interruption
// note — nothing ran to write one — so the README's promise that a
// re-dispatched sub-agent comes back "with what was already touched still
// on disk" was only half true: it was not told what it had written. The
// evidence knows, so Steps recovers it.
func TestACrashedSubtreeIsSeenAsInterrupted(t *testing.T) {
	s, root := planOwned(t)
	s.SetDispatched(root+".2", true)
	_ = s.SetStatus(root+".2.1", StatusDoing, "")
	s.ObserveFor(root+".2", Event{Tool: "write_file",
		Args: map[string]any{"path": "internal/scan/a.go", "content": "x"}, Content: "wrote"})
	// No Interrupt call, no note: the session was killed.
	steps := s.Steps()
	sub := steps[0].Children[1]
	if !sub.Interrupted || len(sub.Touched) != 1 || sub.Touched[0] != "internal/scan/a.go" {
		t.Fatalf("a crashed subtree reads as untouched: %+v", sub)
	}
	// An assigned step nothing has written under is not "interrupted".
	if third := steps[0].Children[2]; third.Interrupted {
		t.Fatalf("an untouched step was called interrupted: %+v", third)
	}
}

// TestDelegatingAStartedStepIsNotAnInterruption is the regression the
// crash-recovery rule introduced. `Interrupted` is what resumeSubAgents
// reads to decide a run really was dispatched and cut short, and it CLOSES
// such a node `blocked` when it is not ready — and "not ready" is common
// (`no scope`, `status is doing`, `waiting for 1.1`). The main model
// working a step, writing a file under it and then delegating it leaves
// exactly the shape the rule was keying on, so the delegation would be
// destroyed at the next session start. A dispatch that was cut short
// leaves a doing node strictly BELOW the root; a root that is itself doing
// is the main model's own mark.
func TestDelegatingAStartedStepIsNotAnInterruption(t *testing.T) {
	s := testStore(t)
	s.SetCards(cardsForTest())
	root := s.Plan("port the scanner", []string{"port internal/scan"})
	id := root + ".1"
	// The main model works the step and writes a file under it...
	if err := s.SetStatus(id, StatusDoing, ""); err != nil {
		t.Fatal(err)
	}
	s.Observe(Event{Tool: "write_file",
		Args: map[string]any{"path": "internal/scan/a.go", "content": "x"}, Content: "wrote"})
	// ...and only then delegates it. Nothing was ever dispatched.
	if err := s.SetOwner(id, "big", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetScope(id, []string{"internal/scan"}); err != nil {
		t.Fatal(err)
	}
	if got := s.Touched(id); len(got) == 0 {
		t.Fatal("the fixture must leave a written file under the step")
	}
	step := s.Steps()[0].Children[0]
	if step.ID != id {
		t.Fatalf("fixture: %+v", step)
	}
	if step.Interrupted {
		t.Fatalf("a delegated-but-never-dispatched step reads as interrupted, so resume would close it blocked: %+v", step)
	}
	// The step must survive a restart still assigned and still open.
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenAt(s.dir, s.root, "s1", true, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	s2.SetCards(cardsForTest())
	n := s2.tree.Find(id)
	if n == nil || n.Owner != "big" || n.Status.terminal() {
		t.Fatalf("the delegation did not survive: %+v", n)
	}
	if again := s2.Steps()[0].Children[0]; again.Interrupted {
		t.Fatalf("still interrupted after a reload: %+v", again)
	}
}

// TestDoneByIsNotStampedOnTheMainModelsOwnFinishedWork: CloseAs recurses
// into every descendant, so stamping an already-terminal node there put
// "done by big" on a child the MAIN model had completed before the step was
// ever delegated — which is precisely the line the operator reads to know
// who did what. Attribution now happens at the transition, inside the pen
// that made it.
func TestDoneByIsNotStampedOnTheMainModelsOwnFinishedWork(t *testing.T) {
	s := testStore(t)
	s.SetCards(cardsForTest())
	root := s.Plan("port the scanner", []string{"port internal/scan"})
	id := root + ".1"
	mine, err := s.Add(id, "the main model's own sub-step")
	if err != nil {
		t.Fatal(err)
	}
	theirs, err := s.Add(id, "the sub-agent's sub-step")
	if err != nil {
		t.Fatal(err)
	}
	// The main model finishes its own sub-step before delegating.
	if err := s.SetStatus(mine, StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOwner(id, "big", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetScope(id, []string{"internal/scan"}); err != nil {
		t.Fatal(err)
	}
	s.SetDispatched(id, true)
	// The sub-agent closes its own sub-step with its task tool...
	if err := s.SetStatus(theirs, StatusDone, ""); err != nil {
		t.Fatal(err)
	}
	// ...and the runner closes the subtree on its behalf.
	if err := s.CloseAs(id, "big", "done", ""); err != nil {
		t.Fatal(err)
	}
	if n := s.tree.Find(mine); n.DoneBy != "" {
		t.Fatalf("the main model's own finished sub-step reads %q: %q", "done by "+n.DoneBy, statusLine(n))
	}
	// The sub-agent's own work is still attributed, root included.
	for _, want := range []string{theirs, id} {
		if n := s.tree.Find(want); n.DoneBy != "big" {
			t.Fatalf("%s lost its attribution: %+v", want, n)
		}
	}
}
