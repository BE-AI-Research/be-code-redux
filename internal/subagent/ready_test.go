package subagent

import "testing"

func cards() map[string]Card {
	return map[string]Card{
		"big":    {Name: "big", SubAgent: true},
		"claude": {Name: "claude", SubAgent: true, Online: true, MaxScope: []string{"docs"}},
		"talk":   {Name: "talk", SubAgent: false},
	}
}

func tree() []Step {
	return []Step{{ID: "3", Text: "port", Status: "doing", Children: []Step{
		{ID: "3.1", Text: "list", Status: "done"},
		{ID: "3.2", Text: "port scan", Status: "todo", Owner: "big", Scope: []string{"internal/scan"},
			Children: []Step{{ID: "3.2.1", Status: "todo", Owner: "big", Scope: []string{"internal/scan"}}}},
		{ID: "3.3", Text: "note", Status: "todo", Owner: "claude", Scope: []string{"docs/scanner.md"}, After: []string{"3.1"}},
		{ID: "3.4", Text: "no scope", Status: "todo", Owner: "big"},
		{ID: "3.5", Text: "not a sub-agent", Status: "todo", Owner: "talk", Scope: []string{"x"}},
		{ID: "3.6", Text: "overlaps 3.2", Status: "todo", Owner: "big", Scope: []string{"internal"}},
		{ID: "3.7", Text: "outside max_scope", Status: "todo", Owner: "claude", Scope: []string{"cmd"}},
	}}}
}

func find(cs []Candidate, id string) *Candidate {
	for i := range cs {
		if cs[i].ID == id {
			return &cs[i]
		}
	}
	return nil
}

func TestReadyRules(t *testing.T) {
	ready, waiting := Ready(tree(), cards(), nil)
	if len(ready) != 2 || ready[0].ID != "3.2" || ready[1].ID != "3.3" {
		t.Fatalf("ready: %+v", ready)
	}
	want := map[string]string{
		"3.4": "no scope",
		"3.5": "talk is not a sub-agent",
		"3.6": "scope overlaps 3.2",
		"3.7": "scope is outside claude's max_scope (docs)",
	}
	for id, reason := range want {
		c := find(waiting, id)
		if c == nil || c.Reason != reason {
			t.Fatalf("%s: got %+v, want reason %q", id, c, reason)
		}
	}
	if find(waiting, "3.2.1") != nil || find(ready, "3.2.1") != nil {
		t.Fatal("a child of an assigned subtree is never a candidate")
	}
}

func TestReadyOrderAndRunning(t *testing.T) {
	roots := tree()
	// 3.3 positional rule: without after:, an earlier open sibling blocks it.
	roots[0].Children[2].After = nil
	ready, waiting := Ready(roots, cards(), nil)
	if find(ready, "3.3") != nil {
		t.Fatal("3.3 should wait for 3.2 without after:")
	}
	if c := find(waiting, "3.3"); c == nil || c.Reason != "waiting for 3.2" {
		t.Fatalf("3.3: %+v", c)
	}
	// A running root is not re-dispatched, and its scope still blocks 3.6.
	ready, waiting = Ready(tree(), cards(), map[string]bool{"3.2": true})
	if find(ready, "3.2") != nil {
		t.Fatal("running node offered again")
	}
	if c := find(waiting, "3.6"); c == nil || c.Reason != "scope overlaps 3.2" {
		t.Fatalf("3.6: %+v", c)
	}
}

func TestReadyIgnoresClosedAndMain(t *testing.T) {
	roots := []Step{{ID: "1", Status: "todo", Children: []Step{
		{ID: "1.1", Status: "done", Owner: "big", Scope: []string{"a"}},
		{ID: "1.2", Status: "todo", Scope: []string{"b"}},
		{ID: "1.3", Status: "todo", Owner: "nobody", Scope: []string{"c"}},
	}}}
	ready, waiting := Ready(roots, cards(), nil)
	if len(ready) != 0 {
		t.Fatalf("ready: %+v", ready)
	}
	if c := find(waiting, "1.3"); c == nil || c.Reason != "nobody is not in coworkers" {
		t.Fatalf("1.3: %+v", c)
	}
	if find(waiting, "1.1") != nil || find(waiting, "1.2") != nil {
		t.Fatal("closed or main-owned nodes are not candidates")
	}
}
