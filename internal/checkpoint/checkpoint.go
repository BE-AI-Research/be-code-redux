// Package checkpoint snapshots files before the agent modifies them,
// giving BE-Code turn-level undo. Weaker local models take more wrong
// turns than frontier models, so cheap rollback is a core safety feature,
// not a nicety.
//
// Scope: file changes made through the agent's write/edit tools. Shell
// side effects are outside the snapshot (use git for those).
package checkpoint

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// fileState remembers a file's condition before its first change in a turn.
type fileState struct {
	existed    bool
	backupPath string
	mode       os.FileMode
}

// turn groups all files touched during one agent request.
type turn struct {
	label string
	dir   string
	files map[string]*fileState // workspace-relative path -> state
	order []string
}

// Checkpointer manages snapshots for one session.
type Checkpointer struct {
	root  string // workspace root
	base  string // backup storage dir
	turns []*turn
	cur   *turn
}

// New creates a checkpointer storing backups under baseDir (e.g.
// ~/.be-code/checkpoints/<session-id>).
func New(workspaceRoot, baseDir string) (*Checkpointer, error) {
	if err := os.MkdirAll(baseDir, 0o700); err != nil {
		return nil, err
	}
	return &Checkpointer{root: workspaceRoot, base: baseDir}, nil
}

// BeginTurn opens a new snapshot group. Empty turns are discarded.
func (c *Checkpointer) BeginTurn(label string) {
	if c == nil {
		return
	}
	if c.cur != nil && len(c.cur.files) == 0 {
		c.turns = c.turns[:len(c.turns)-1] // drop the empty one
	}
	t := &turn{
		label: label,
		dir:   filepath.Join(c.base, fmt.Sprintf("t%03d-%s", len(c.turns), time.Now().Format("150405"))),
		files: map[string]*fileState{},
	}
	c.turns = append(c.turns, t)
	c.cur = t
}

// Record snapshots absPath before its first modification in this turn.
// Safe to call repeatedly; only the first call per turn per file stores.
func (c *Checkpointer) Record(absPath string) error {
	if c == nil || c.cur == nil {
		return nil
	}
	rel, err := filepath.Rel(c.root, absPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil // outside workspace; not ours to track
	}
	if _, done := c.cur.files[rel]; done {
		return nil
	}
	st := &fileState{}
	info, statErr := os.Stat(absPath)
	if statErr == nil {
		st.existed = true
		st.mode = info.Mode()
		st.backupPath = filepath.Join(c.cur.dir, rel)
		if err := os.MkdirAll(filepath.Dir(st.backupPath), 0o700); err != nil {
			return err
		}
		data, err := os.ReadFile(absPath)
		if err != nil {
			return err
		}
		if err := os.WriteFile(st.backupPath, data, 0o600); err != nil {
			return err
		}
	}
	c.cur.files[rel] = st
	c.cur.order = append(c.cur.order, rel)
	return nil
}

// ChangedLast lists workspace-relative paths touched in the most recent
// non-empty turn (used for reviewer routing and /undo display).
func (c *Checkpointer) ChangedLast() []string {
	t := c.lastNonEmpty()
	if t == nil {
		return nil
	}
	return append([]string(nil), t.order...)
}

// ChangedAll lists every path touched this session (deduplicated).
func (c *Checkpointer) ChangedAll() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range c.turns {
		for _, rel := range t.order {
			if !seen[rel] {
				seen[rel] = true
				out = append(out, rel)
			}
		}
	}
	return out
}

func (c *Checkpointer) lastNonEmpty() *turn {
	for i := len(c.turns) - 1; i >= 0; i-- {
		if len(c.turns[i].files) > 0 {
			return c.turns[i]
		}
	}
	return nil
}

// Undo restores the most recent non-empty turn's files and pops it.
// Returns the restored paths.
func (c *Checkpointer) Undo() ([]string, error) {
	t := c.lastNonEmpty()
	if t == nil {
		return nil, fmt.Errorf("nothing to undo")
	}
	var restored []string
	var firstErr error
	for rel, st := range t.files {
		abs := filepath.Join(c.root, rel)
		var err error
		if st.existed {
			var data []byte
			data, err = os.ReadFile(st.backupPath)
			if err == nil {
				mode := st.mode
				if mode == 0 {
					mode = 0o644
				}
				err = os.WriteFile(abs, data, mode.Perm())
			}
		} else {
			err = os.Remove(abs)
			if os.IsNotExist(err) {
				err = nil
			}
		}
		if err != nil && firstErr == nil {
			firstErr = fmt.Errorf("restoring %s: %w", rel, err)
		}
		if err == nil {
			restored = append(restored, rel)
		}
	}
	// Pop the turn.
	for i := len(c.turns) - 1; i >= 0; i-- {
		if c.turns[i] == t {
			c.turns = append(c.turns[:i], c.turns[i+1:]...)
			break
		}
	}
	if c.cur == t {
		c.cur = nil
	}
	_ = os.RemoveAll(t.dir)
	return restored, firstErr
}

// Depth reports how many undoable turns exist.
func (c *Checkpointer) Depth() int {
	n := 0
	for _, t := range c.turns {
		if len(t.files) > 0 {
			n++
		}
	}
	return n
}

// Cleanup removes all backup storage for this session.
func (c *Checkpointer) Cleanup() {
	if c != nil {
		_ = os.RemoveAll(c.base)
	}
}
