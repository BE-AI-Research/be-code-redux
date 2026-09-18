package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// legacyFiles are the three 0.10.0 store files, in the order they are moved.
var legacyFiles = []string{"ledger.json", "digests.json", "lookups.json"}

// movedFile is one legacy file and where it was moved to, so a failed
// migration can put it back exactly where it was.
type movedFile struct{ from, to string }

// retryableError marks a migration that failed after the store had been put
// back exactly as it was. Nothing was converted, so the next open simply
// tries again and the caller does not need its blunt rename-the-whole-store
// net.
type retryableError struct{ err error }

func (e retryableError) Error() string { return e.err.Error() }
func (e retryableError) Unwrap() error { return e.err }

// migrateLedger lifts a 0.10.0 flat ledger into one task. Steps become
// children, decisions and facts become notes on the task, and digests and
// lookups become its evidence. Nothing is discarded.
//
// The legacy files are renamed aside *before* anything is written, which is
// what makes this run once: "has this been migrated?" becomes a fact about
// the filesystem — no ledger.json, no migration — rather than a flag written
// after the documents, which a kill in between could leave lagging behind
// the tree it describes. And they are renamed, never removed (ruling T3-d),
// so a ledger.json that turns up later — a downgrade, a restored backup — is
// simply one that has not been migrated yet, and is lifted like any other
// instead of being deleted unread.
func (s *Store) migrateLedger(path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var l Ledger
	if err := json.Unmarshal(b, &l); err != nil {
		return err
	}
	var digests []Digest
	loadJSON(filepath.Join(s.dir, "digests.json"), &digests)
	var lookups []Lookup
	loadJSON(filepath.Join(s.dir, "lookups.json"), &lookups)

	moved, err := s.setLegacyAside()
	if err != nil {
		return retryableError{err}
	}

	// Everything that follows is undone on a failed flush, so the store is
	// left exactly as it was found.
	roots, turn, baseline := len(s.tree.Roots), s.turn, s.baseline
	undo := func(err error) error {
		s.tree.Roots = s.tree.Roots[:roots]
		s.turn, s.baseline = turn, baseline
		restoreLegacy(moved)
		return retryableError{err}
	}

	task := strings.TrimSpace(l.Task)
	if task == "" && len(l.Steps) == 0 && len(l.Decisions) == 0 && len(l.Facts) == 0 {
		// An empty ledger is not a task; there is nothing to lift, and the
		// files are already safely aside.
		return nil
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
	for _, lk := range lookups {
		n.Evidence.Lookups = append(n.Evidence.Lookups, LookupRef{
			Tool: lk.Tool, Query: summariseKey(strings.TrimPrefix(lk.Query, lk.Tool+" ")), Hits: lk.Hits,
		})
		if lk.Turn > s.turn {
			s.turn = lk.Turn
		}
	}

	s.markDirtyLocked()
	// Write the lifted tree out now. Until this succeeds the migration
	// exists only in memory, and a normal session does not flush until a
	// request completes — so a Ctrl-C at the prompt would otherwise lose it.
	if err := s.Flush(); err != nil {
		return undo(err)
	}
	return nil
}

// setLegacyAside renames the 0.10.0 files out of the way under one stamp.
// Nothing is deleted: the old store stays readable next to the new one.
func (s *Store) setLegacyAside() ([]movedFile, error) {
	at := stamp()
	var moved []movedFile
	for _, name := range legacyFiles {
		from := filepath.Join(s.dir, name)
		if _, err := os.Stat(from); err != nil {
			continue
		}
		// os.Rename replaces the destination, and the stamp is only a
		// second wide, so two migrations in the same second would destroy
		// the first one's copy. Nothing is deleted here (ruling T3-d), so
		// the name has to be free before we use it.
		to := freeName(from + ".migrated-" + at)
		if err := os.Rename(from, to); err != nil {
			restoreLegacy(moved)
			return nil, err
		}
		moved = append(moved, movedFile{from: from, to: to})
	}
	return moved, nil
}

// freeName is path, or path-2, path-3 … — the first that nothing occupies.
func freeName(path string) string {
	cand := path
	for n := 2; ; n++ {
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
		cand = fmt.Sprintf("%s-%d", path, n)
	}
}

func restoreLegacy(moved []movedFile) {
	for _, m := range moved {
		os.Rename(m.to, m.from)
	}
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
