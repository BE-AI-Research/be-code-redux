package subagent

import (
	"fmt"
	"strings"
)

// Ready applies the six rules of spec §2.1 to a tree and returns, in id
// order, the assigned steps that may be dispatched now and the assigned
// steps that may not, each with the reason /agents shows. Only the top of
// an assigned subtree is a candidate; running names roots already
// dispatched, which are skipped but still hold their scope.
func Ready(roots []Step, cards map[string]Card, running map[string]bool) (ready, waiting []Candidate) {
	closed := map[string]bool{}
	var index func(s Step)
	index = func(s Step) {
		closed[s.ID] = s.Status == "done" || s.Status == "dropped"
		for _, c := range s.Children {
			index(c)
		}
	}
	for _, r := range roots {
		index(r)
	}
	// Scopes that are taken: every running root's, then every candidate
	// earlier in document order that is itself ready or running.
	taken := map[string][]string{}
	var walk func(s Step, siblings []Step, i int, inherited string)
	walk = func(s Step, siblings []Step, i int, inherited string) {
		if inherited != "" {
			return // inside an assigned subtree: the sub-agent's own steps
		}
		if s.Owner != "" && !closed[s.ID] && s.Status != "blocked" {
			if running[s.ID] {
				taken[s.ID] = s.Scope
			} else {
				c := Candidate{ID: s.ID, Owner: s.Owner}
				card, known := cards[s.Owner]
				switch {
				case !known:
					c.Reason = s.Owner + " is not in coworkers"
				case !card.SubAgent:
					c.Reason = s.Owner + " is not a sub-agent"
				case len(s.Scope) == 0:
					c.Reason = "no scope"
				case !Within(s.Scope, card.MaxScope):
					c.Reason = fmt.Sprintf("scope is outside %s's max_scope (%s)", s.Owner, strings.Join(card.MaxScope, ", "))
				case s.Status != "todo":
					c.Reason = "status is " + s.Status
				}
				if c.Reason == "" {
					for id, sc := range taken {
						if Overlap(s.Scope, sc) {
							c.Reason = "scope overlaps " + id
							break
						}
					}
				}
				if c.Reason == "" {
					if len(s.After) > 0 {
						for _, dep := range s.After {
							if !closed[dep] {
								c.Reason = "waiting for " + dep
								break
							}
						}
					} else {
						for j := 0; j < i; j++ {
							if !closed[siblings[j].ID] {
								c.Reason = "waiting for " + siblings[j].ID
								break
							}
						}
					}
				}
				if c.Reason == "" {
					ready = append(ready, c)
					taken[s.ID] = s.Scope
				} else {
					waiting = append(waiting, c)
				}
			}
		}
		// Children of an owned node inherit that owner, so the guard above
		// skips them (never a candidate of their own); children of a
		// main-owned node (s.Owner == "") keep inherited == "" and are
		// examined in their own right.
		for j, c := range s.Children {
			walk(c, s.Children, j, s.Owner)
		}
	}
	for i, r := range roots {
		walk(r, roots, i, "")
	}
	return ready, waiting
}
