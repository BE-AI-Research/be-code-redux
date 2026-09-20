package engine

import (
	"encoding/json"
	"errors"
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

// unsavedStateError marks a migration whose task documents were written and
// whose dotdir state was not (ruling T3-f). The lift itself is on disk, so
// the store is usable and the session continues; what was lost is the turn
// counter and the baseline that the same flush would have persisted, and a
// later flush in this session may still write them.
type unsavedStateError struct{ err error }

func (e unsavedStateError) Error() string { return e.err.Error() }
func (e unsavedStateError) Unwrap() error { return e.err }

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

	// Everything that follows is undone on a flush that failed *before* it
	// wrote any document, so the store is left exactly as it was found. Once
	// a document is on disk the lift has happened and undoing is the wrong
	// move — see the flush below.
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
	// The lift's own document, by name. Decided before the flush and not
	// after: Flush only adopts the names it chose once it has succeeded, so
	// asking again after a failure gives this same answer, but asking first
	// does not depend on that.
	own := s.docNamesLocked()[roots]
	// Write the lifted tree out now. Until this succeeds the migration
	// exists only in memory, and a normal session does not flush until a
	// request completes — so a Ctrl-C at the prompt would otherwise lose it.
	if err := s.Flush(); err != nil {
		var fe flushError
		// The question is whether the lift's *own* document reached the disk,
		// and the only honest way to ask it is by name. Counting writes
		// answered it only while documents went out in root order with none
		// skipped; a root that gets no document (a spent unfiled one) or one
		// written out of order (a split root, ruling T3-g) made the count
		// name the wrong document — and a wrong "yes" here is the duplicate
		// migration that took five rounds to close.
		if errors.As(err, &fe) && fe.wrote[own] {
			// Ruling T3-f. The lifted task's own document is written, so
			// the lift has happened; only the dotdir state (or a document
			// after this one) failed to persist. Restoring
			// the legacy files here would put ledger.json back beside the
			// document it was lifted into, and the next open would load the
			// document *and* migrate the ledger again — two roots and two
			// documents for one task, the duplicate the rename-first
			// ordering exists to prevent. So nothing is put back and
			// nothing is taken away: the aside stays aside, the documents
			// stay written, the in-memory tree (with the turn and baseline
			// the digests raised) stays as it is, and the failure is
			// reported. The next open reads the documents, finds no
			// ledger.json, and neither duplicates nor re-migrates.
			return unsavedStateError{err}
		}
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
			if !os.IsNotExist(err) {
				// Not "there is no such file" but "this file cannot be
				// answered for" — a symlink loop, an unreadable directory.
				// Skipping it would leave the 0.10.0 store half moved, one
				// file aside and another still in place, so the migration
				// stops here and whatever moved already goes back.
				restoreLegacy(moved)
				return nil, err
			}
			continue
		}
		// os.Rename replaces the destination, and the stamp is only a
		// second wide, so two migrations in the same second would destroy
		// the first one's copy. Nothing is deleted here (ruling T3-d), so
		// the name has to be free before we use it.
		to, err := freeName(from + ".migrated-" + at)
		if err != nil {
			restoreLegacy(moved)
			return nil, err
		}
		if err := os.Rename(from, to); err != nil {
			restoreLegacy(moved)
			return nil, err
		}
		moved = append(moved, movedFile{from: from, to: to})
	}
	return moved, nil
}

// freeName is path, or path-2, path-3 … — the first that nothing occupies.
// A stat that fails for any other reason than "not there" is not an answer:
// treating it as "occupied" would loop over an unreadable directory forever,
// so it stops the migration instead and the legacy files stay where they are.
func freeName(path string) (string, error) {
	cand := path
	for n := 2; ; n++ {
		_, err := os.Stat(cand)
		if err != nil && !os.IsNotExist(err) {
			return "", err
		}
		if err != nil {
			return cand, nil
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
