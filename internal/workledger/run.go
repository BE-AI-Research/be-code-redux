package workledger

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// Run is one sub-agent run's own view of what it read, kept separately from
// the session's.
//
// The session's Seen is shared with the primary, so if the primary rewrites a
// file a sub-agent had read, Seen is updated by that write and reports "no
// change" — true of the session, and exactly wrong for the sub-agent, whose
// summary was written against the older version. The primary's own writes are
// the case where a hand-back's advice is most likely to be stale, and the
// sub-agent has no way to tell who moved the file.
//
// A nil *Run is usable and does nothing: a sub-agent can be configured with
// the work ledger disabled.
type Run struct {
	mu   sync.Mutex
	ws   string
	read map[string]string // path → hash as this run last saw it
}

// NewRun starts tracking a run's reads, relative to workspace for reporting.
func NewRun(workspace string) *Run {
	return &Run{ws: workspace, read: map[string]string{}}
}

// Read records what the run saw at path.
func (r *Run) Read(path string, content []byte) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.read[path]; !ok {
		// The *first* read is what the run's reasoning rests on. A later read
		// of a file that already moved must not quietly become the baseline.
		r.read[path] = HashBytes(content)
	}
}

// Wrote records that the run itself is changing path, so its own work is not
// reported back to it as drift. The registry reports a write as OnWrote(path)
// followed by OnRead(path, what is on disk after post_write hooks), so this
// forgets the baseline and that read sets it to the run's own result. A path
// the run only wrote and never read back has no baseline and is never drift.
func (r *Run) Wrote(path string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.read, path)
}

// Drifted is every file this run read that has changed since it read it,
// workspace-relative and sorted. A file removed underneath the run counts.
func (r *Run) Drifted() []string {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	snapshot := make(map[string]string, len(r.read))
	for k, v := range r.read {
		snapshot[k] = v
	}
	ws := r.ws
	r.mu.Unlock()

	var out []string
	for path, was := range snapshot {
		b, err := os.ReadFile(path)
		switch {
		case err != nil && !os.IsNotExist(err):
			continue // unreadable now is not evidence of a change
		case err != nil: // removed underneath the run
		case HashBytes(b) == was:
			continue
		}
		rel := path
		if rp, rerr := filepath.Rel(ws, path); rerr == nil && !strings.HasPrefix(rp, "..") {
			rel = filepath.ToSlash(rp)
		}
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}
