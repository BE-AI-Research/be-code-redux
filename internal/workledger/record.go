// Package workledger records what each running session is working on, and
// notices when a write would overwrite a change that arrived since this
// session read the file.
//
// Everything here is advisory and nothing here may fail a session: a record
// that cannot be written or read costs a notice, never a run. It is not a
// lock — a lock that outlives a crashed process is worse than a lost update,
// and BE-Code already chose advisory records plus pid liveness for live
// sessions (internal/live/record.go). This follows that.
package workledger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/live"
)

// Alive is how a record's process is checked. A var so tests can stub it.
//
// processAlive is this package's per-platform pair (alive_unix.go,
// alive_windows.go), the same shape internal/live and internal/ide carry.
// Never os.FindProcess: it asks for PROCESS_QUERY_INFORMATION, which a
// non-elevated terminal is refused for an elevated SSH login's process, and
// live.List once deleted a live record that way and the launch forked a copy
// onto a copy of the session. "Access denied" on Windows, and EPERM on Unix,
// both count as alive.
var Alive = processAlive

// StaleAfter is how long a record may go untouched before it is suspect. A
// live pid plus an old record may be a reused pid rather than a working
// session, and this package cannot prove which.
var StaleAfter = time.Hour

// Record is one running session's work. Written at turn boundaries, not per
// tool call: the only consumers are another session's startup notice and a
// conflict prompt's detail, both read at human timescales.
type Record struct {
	PID       int      `json:"pid"`
	Started   string   `json:"started"`
	Updated   string   `json:"updated"`
	Workspace string   `json:"workspace"`
	Branch    string   `json:"branch,omitempty"`
	Session   string   `json:"session,omitempty"`
	Model     string   `json:"model,omitempty"`
	Task      string   `json:"task,omitempty"`
	Writing   []string `json:"writing,omitempty"`

	// Suspect is set by Others, never stored: the record went untouched for
	// longer than StaleAfter while its pid is alive, so the pid may have been
	// reused and Writing cannot be trusted.
	Suspect bool `json:"-"`
}

// Dir is ~/.be-code/work, created if it is missing.
func Dir() (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(base, "work")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}

// Path is where this record lives. The pid alone would collide after a reuse;
// the start time makes each run's record its own.
func (r Record) Path(dir string) string {
	stamp := strings.NewReplacer(":", "", "-", "").Replace(r.Started)
	return filepath.Join(dir, fmt.Sprintf("%d-%s.json", r.PID, stamp))
}

// Save writes the record, 0600: it names a workspace path and what somebody
// is working on. Through a temp file and a rename, as everything in the
// dotdir is, so a crash cannot leave half a record.
func Save(dir string, r Record) error {
	if r.Updated == "" {
		r.Updated = time.Now().UTC().Format(time.RFC3339)
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := r.Path(dir)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Others is every live session working in this workspace apart from selfPID,
// pruning the records of processes that are gone. A missing directory, an
// unreadable record or a corrupt one is simply not a session.
func Others(dir, workspace string, selfPID int) []Record {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Record
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var r Record
		if err := json.Unmarshal(b, &r); err != nil || r.PID == 0 {
			os.Remove(p) // not a record; nothing reads it again
			continue
		}
		if !Alive(r.PID) {
			os.Remove(p)
			continue
		}
		if r.PID == selfPID {
			continue
		}
		// live.SameWorkspace, never ==: two shells spell one folder
		// differently (case on Windows, a symlink, an 8.3 name), and a miss
		// here reports no other work when there is some.
		if !live.SameWorkspace(r.Workspace, workspace) {
			continue
		}
		if t, perr := time.Parse(time.RFC3339, r.Updated); perr == nil && time.Since(t) > StaleAfter {
			// A live pid and an old record may be a reused pid. Say so, and
			// drop the claim about what it is writing rather than reporting
			// work nobody is doing.
			r.Suspect = true
			r.Writing = nil
		}
		out = append(out, r)
	}
	return out
}

// Remove takes this session's own record, named by pid and start time so a
// session can never remove another's.
func Remove(dir string, pid int, started string) error {
	err := os.Remove(Record{PID: pid, Started: started}.Path(dir))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
