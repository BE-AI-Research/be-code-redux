package workledger

import (
	"sync"

	"context"
	"fmt"
	"github.com/brown-enterprises/be-code/internal/gitctx"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Options is what a session tells the ledger about itself.
type Options struct {
	Enabled         bool
	KeepOverwritten time.Duration
	PID             int
	Session         string
	Model           string
}

// Ledger is one session's view: its own record, what it has seen, and the
// decision when a write collides.
//
// Nothing here may fail a session. A record that cannot be written costs the
// notice and nothing else; detection carries on in memory.
type Ledger struct {
	// mu guards rec and lastTouch: the ledger is reached from the agent
	// goroutine and from tool dispatch.
	mu        sync.Mutex
	lastTouch time.Time

	opt     Options
	ws      string
	dir     string
	rec     Record
	seen    *Seen
	warned  bool
	pending string       // a notice from before Warn existed
	Warn    func(string) // where a "could not write the record" notice goes
}

// Open starts this session's ledger. A disabled ledger is inert: no record,
// no detection, no notice — the behaviour before this feature.
func Open(workspace string, opt Options) (*Ledger, error) {
	l := &Ledger{opt: opt, ws: workspace}
	if !opt.Enabled {
		return l, nil
	}
	dir, err := Dir()
	if err != nil {
		// Not fatal: in-memory detection still works, and the caller is told
		// once, through SetWarn, as soon as there is somewhere to say it.
		l.seen = NewSeen(filepath.Join(os.TempDir(), fmt.Sprintf("be-code-seen-%d.json", opt.PID)))
		l.note("no record of this session's work: " + err.Error())
		return l, nil
	}
	l.dir = dir
	l.rec = Record{
		PID:       opt.PID,
		Started:   time.Now().UTC().Format(time.RFC3339),
		Workspace: workspace,
		Session:   opt.Session,
		Model:     opt.Model,
	}
	// The branch is worth one subprocess at startup: without it the notice
	// can only say a pid, which §14 names as the thing that makes it
	// ignorable. Task and writing need a turn boundary and are not set here.
	l.rec.Branch = currentBranch(workspace)
	l.seen = NewSeen(strings.TrimSuffix(l.rec.Path(dir), ".json") + ".seen.json")
	if err := Save(dir, l.rec); err != nil {
		l.note("could not record this session's work: " + err.Error())
	}
	if opt.KeepOverwritten > 0 {
		// Beside each overwritten file, not here: no sidecar is ever written
		// into ~/.be-code/work, so pruning this directory did nothing.
		KeepSidecarsFor = opt.KeepOverwritten
	}
	return l, nil
}

// note says something once. The pending string matters: Warn is assigned by
// the caller *after* Open returns, so a failure during Open had nowhere to go
// and was dropped — and the early return before setting warned meant it was
// not even remembered as said.
func (l *Ledger) note(s string) {
	if l.warned {
		return
	}
	l.warned = true // one notice, not one per attempt
	if l.Warn == nil {
		l.pending = s // delivered by SetWarn, once there is somewhere to put it
		return
	}
	l.Warn(s)
}

// SetWarn gives the ledger somewhere to put a notice, and delivers anything
// Open could not report at the time.
func (l *Ledger) SetWarn(f func(string)) {
	l.Warn = f
	if l.pending != "" && f != nil {
		f(l.pending)
		l.pending = ""
	}
}

// Others is every other live session in this workspace.
func (l *Ledger) Others() []Record {
	if !l.opt.Enabled || l.dir == "" {
		return nil
	}
	return Others(l.dir, l.ws, l.opt.PID)
}

// NoticeLine is said once at startup, and only when there is something to
// say. It names what the other session is *doing*, because it competes with
// every other startup notice and "a session exists" is not worth reading.
func (l *Ledger) NoticeLine() string {
	others := l.Others()
	if len(others) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("another session is working in this workspace:")
	if len(others) > 1 {
		b.Reset()
		fmt.Fprintf(&b, "%d other sessions are working in this workspace:", len(others))
	}
	for _, o := range others {
		fmt.Fprintf(&b, "\n  pid %d", o.PID)
		if o.Branch != "" {
			fmt.Fprintf(&b, " on %s", o.Branch)
		}
		if o.Task != "" {
			fmt.Fprintf(&b, ", task %s", o.Task)
		}
		if o.Suspect {
			b.WriteString(" (its record is stale; the process may be unrelated)")
		}
	}
	return b.String()
}

// OnRead records what this session last saw at path, and doubles as the
// heartbeat: Updated was written once, at Open, so with StaleAfter at an hour
// every genuinely live session older than that was marked Suspect and
// announced as possibly-unrelated. Reading a file is the plainest evidence a
// session is working.
func (l *Ledger) OnRead(path string, content []byte) {
	if !l.opt.Enabled || l.seen == nil {
		return
	}
	l.seen.Note(path, content)
	l.touch()
}

// heartbeat is how often activity rewrites the record. A record is read at
// human timescales — a startup notice and a conflict's detail — so a write
// per file read would be disk traffic inside the tool loop for nothing.
const heartbeat = time.Minute

// touch refreshes Updated, at most once per heartbeat.
func (l *Ledger) touch() {
	if l.dir == "" {
		return
	}
	l.mu.Lock()
	if time.Since(l.lastTouch) < heartbeat {
		l.mu.Unlock()
		return
	}
	l.lastTouch = time.Now()
	rec := l.rec
	rec.Updated = time.Now().UTC().Format(time.RFC3339)
	l.rec = rec
	dir := l.dir
	l.mu.Unlock()
	if err := Save(dir, rec); err != nil {
		l.note("could not update this session's work record: " + err.Error())
	}
}

// currentBranch is the workspace's branch, or "" outside a repository. One
// subprocess, at startup only.
func currentBranch(workspace string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if !gitctx.IsRepo(ctx, workspace) {
		return ""
	}
	return gitctx.Branch(ctx, workspace)
}

// NoteHash records a hash computed elsewhere — the engine's, for a task
// document it already hashed. Feeding its hash rather than its bytes is
// deliberate: two hashes of one file that could disagree is worse than one,
// and the engine's is the one it acts on.
func (l *Ledger) NoteHash(path, hash string) {
	if !l.opt.Enabled || l.seen == nil {
		return
	}
	l.seen.NoteHash(path, hash)
}

// OnConflict is the tools hook: has path changed since this session read it,
// and if so what happens. true means write.
func (l *Ledger) OnConflict(ctx context.Context, path string, ask Asker, who Attendance, notice func(string)) (bool, error) {
	if !l.opt.Enabled || l.seen == nil {
		return true, nil
	}
	changed, theirs, err := l.seen.Changed(path)
	if err != nil || !changed {
		// An unreadable file is the write's own problem to report, not a
		// conflict to prompt about.
		return true, nil
	}
	d, rerr := Resolve(ctx, Conflict{
		Path: path, Workspace: l.ws, Theirs: theirs, Others: l.Others(),
	}, ask, who, notice)
	if rerr != nil {
		return false, rerr
	}
	if d == KeepTheirs {
		// Their version is what this session has now seen, so the next write
		// is judged against it rather than prompting again about the same
		// change.
		l.seen.Note(path, theirs)
		return false, nil
	}
	return true, nil
}

// Turn updates the record at a turn boundary. Not per tool call: the only
// consumers read it at human timescales, and a record rewritten inside the
// tool loop is a disk write for nothing.
func (l *Ledger) Turn(branch, task string, writing []string) {
	if !l.opt.Enabled || l.dir == "" {
		return
	}
	l.rec.Branch, l.rec.Task, l.rec.Writing = branch, task, writing
	l.rec.Updated = time.Now().UTC().Format(time.RFC3339)
	if err := Save(l.dir, l.rec); err != nil {
		l.note("could not update this session's work record: " + err.Error())
	}
}

// Close removes this session's record and persists what it saw, so a resumed
// session does not treat every file as unseen.
func (l *Ledger) Close() error {
	if !l.opt.Enabled {
		return nil
	}
	if l.seen != nil {
		_ = l.seen.Flush()
	}
	if l.dir == "" {
		return nil
	}
	return Remove(l.dir, l.rec.PID, l.rec.Started)
}
