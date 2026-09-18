package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// migrateLedger lifts a 0.10.0 flat ledger into one task. Steps become
// children, decisions and facts become notes on the task, digests and
// lookups become its evidence, and the three legacy files are removed so
// this runs exactly once. Nothing is discarded.
//
// A failure here is handled by the caller, which renames the whole store
// directory aside and starts fresh: a half-converted store is worse than
// none, because it reads as complete.
func (s *Store) migrateLedger(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var l Ledger
	if err := json.Unmarshal(b, &l); err != nil {
		return err
	}

	task := strings.TrimSpace(l.Task)
	if task == "" && len(l.Steps) == 0 && len(l.Decisions) == 0 && len(l.Facts) == 0 {
		// An empty ledger is not a task; there is nothing to lift.
		return s.dropLegacy(path)
	}
	if task == "" {
		task = "work carried over from an earlier session"
	}
	n := s.tree.Add("", firstLine(task, 200))
	for _, st := range l.Steps {
		if strings.TrimSpace(st.Text) == "" {
			continue
		}
		c := s.tree.Add(n.ID, st.Text)
		c.Status = migratedStatus(st.Status)
		if c.Status == StatusDropped {
			c.Reason = "skipped in a previous session"
		}
		if c.Status.terminal() {
			c.Closed = c.Opened
		}
	}
	for _, d := range l.Decisions {
		if d = strings.TrimSpace(d); d != "" {
			n.Evidence.Notes = append(n.Evidence.Notes, NoteRef{Text: d, Decision: true})
		}
	}
	for _, f := range l.Facts {
		if f = strings.TrimSpace(f); f != "" {
			n.Evidence.Notes = append(n.Evidence.Notes, NoteRef{Text: f})
		}
	}
	if l.Baseline != (Baseline{}) {
		s.baseline = l.Baseline
	}

	var digests []Digest
	loadJSON(filepath.Join(s.dir, "digests.json"), &digests)
	for _, d := range digests {
		if d.Path == "" {
			continue
		}
		n.Evidence.Files = append(n.Evidence.Files, FileRef{
			Path: d.Path, Hash: d.Hash, Ranges: d.Ranges,
			Edited: d.Edited, Note: d.Note, Outline: d.Outline, Turn: d.Turn,
		})
		if d.Turn > s.turn {
			s.turn = d.Turn
		}
	}
	var lookups []Lookup
	loadJSON(filepath.Join(s.dir, "lookups.json"), &lookups)
	for _, lk := range lookups {
		n.Evidence.Lookups = append(n.Evidence.Lookups, LookupRef{
			Tool: lk.Tool, Query: summariseKey(strings.TrimPrefix(lk.Query, lk.Tool+" ")), Hits: lk.Hits,
		})
		if lk.Turn > s.turn {
			s.turn = lk.Turn
		}
	}

	s.markDirtyLocked()
	return s.dropLegacy(path)
}

// dropLegacy removes the three 0.10.0 files. Only the ledger's removal can
// fail the migration: it is what decides whether this runs again.
func (s *Store) dropLegacy(ledger string) error {
	os.Remove(filepath.Join(s.dir, "digests.json"))
	os.Remove(filepath.Join(s.dir, "lookups.json"))
	return os.Remove(ledger)
}

func migratedStatus(s string) Status {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "doing":
		return StatusDoing
	case "done":
		return StatusDone
	case "skip":
		return StatusDropped
	}
	return StatusTodo
}
