package engine

import (
	"strings"
	"testing"

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
	if err := s.Interrupt(root+".2", "interrupted 10:00 after 1 tool calls; files written: internal/scan/a.go"); err != nil {
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
