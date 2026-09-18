package engine

import (
	"fmt"
	"sort"
	"strings"
)

// This file is the 0.10.0 flat-ledger surface, kept working as a projection
// of the task tree so that the prompt block (Task 4) and the tool and agent
// wiring (Task 5) can each move on their own instead of all at once.
//
// Nothing here holds state: every function reads the tree and shapes it
// into the Ledger/Digest/Step vocabulary the old block and the old task
// tool speak. Tasks 4 and 5 delete this file.

// Ledger is a snapshot of the active task in the old flat shape.
func (s *Store) Ledger() Ledger {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ledgerLocked()
}

func (s *Store) ledgerLocked() Ledger {
	l := Ledger{Baseline: s.baseline, Session: s.session}
	// No open task means no task line: with every root finished — or
	// dropped by "/task clear" — the old block has nothing to describe.
	r := s.activeRootLocked()
	if r == nil {
		return l
	}
	l.Task = r.Text
	for _, c := range r.Children {
		if c.Text == unfiledText {
			continue
		}
		l.Steps = append(l.Steps, Step{Text: c.Text, Status: legacyStatus(c.Status)})
	}
	var walk func(n *Node)
	walk = func(n *Node) {
		for _, nt := range n.Evidence.Notes {
			if nt.Decision {
				l.Decisions = append(l.Decisions, nt.Text)
			} else {
				l.Facts = append(l.Facts, nt.Text)
			}
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(r)
	return l
}

// legacyStatus maps a node status onto the four words the old ledger knew.
// blocked has no old equivalent; it reads as skip, which is at least
// terminal, and Task 4's report is where the distinction comes back.
func legacyStatus(st Status) string {
	switch st {
	case StatusDoing:
		return "doing"
	case StatusDone:
		return "done"
	case StatusDropped, StatusBlocked:
		return "skip"
	}
	return "todo"
}

// Digests flattens every FileRef in the tree into the old per-file shape,
// most recently touched first.
func (s *Store) Digests() []Digest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.digestsLocked()
}

func (s *Store) digestsLocked() []Digest {
	byPath := map[string]*Digest{}
	var order []string
	s.tree.Walk(func(n *Node, _ int) {
		for _, f := range n.Evidence.Files {
			d, ok := byPath[f.Path]
			if !ok {
				d = &Digest{Path: f.Path}
				byPath[f.Path] = d
				order = append(order, f.Path)
			}
			for _, r := range f.Ranges {
				d.Ranges = mergeRange(d.Ranges, r)
			}
			d.Edited = d.Edited || f.Edited
			if f.Turn >= d.Turn {
				d.Turn = f.Turn
				if f.Hash != "" {
					d.Hash = f.Hash
				}
				if len(f.Outline) > 0 {
					d.Outline = append([]string(nil), f.Outline...)
				}
				if f.Note != "" {
					d.Note = f.Note
				}
			}
		}
	})
	out := make([]Digest, 0, len(order))
	for _, p := range order {
		out = append(out, *byPath[p])
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Turn != out[j].Turn {
			return out[i].Turn > out[j].Turn
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// Lookups is the cache's snapshot, newest first.
func (s *Store) Lookups() []Lookup {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Lookup, 0, len(s.lookups))
	for i := len(s.lookups) - 1; i >= 0; i-- {
		out = append(out, s.lookups[i])
	}
	return out
}

// SetPlan replaces the task line and steps (all todo).
func (s *Store) SetPlan(task string, steps []string) { s.Plan(task, steps) }

// SetStep marks 1-based step i of the active task.
func (s *Store) SetStep(i int, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	steps := s.stepsLocked()
	if i < 1 || i > len(steps) {
		return fmt.Errorf("step %d does not exist (%d steps)", i, len(steps))
	}
	st, ok := parseStatus(status)
	if !ok {
		return fmt.Errorf("status must be doing, done or skip")
	}
	n := steps[i-1]
	if st == StatusDoing {
		if prev := s.tree.Doing(); prev != nil && prev != n {
			s.rec.distill(prev)
		}
	}
	s.tree.SetStatus(n.ID, st, "")
	if st.terminal() {
		s.rec.distill(n)
	}
	s.markDirtyLocked()
	return nil
}

// stepsLocked is the active task's own steps, without the unfiled node the
// recorder may have opened under it.
func (s *Store) stepsLocked() []*Node {
	r := s.activeRootLocked()
	if r == nil {
		return nil
	}
	var out []*Node
	for _, c := range r.Children {
		if c.Text == unfiledText {
			continue
		}
		out = append(out, c)
	}
	return out
}

// AddNote records a fact or decision against whatever is doing.
func (s *Store) AddNote(text, file string, decision, keep bool) error {
	return s.Note("", text, file, decision, keep)
}

// EnsureTask opens a task from the user's message when none is open.
func (s *Store) EnsureTask(text string) { s.EnsureRoot(text) }

// StartTask refreshes the task line for a new request: always when no task
// is open, and otherwise only when no step is in progress and none is still
// to do. A plan the model is part-way through keeps its own task line, so
// the block does not start describing a side question as the task.
func (s *Store) StartTask(text string) {
	line := firstLine(text, 200)
	if line == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.activeRootLocked(); r != nil {
		for _, c := range r.Children {
			if c.Text == unfiledText {
				continue
			}
			if c.Status == StatusDoing || c.Status == StatusTodo {
				return
			}
		}
		if r.Text != line {
			r.Text = line
			s.markDirtyLocked()
		}
		return
	}
	s.tree.Add("", line)
	s.markDirtyLocked()
}

// StoppedAt is the text of the node in progress, or "".
func (s *Store) StoppedAt() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.tree.Doing()
	if d == nil || d.Text == unfiledText {
		return ""
	}
	return d.Text
}

// ApplyFileNotes stores "path — note" lines from a compaction summary's
// files: block against files the record already knows.
func (s *Store) ApplyFileNotes(block string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range fileNoteLine.FindAllStringSubmatch(block, -1) {
		path := relPath(m[1])
		note := strings.TrimSpace(m[2])
		s.tree.Walk(func(n *Node, _ int) {
			for i := range n.Evidence.Files {
				if n.Evidence.Files[i].Path == path {
					n.Evidence.Files[i].Note = note
					s.markDirtyLocked()
				}
			}
		})
	}
}
