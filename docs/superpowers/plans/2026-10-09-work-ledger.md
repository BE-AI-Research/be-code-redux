# Work Ledger (sub-project A) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A session records what it is working on so other sessions can see it,
and a write that would overwrite a change which arrived since this session
read the file stops and asks — or, with nobody to ask, writes after making the
old version restorable.

**Architecture:** One new leaf package, `internal/workledger`: the per-session
record under `~/.be-code/work/`, hash-based change detection modelled on the
hash `internal/engine` already keeps for task documents, and restore points
through `gitctx` for workspace files and timestamped sidecars for the dotdir.
`internal/tools` reaches it through an injected hook, never an import, the way
`ReviewerFactory` and `LoaderFactory` already avoid that direction.

**Tech Stack:** Go, stdlib only, plus the repo's own `internal/gitctx`,
`internal/procattr` and `internal/live`. No new module dependencies.

**Spec:** `docs/superpowers/specs/2026-10-09-work-ledger-design.md`

## Global Constraints

Copied from the spec; every task's requirements include these.

- **Two different events are called a conflict and only one prompts** (spec
  §2). A mechanical race in a delta write retries. A write over somebody's
  decision or edit prompts. If a race ever reaches the prompt, the feature is
  worse than silence.
- **Liveness is `procattr.Alive`, never `os.FindProcess`.** A pid that cannot
  be queried counts as alive.
- **Workspaces are compared with `live.SameWorkspace`, never `==`.**
- **With nobody to ask: write last, snapshot first, say where the old version
  went.** Never refuse — except when the restore point itself fails, where the
  write does not proceed, because unattended overwriting is only acceptable
  while it is recoverable (spec §12).
- `work_conflict` has **no "always"**, and `-y` does not answer it. It must be
  named in `internal/tui/view.go`'s `a` handler *and* its title/hint switch,
  `internal/ui/repl.go`'s `noAlwaysAction` *and* its display switch, and
  `cmd/commands.go:headlessApprover`'s **two** branches (the `-y` group and
  the non-TTY hint list).
- **Nothing here adds per-turn text to the system prompt.** A value that
  changes every turn belongs on the end of a stored message.
- **Never fail a session.** Every failure in this package degrades to "no
  record, no notice, detection in memory only", with one notice.
- `internal/tools` must not import `internal/workledger`. The seam is
  `Registry.OnConflict`, assigned in `cmd/root.go`.
- `go vet` is the only linter; `make -f build.mk verify` must pass before any
  task is called done.
- No test may write to the real `~/.be-code`. Pin `HOME` with `t.Setenv`, and
  `stat -c %Y ~/.be-code/config.json` before and after a run.

## Review Focus

Input classes the spec implies that no task's deliverable would naturally
exercise. Each has its test added to the task that owns the code.

1. **A workspace that is not a git repository.** There is no branch to
   snapshot to, so the restore point must fall back to a sidecar — and if that
   also fails, an unattended write must not proceed. (Task 4)
2. **A pid reused by an unrelated process.** A dead session's record then
   looks alive and its `writing` list is fiction, so a notice names work
   nobody is doing. (Task 1)
3. **Two spellings of one workspace** — case, a symlink, an 8.3 name. Records
   must see each other or the feature silently never fires. (Task 1)
4. **A file this session has never read.** Not a conflict; a first write must
   not prompt. (Task 2)
5. **A conflict reached with no approver and no restore point available.** The
   "write last" premise fails, and the write must stop rather than lose work
   unrecoverably. (Task 6)

---

### Task 1: The record, and who else is working here

**Files:**
- Create: `internal/workledger/record.go`
- Create: `internal/workledger/record_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type Record struct { PID int; Started, Updated, Workspace, Branch, Session, Model, Task string; Writing []string }`
  - `func Dir() (string, error)` — `~/.be-code/work`
  - `func (r Record) Path(dir string) string`
  - `func Save(dir string, r Record) error`
  - `func Others(dir, workspace string, selfPID int) []Record`
  - `func Remove(dir string, pid int, started string) error`
  - `var Alive = procattr.Alive` — a var so a test can stub liveness
  - `var StaleAfter = time.Hour`

- [ ] **Step 1: Write the failing test**

Create `internal/workledger/record_test.go`:

```go
package workledger

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func stubAlive(t *testing.T, live map[int]bool) {
	t.Helper()
	saved := Alive
	Alive = func(pid int) bool { return live[pid] }
	t.Cleanup(func() { Alive = saved })
}

func rec(pid int, ws string) Record {
	now := time.Now().UTC().Format(time.RFC3339)
	return Record{PID: pid, Started: now, Updated: now, Workspace: ws, Branch: "main"}
}

func TestSaveAndReadBack(t *testing.T) {
	dir := t.TempDir()
	stubAlive(t, map[int]bool{41: true, 42: true})

	if err := Save(dir, rec(41, "/w")); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, rec(42, "/w")); err != nil {
		t.Fatal(err)
	}
	got := Others(dir, "/w", 41)
	if len(got) != 1 || got[0].PID != 42 {
		t.Fatalf("Others should report the other session only, got %+v", got)
	}
}

// A record is 0600: it names a workspace path and what somebody is working on.
func TestRecordIsPrivate(t *testing.T) {
	dir := t.TempDir()
	r := rec(41, "/w")
	if err := Save(dir, r); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(r.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("record mode is %v, want 0600", st.Mode().Perm())
	}
}

// A dead process's record is pruned, not reported.
func TestADeadSessionsRecordIsPruned(t *testing.T) {
	dir := t.TempDir()
	stubAlive(t, map[int]bool{41: true}) // 42 is gone
	if err := Save(dir, rec(41, "/w")); err != nil {
		t.Fatal(err)
	}
	gone := rec(42, "/w")
	if err := Save(dir, gone); err != nil {
		t.Fatal(err)
	}
	if got := Others(dir, "/w", 41); len(got) != 0 {
		t.Fatalf("a dead session was reported: %+v", got)
	}
	if _, err := os.Stat(gone.Path(dir)); !os.IsNotExist(err) {
		t.Error("a dead session's record was not removed from disk")
	}
}

// Global constraint: a pid that cannot be queried counts as alive. The
// Windows lesson — os.FindProcess asks for a privilege a non-elevated
// terminal is refused for an elevated session, and live.List then deleted a
// live record and the launch forked a copy onto a copy of the session.
func TestAnUnqueryablePIDCountsAlive(t *testing.T) {
	dir := t.TempDir()
	saved := Alive
	Alive = func(int) bool { return true } // procattr.Alive's "access denied" answer
	t.Cleanup(func() { Alive = saved })

	r := rec(42, "/w")
	if err := Save(dir, r); err != nil {
		t.Fatal(err)
	}
	if got := Others(dir, "/w", 41); len(got) != 1 {
		t.Fatal("a record whose pid cannot be queried must be kept, not pruned")
	}
}

// Review Focus 3: two spellings of one workspace must see each other, or the
// feature silently never fires. live.SameWorkspace resolves links and folds
// case on Windows; == does not.
func TestTwoSpellingsOfOneWorkspaceSeeEachOther(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	dir := t.TempDir()
	stubAlive(t, map[int]bool{42: true})
	if err := Save(dir, rec(42, real)); err != nil {
		t.Fatal(err)
	}
	if got := Others(dir, link, 41); len(got) != 1 {
		t.Errorf("a session reached through a symlink did not see the other: %+v", got)
	}
}

// Review Focus 2: a pid reused by an unrelated process makes a dead session
// look alive, and its Writing list is then fiction. The record cannot prove
// identity, so it says so rather than asserting work nobody is doing.
func TestAStaleRecordWithALivePIDIsMarkedSuspect(t *testing.T) {
	dir := t.TempDir()
	stubAlive(t, map[int]bool{42: true})
	old := rec(42, "/w")
	old.Updated = time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	old.Writing = []string{"internal/thing.go"}
	if err := Save(dir, old); err != nil {
		t.Fatal(err)
	}
	got := Others(dir, "/w", 41)
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	if !got[0].Suspect {
		t.Error("a record untouched for longer than StaleAfter with a live pid must be marked suspect: the pid may have been reused")
	}
	if len(got[0].Writing) != 0 {
		t.Error("a suspect record's Writing list must be dropped, not reported as current work")
	}
}

func TestRemoveTakesOnlyThisSessionsRecord(t *testing.T) {
	dir := t.TempDir()
	stubAlive(t, map[int]bool{41: true, 42: true})
	mine, theirs := rec(41, "/w"), rec(42, "/w")
	if err := Save(dir, mine); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, theirs); err != nil {
		t.Fatal(err)
	}
	if err := Remove(dir, mine.PID, mine.Started); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine.Path(dir)); !os.IsNotExist(err) {
		t.Error("my record is still there")
	}
	if _, err := os.Stat(theirs.Path(dir)); err != nil {
		t.Error("somebody else's record was removed")
	}
}

// Never fail a session: an unreadable record is ignored and removed, like a
// pruned live record.
func TestAnUnparsableRecordIsIgnored(t *testing.T) {
	dir := t.TempDir()
	stubAlive(t, map[int]bool{42: true})
	if err := os.WriteFile(filepath.Join(dir, "99-bad.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, rec(42, "/w")); err != nil {
		t.Fatal(err)
	}
	got := Others(dir, "/w", 41)
	if len(got) != 1 {
		t.Fatalf("a corrupt record broke the listing: %+v", got)
	}
}

func TestOthersOnAMissingDirectoryIsEmptyNotAnError(t *testing.T) {
	if got := Others(filepath.Join(t.TempDir(), "nope"), "/w", 41); len(got) != 0 {
		t.Errorf("got %+v, want nothing", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/workledger -v`
Expected: FAIL — the package does not exist (`no Go files` / `undefined: Record`).

- [ ] **Step 3: Write the implementation**

Create `internal/workledger/record.go`:

```go
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
	"github.com/brown-enterprises/be-code/internal/procattr"
)

// Alive is how a record's process is checked. A var so tests can stub it.
//
// procattr.Alive, never os.FindProcess: os.FindProcess asks for
// PROCESS_QUERY_INFORMATION, which a non-elevated terminal is refused for an
// elevated SSH login's process, and live.List then deleted a live record and
// the launch forked a copy onto a copy of the session. "Access denied" counts
// as alive.
var Alive = procattr.Alive

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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/workledger -v`
Expected: PASS, 9 tests.

- [ ] **Step 5: Commit**

```bash
git add internal/workledger/record.go internal/workledger/record_test.go
git commit -m "workledger: the per-session record of what is being worked on

Advisory, never a lock: a lock that outlives a crashed process is worse than a
lost update, and BE-Code already chose records plus pid liveness for live
sessions. Liveness is procattr.Alive, so a pid that cannot be queried counts
as alive — os.FindProcess is refused that privilege across integrity levels
and once deleted a live record. Workspaces compare with live.SameWorkspace,
because two shells spell one folder differently.

A live pid with an old record may be a reused pid, which this package cannot
disprove, so the record is marked suspect and its Writing list dropped rather
than reporting work nobody is doing."
```

---

### Task 2: Seen and Changed

**Files:**
- Create: `internal/workledger/seen.go`
- Create: `internal/workledger/seen_test.go`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces:
  - `type Seen struct{ ... }` with `func NewSeen(path string) *Seen`
  - `func (s *Seen) Note(file string, content []byte)`
  - `func (s *Seen) NoteHash(file, hash string)`
  - `func (s *Seen) Changed(file string) (changed bool, theirs []byte, err error)`
  - `func (s *Seen) Flush() error`
  - `func HashBytes(b []byte) string`

- [ ] **Step 1: Write the failing test**

Create `internal/workledger/seen_test.go`:

```go
package workledger

import (
	"os"
	"path/filepath"
	"testing"
)

func TestChangedAfterSomebodyElseWrites(t *testing.T) {
	ws := t.TempDir()
	f := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(f, []byte("ours\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewSeen(filepath.Join(t.TempDir(), "seen.json"))
	s.Note(f, []byte("ours\n"))

	changed, theirs, err := s.Changed(f)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("nothing changed yet")
	}

	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, theirs, err = s.Changed(f)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("an outside write was not noticed")
	}
	if string(theirs) != "theirs\n" {
		t.Errorf("theirs = %q, want the content on disk", theirs)
	}
}

// Review Focus 4: a file this session never read is a first write, not a
// conflict. Treating it as one would prompt on every new file.
func TestAnUnseenFileIsNotAConflict(t *testing.T) {
	ws := t.TempDir()
	f := filepath.Join(ws, "new.txt")
	if err := os.WriteFile(f, []byte("somebody else made this\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewSeen(filepath.Join(t.TempDir(), "seen.json"))
	changed, _, err := s.Changed(f)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("a file this session has never seen must not be a conflict")
	}
}

// What we wrote is what we last saw, or our own write would look like
// somebody else's on the next one.
func TestOurOwnWriteIsNotAConflict(t *testing.T) {
	ws := t.TempDir()
	f := filepath.Join(ws, "a.txt")
	s := NewSeen(filepath.Join(t.TempDir(), "seen.json"))
	if err := os.WriteFile(f, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Note(f, []byte("v1\n"))
	if err := os.WriteFile(f, []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Note(f, []byte("v2\n"))
	changed, _, err := s.Changed(f)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("our own second write was reported as somebody else's change")
	}
}

// A file that was seen and is now gone is a change: somebody removed it.
func TestADeletedFileIsAChange(t *testing.T) {
	ws := t.TempDir()
	f := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(f, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewSeen(filepath.Join(t.TempDir(), "seen.json"))
	s.Note(f, []byte("v1\n"))
	if err := os.Remove(f); err != nil {
		t.Fatal(err)
	}
	changed, theirs, err := s.Changed(f)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("a file that was seen and is now gone is a change")
	}
	if theirs != nil {
		t.Errorf("theirs should be nil for a deleted file, got %q", theirs)
	}
}

// A resumed session must not treat every file as unseen: the hashes outlive
// the process.
func TestSeenSurvivesAFlushAndReload(t *testing.T) {
	ws := t.TempDir()
	f := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(f, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(t.TempDir(), "seen.json")
	s := NewSeen(store)
	s.Note(f, []byte("v1\n"))
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	again := NewSeen(store)
	changed, _, err := again.Changed(f)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("a reloaded Seen did not remember what it had seen")
	}
}

// NoteHash exists so the engine can feed the hash it already computed for a
// task document rather than hashing the same bytes twice.
func TestNoteHashMatchesNote(t *testing.T) {
	ws := t.TempDir()
	f := filepath.Join(ws, "a.txt")
	body := []byte("v1\n")
	if err := os.WriteFile(f, body, 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewSeen(filepath.Join(t.TempDir(), "seen.json"))
	s.NoteHash(f, HashBytes(body))
	changed, _, err := s.Changed(f)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("a hash noted directly must compare equal to the file it came from")
	}
}

func TestFlushToAnUnwritablePlaceIsNotFatal(t *testing.T) {
	s := NewSeen(filepath.Join(t.TempDir(), "nope", "deeper", "seen.json"))
	s.Note("/x", []byte("v"))
	// It may error, but it must not panic, and the in-memory map still works.
	_ = s.Flush()
	if changed, _, _ := s.Changed("/x"); changed {
		t.Error("in-memory detection stopped working after a failed flush")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/workledger -run 'TestChanged|TestAnUnseen|TestOurOwn|TestADeleted|TestSeen|TestNoteHash|TestFlush' -v`
Expected: FAIL to build — `undefined: NewSeen`, `undefined: HashBytes`.

- [ ] **Step 3: Write the implementation**

Create `internal/workledger/seen.go`:

```go
package workledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// deleted is the hash recorded for a file that was seen and is now gone, so
// "somebody removed it" is a change rather than indistinguishable from never
// having seen it.
const deleted = "-"

// Seen is the hash of each file as this session last read or wrote it. It is
// the same idea internal/engine already uses for task documents, where
// state.Docs is "the hash of each document as the engine last wrote it, so an
// outside edit is recognisable" (internal/engine/store.go).
//
// A file this session has never seen is never a conflict: it is a first read.
// That makes a missed Note a detection that silently never fires, which is
// why the spec lists every call site.
type Seen struct {
	mu    sync.Mutex
	path  string
	hash  map[string]string
	dirty bool
}

// NewSeen loads what a previous run of this session recorded, if any. A
// missing or corrupt store is simply nothing seen yet.
func NewSeen(path string) *Seen {
	s := &Seen{path: path, hash: map[string]string{}}
	if b, err := os.ReadFile(path); err == nil {
		var m map[string]string
		if json.Unmarshal(b, &m) == nil && m != nil {
			s.hash = m
		}
	}
	return s
}

// HashBytes is the hash both Note and the engine's own document hash use.
// SHA-256 of the exact bytes, with no normalisation: a byte that changed is
// a change.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Note records content as what this session last saw at file.
func (s *Seen) Note(file string, content []byte) { s.NoteHash(file, HashBytes(content)) }

// NoteHash records a hash computed elsewhere — the engine's state.Docs, which
// already hashes every document it writes for exactly this purpose.
func (s *Seen) NoteHash(file, hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hash[file] != hash {
		s.hash[file] = hash
		s.dirty = true
	}
}

// Changed reports whether file differs from what this session last saw, and
// returns what is on disk now so a caller can show the difference.
func (s *Seen) Changed(file string) (bool, []byte, error) {
	s.mu.Lock()
	was, seen := s.hash[file]
	s.mu.Unlock()
	if !seen {
		return false, nil, nil // never read it; a first write is not a conflict
	}
	b, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return was != deleted, nil, nil
		}
		return false, nil, err
	}
	if HashBytes(b) == was {
		return false, nil, nil
	}
	return true, b, nil
}

// Forget records that file is gone, so its removal is a change rather than
// looking unseen.
func (s *Seen) Forget(file string) { s.NoteHash(file, deleted) }

// Flush persists the hashes so a resumed session does not treat every file as
// unseen. A failure is returned but never fatal: in-memory detection is
// unaffected.
func (s *Seen) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	b, err := json.Marshal(s.hash)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return err
	}
	s.dirty = false
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/workledger -v`
Expected: PASS, all tests from Tasks 1 and 2.

- [ ] **Step 5: Commit**

```bash
git add internal/workledger/seen.go internal/workledger/seen_test.go
git commit -m "workledger: hash what we read, compare before we write

The mechanism internal/engine already uses for task documents, generalised:
state.Docs is the hash of each document as the engine last wrote it, so an
outside edit is recognisable. NoteHash exists so the engine can feed that hash
rather than computing a second one that could disagree.

A file this session never saw is never a conflict — a first write must not
prompt — which makes a missed Note a detection that silently never fires."
```

---

### Task 3: `gitctx.SnapshotTo`

**Files:**
- Modify: `internal/gitctx/gitctx.go:93-94` (the prefix const), `:115-175` (`Snapshot`)
- Modify: `internal/ui/initflow.go:32` (the comment naming the const)
- Create: `internal/gitctx/snapshotto_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `const SnapshotPrefixInit = "be-code/pre-init/"`
  - `const SnapshotPrefixOverwrite = "be-code/pre-overwrite/"`
  - `func SnapshotTo(ctx context.Context, root, prefix, message string) (string, error)`
  - `func Snapshot(ctx context.Context, root, message string) (string, error)` — unchanged signature, delegates with the init prefix
  - `const SnapshotPrefix = SnapshotPrefixInit` — kept so existing callers and tests do not move

- [ ] **Step 1: Write the failing test**

Create `internal/gitctx/snapshotto_test.go`:

```go
package gitctx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func repoWithACommit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(argv ...string) {
		cmd := exec.Command("git", argv...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", argv, err, out)
		}
	}
	run("init", "--quiet", "--initial-branch=main")
	run("config", "user.email", "fixture@example.com")
	run("config", "user.name", "Fixture")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "--quiet", "-m", "first")
	return dir
}

func gitOut(t *testing.T, dir string, argv ...string) string {
	t.Helper()
	cmd := exec.Command("git", argv...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", argv, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestSnapshotToUsesTheGivenPrefix(t *testing.T) {
	dir := repoWithACommit(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("about to be overwritten\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	branch, err := SnapshotTo(context.Background(), dir, SnapshotPrefixOverwrite, "be-code: before an unattended overwrite")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(branch, SnapshotPrefixOverwrite) {
		t.Errorf("branch = %q, want the overwrite prefix", branch)
	}
	// The content at that branch is what was about to be lost.
	body := gitOut(t, dir, "show", branch+":a.txt")
	if body != "about to be overwritten" {
		t.Errorf("the snapshot holds %q", body)
	}
}

// The snapshot must not move HEAD, the index or the checkout — the whole
// point of the private GIT_INDEX_FILE.
func TestSnapshotToLeavesTheCheckoutAlone(t *testing.T) {
	dir := repoWithACommit(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("working\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	headBefore := gitOut(t, dir, "rev-parse", "HEAD")
	branchBefore := gitOut(t, dir, "rev-parse", "--abbrev-ref", "HEAD")

	if _, err := SnapshotTo(context.Background(), dir, SnapshotPrefixOverwrite, "m"); err != nil {
		t.Fatal(err)
	}
	if got := gitOut(t, dir, "rev-parse", "HEAD"); got != headBefore {
		t.Error("HEAD moved")
	}
	if got := gitOut(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != branchBefore {
		t.Error("the checked-out branch changed")
	}
	body, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "working\n" {
		t.Errorf("the working tree changed: %q", body)
	}
	if out := gitOut(t, dir, "status", "--porcelain"); !strings.Contains(out, "a.txt") {
		t.Errorf("the uncommitted change is no longer uncommitted: %q", out)
	}
}

// Snapshot keeps its signature and its prefix, so /init's callers and tests
// do not move.
func TestSnapshotStillUsesTheInitPrefix(t *testing.T) {
	dir := repoWithACommit(t)
	branch, err := Snapshot(context.Background(), dir, "be-code: restore point before init")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(branch, SnapshotPrefixInit) {
		t.Errorf("branch = %q, want the init prefix", branch)
	}
	if SnapshotPrefix != SnapshotPrefixInit {
		t.Error("SnapshotPrefix must stay an alias for the init prefix")
	}
}

// Review Focus 1, first half: outside a repository there is nothing to
// snapshot to, and the caller has to be able to tell.
func TestSnapshotToOutsideARepositorySaysSo(t *testing.T) {
	_, err := SnapshotTo(context.Background(), t.TempDir(), SnapshotPrefixOverwrite, "m")
	if err == nil {
		t.Fatal("snapshotting a directory that is not a repository succeeded")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("the error should say what is wrong: %v", err)
	}
}

// Two snapshots in the same second must not collide.
func TestTwoSnapshotsInOneSecondGetDifferentBranches(t *testing.T) {
	dir := repoWithACommit(t)
	a, err := SnapshotTo(context.Background(), dir, SnapshotPrefixOverwrite, "m")
	if err != nil {
		t.Fatal(err)
	}
	b, err := SnapshotTo(context.Background(), dir, SnapshotPrefixOverwrite, "m")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("both snapshots are on %q", a)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/gitctx -run 'TestSnapshotTo|TestSnapshotStill|TestTwoSnapshots' -v`
Expected: FAIL to build — `undefined: SnapshotTo`, `undefined: SnapshotPrefixOverwrite`,
`undefined: SnapshotPrefixInit`.

- [ ] **Step 3: Make the prefix a parameter**

In `internal/gitctx/gitctx.go`, replace the const at line 93-94:

```go
const (
	// SnapshotPrefixInit is the branch namespace /init's restore point uses.
	SnapshotPrefixInit = "be-code/pre-init/"
	// SnapshotPrefixOverwrite is the namespace for a restore point taken
	// before an unattended write overwrites somebody else's change (work
	// ledger spec §9).
	SnapshotPrefixOverwrite = "be-code/pre-overwrite/"

	// SnapshotPrefix is the original name, kept so existing callers and the
	// comment in internal/ui/initflow.go do not have to move.
	SnapshotPrefix = SnapshotPrefixInit
)
```

Then rename `Snapshot` to `SnapshotTo`, give it the prefix parameter, and add
the thin `Snapshot` back. The body changes in exactly two places: the
signature, and the two uses of `SnapshotPrefix` at the end.

```go
// Snapshot is SnapshotTo under the init prefix: /init's restore point.
func Snapshot(ctx context.Context, root, message string) (string, error) {
	return SnapshotTo(ctx, root, SnapshotPrefixInit, message)
}

// SnapshotTo commits the whole working tree — tracked and untracked,
// honouring .gitignore — as one commit on a new branch under prefix, through
// a private GIT_INDEX_FILE so HEAD, the real index and the checkout never
// move. It returns the branch name.
func SnapshotTo(ctx context.Context, root, prefix, message string) (string, error) {
```

and at the branch-naming lines (was 165 and 170):

```go
	branch := prefix + time.Now().Format("20060102-150405")
	for i := 2; ; i++ {
		if _, err := gitEnv(ctx, root, nil, "rev-parse", "--verify", "-q", "refs/heads/"+branch); err != nil {
			break
		}
		branch = fmt.Sprintf("%s%s-%d", prefix, time.Now().Format("20060102-150405"), i)
	}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/gitctx -v && go build ./...`
Expected: PASS, 5 new tests plus the package's existing ones, and a clean
build (which is what proves `/init`'s caller still compiles).

- [ ] **Step 5: Commit**

```bash
git add internal/gitctx/gitctx.go internal/gitctx/snapshotto_test.go
git commit -m "gitctx: the snapshot branch prefix is a parameter

/init's restore point is one use; a restore point taken before an unattended
write overwrites somebody else's change is another, and it needs its own
namespace. Snapshot keeps its signature and SnapshotPrefix stays an alias, so
no existing caller moves."
```

---

### Task 4: Restore points

**Files:**
- Create: `internal/workledger/restore.go`
- Create: `internal/workledger/restore_test.go`

**Interfaces:**
- Consumes: `gitctx.SnapshotTo`, `gitctx.SnapshotPrefixOverwrite` (Task 3).
- Produces:
  - `type RestoreKind int` with `RestoreGit`, `RestoreSidecar`
  - `type Restore struct { Kind RestoreKind; Branch, Sidecar, HowTo string }`
  - `func KeepOld(ctx context.Context, workspace, path string) (Restore, error)`
  - `func PruneSidecars(dir string, keep time.Duration) int`
  - `var Now = func() time.Time { return time.Now() }`

- [ ] **Step 1: Write the failing test**

Create `internal/workledger/restore_test.go`:

```go
package workledger

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(argv ...string) {
		cmd := exec.Command("git", argv...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", argv, err, out)
		}
	}
	run("init", "--quiet", "--initial-branch=main")
	run("config", "user.email", "fixture@example.com")
	run("config", "user.name", "Fixture")
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "--quiet", "-m", "seed")
	return dir
}

// A workspace file goes to a git branch, and the HowTo line is the command
// that brings it back — the same shape /init prints.
func TestKeepOldSnapshotsAWorkspaceFileToABranch(t *testing.T) {
	ws := gitRepo(t)
	f := filepath.Join(ws, "doc.md")
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := KeepOld(context.Background(), ws, f)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != RestoreGit {
		t.Fatalf("kind = %v, want RestoreGit", got.Kind)
	}
	if !strings.HasPrefix(got.Branch, "be-code/pre-overwrite/") {
		t.Errorf("branch = %q", got.Branch)
	}
	if !strings.Contains(got.HowTo, "git restore") || !strings.Contains(got.HowTo, got.Branch) {
		t.Errorf("HowTo must be the command that restores it: %q", got.HowTo)
	}
}

// Review Focus 1: outside a repository there is no branch, so the old version
// goes to a sidecar and the caller can still tell the person where it is.
func TestKeepOldOutsideARepositoryUsesASidecar(t *testing.T) {
	ws := t.TempDir() // not a repo
	f := filepath.Join(ws, "doc.md")
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := KeepOld(context.Background(), ws, f)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != RestoreSidecar {
		t.Fatalf("kind = %v, want RestoreSidecar", got.Kind)
	}
	body, rerr := os.ReadFile(got.Sidecar)
	if rerr != nil {
		t.Fatalf("the sidecar is not there: %v", rerr)
	}
	if string(body) != "theirs\n" {
		t.Errorf("the sidecar holds %q", body)
	}
	if !strings.Contains(got.HowTo, got.Sidecar) {
		t.Errorf("HowTo must name the sidecar: %q", got.HowTo)
	}
}

// A dotdir file is in no repository, so it always goes to a sidecar even when
// the workspace is one.
func TestKeepOldOfAFileOutsideTheWorkspaceUsesASidecar(t *testing.T) {
	ws := gitRepo(t)
	outside := filepath.Join(t.TempDir(), "packs.json")
	if err := os.WriteFile(outside, []byte(`{"forced":{"a":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := KeepOld(context.Background(), ws, outside)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != RestoreSidecar {
		t.Fatalf("kind = %v, want RestoreSidecar for a file outside the workspace", got.Kind)
	}
	if st, serr := os.Stat(got.Sidecar); serr != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("a dotdir sidecar must be 0600: %v %v", st, serr)
	}
}

// Review Focus 1, the hard half: with no restore point available at all, this
// must fail. The caller's unattended write is only acceptable because it is
// recoverable (spec §12).
func TestKeepOldFailsWhenItCanKeepNothing(t *testing.T) {
	ws := t.TempDir()
	sub := filepath.Join(ws, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(sub, "doc.md")
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	if err := os.Chmod(sub, 0o500); err != nil { // no new files beside it
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sub, 0o755) })

	if _, err := KeepOld(context.Background(), ws, f); err == nil {
		t.Fatal("KeepOld reported success with nowhere to keep the old version")
	}
}

// A file that does not exist has nothing to keep, and that is not a failure:
// there is no old version to lose.
func TestKeepOldOfAMissingFileIsANoop(t *testing.T) {
	ws := t.TempDir()
	got, err := KeepOld(context.Background(), ws, filepath.Join(ws, "nope.md"))
	if err != nil {
		t.Fatalf("a missing file is nothing to keep, not an error: %v", err)
	}
	if got.Sidecar != "" || got.Branch != "" {
		t.Errorf("nothing should have been kept: %+v", got)
	}
}

func TestPruneSidecarsDropsOldOnesOnly(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "packs.json.overwritten-20200101-000000")
	recent := filepath.Join(dir, "packs.json.overwritten-20991231-235959")
	other := filepath.Join(dir, "packs.json")
	for _, p := range []string{old, recent, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	long := time.Now().Add(-365 * 24 * time.Hour)
	if err := os.Chtimes(old, long, long); err != nil {
		t.Fatal(err)
	}

	n := PruneSidecars(dir, 168*time.Hour)
	if n != 1 {
		t.Errorf("pruned %d, want 1", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("the old sidecar is still there")
	}
	for _, p := range []string{recent, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed and should not have been", filepath.Base(p))
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/workledger -run 'TestKeepOld|TestPruneSidecars' -v`
Expected: FAIL to build — `undefined: KeepOld`, `undefined: RestoreGit`,
`undefined: PruneSidecars`.

- [ ] **Step 3: Write the implementation**

Create `internal/workledger/restore.go`:

```go
package workledger

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/gitctx"
)

// Now is the clock restore points are stamped with. A var so a test can fix it.
var Now = func() time.Time { return time.Now() }

// sidecarMark is what makes a sidecar recognisable to PruneSidecars.
const sidecarMark = ".overwritten-"

type RestoreKind int

const (
	// RestoreNone is "there was nothing to keep": the file does not exist.
	RestoreNone RestoreKind = iota
	// RestoreGit is a branch under gitctx.SnapshotPrefixOverwrite.
	RestoreGit
	// RestoreSidecar is a timestamped copy beside the file.
	RestoreSidecar
)

// Restore is where an old version went, and the line that brings it back.
type Restore struct {
	Kind    RestoreKind
	Branch  string // RestoreGit
	Sidecar string // RestoreSidecar
	HowTo   string // the command a person can run
}

// KeepOld makes the current contents of path recoverable before a caller
// overwrites them without asking.
//
// A file inside a git workspace goes to a branch, which is what the owner
// asked for: "marked in the git tree so if it does overwrite something, then
// it can be restored." Anything else — a workspace that is not a repository,
// or a file in the dotdir, which is in no tree at all — goes to a timestamped
// copy beside it.
//
// An error here means nothing could be kept, and the caller must not proceed:
// writing over somebody's work unattended is only acceptable while it is
// recoverable (spec §12).
func KeepOld(ctx context.Context, workspace, path string) (Restore, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return Restore{Kind: RestoreNone}, nil
		}
		return Restore{}, err
	}
	if inside(workspace, path) && gitctx.IsRepo(ctx, workspace) {
		branch, err := gitctx.SnapshotTo(ctx, workspace, gitctx.SnapshotPrefixOverwrite,
			"be-code: before overwriting a change this session did not make")
		if err == nil {
			rel, rerr := filepath.Rel(workspace, path)
			if rerr != nil {
				rel = "."
			}
			return Restore{
				Kind:   RestoreGit,
				Branch: branch,
				HowTo: fmt.Sprintf("git restore --source=%s --staged --worktree -- %s",
					branch, filepath.ToSlash(rel)),
			}, nil
		}
		// Fall through: a repository that cannot be snapshotted still has a
		// directory the old bytes can go in.
	}
	return sidecar(path)
}

func sidecar(path string) (Restore, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Restore{}, err
	}
	name := path + sidecarMark + Now().Format("20060102-150405")
	for i := 2; ; i++ {
		if _, err := os.Stat(name); os.IsNotExist(err) {
			break
		}
		name = fmt.Sprintf("%s%s%s-%d", path, sidecarMark, Now().Format("20060102-150405"), i)
	}
	// 0600 and not the original's mode: a sidecar of a dotdir file holds
	// somebody's approvals, and a sidecar of a workspace file is nobody's
	// business but this user's either.
	if err := os.WriteFile(name, b, 0o600); err != nil {
		return Restore{}, fmt.Errorf("nowhere to keep the old %s: %w", filepath.Base(path), err)
	}
	return Restore{
		Kind:    RestoreSidecar,
		Sidecar: name,
		HowTo:   fmt.Sprintf("cp %s %s", name, path),
	}, nil
}

// inside reports whether path is under workspace. A dotdir file is not, and
// has no tree to be snapshotted into.
func inside(workspace, path string) bool {
	rel, err := filepath.Rel(workspace, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// PruneSidecars removes sidecars in dir older than keep, and returns how many
// it took. It runs when the directory is next written, so a workspace that
// goes quiet keeps its sidecars until it is used again — the honest cost of
// not running a timer for this.
func PruneSidecars(dir string, keep time.Duration) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	cut := Now().Add(-keep)
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.Contains(e.Name(), sidecarMark) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cut) {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			n++
		}
	}
	return n
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/workledger -v`
Expected: PASS, every test in the package.

- [ ] **Step 5: Commit**

```bash
git add internal/workledger/restore.go internal/workledger/restore_test.go
git commit -m "workledger: make an old version recoverable before overwriting it

A workspace file goes to a branch under be-code/pre-overwrite/, which is the
owner's ruling: mark it in the git tree so it can be restored. A file in the
dotdir is in no tree, and a workspace that is not a repository has no branch,
so those go to a timestamped copy beside the file.

An error means nothing could be kept, and the caller must not proceed:
overwriting somebody's work unattended is only acceptable while it is
recoverable."
```

---

### Task 5: The mechanical race in `mutate`

**Files:**
- Modify: `internal/packs/trust.go` (`mutate`, at the end of the file)
- Create: `internal/packs/race_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: no new exported API. `mutate` gains compare-and-retry, and
  `var beforeWriteHook func()` — unexported, nil in production, set by the
  test to drive the interleaving.

- [ ] **Step 1: Write the failing test**

Create `internal/packs/race_test.go`:

```go
package packs

import (
	"path/filepath"
	"testing"
)

// Spec §2: this is the *mechanical* race — two processes' read-modify-write
// interleave and one delta is lost. Both deltas were wanted and re-reading
// recovers both, so it retries and never prompts.
//
// The interleaving is driven, not hoped for: beforeWriteHook fires after this
// mutate has read and before it writes, and the hook writes the other key, as
// another process would.
func TestMutateDoesNotLoseAConcurrentDelta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "packs.json")
	if err := Trust(path, "first", "hash-1"); err != nil {
		t.Fatal(err)
	}

	// While this SetForced is between its read and its write, another writer
	// records a trust. Without compare-and-retry, that trust is lost.
	once := false
	beforeWriteHook = func() {
		if once {
			return
		}
		once = true
		// A second process: its own read-modify-write, completed inside ours.
		if err := trustOutOfBand(path, "second", "hash-2"); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { beforeWriteHook = nil })

	if err := SetForced(path, "a-pack", false); err != nil {
		t.Fatal(err)
	}

	if got := TrustedHash(path, "second"); got != "hash-2" {
		t.Errorf("the concurrent writer's trust was lost: %q", got)
	}
	if on, set := Forced(path, "a-pack"); !set || on {
		t.Errorf("our own delta was lost: on=%v set=%v", on, set)
	}
	if got := TrustedHash(path, "first"); got != "hash-1" {
		t.Errorf("the pre-existing trust was lost: %q", got)
	}
}

// The retry is bounded: a file being rewritten continuously must error rather
// than spin.
func TestMutateGivesUpRatherThanSpinning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "packs.json")
	beforeWriteHook = func() {
		// Every attempt is interfered with, for ever.
		_ = trustOutOfBand(path, "noisy", randomHash())
	}
	t.Cleanup(func() { beforeWriteHook = nil })

	err := SetForced(path, "a-pack", true)
	if err == nil {
		t.Fatal("mutate succeeded against an endlessly changing file; it must bound its retries")
	}
	if !strings.Contains(err.Error(), "packs.json") {
		t.Errorf("the error should name the file: %v", err)
	}
}
```

Add the two helpers to the same file, with `"path/filepath"`, `"strconv"`
and `"strings"` imported. `trustOutOfBand` deliberately bypasses `trustMu` —
it stands in for another process, which does not share our mutex. Use
`strings.Contains` in the second test rather than a hand-rolled one.

```go
// trustOutOfBand is another process's read-modify-write: it does not take
// trustMu, because a second process does not share it.
func trustOutOfBand(path, pack, hash string) error {
	st := readState(path)
	if st.Trusted == nil {
		st.Trusted = map[string]string{}
	}
	st.Trusted[pack] = hash
	return writeState(path, st)
}

var hashSeq int

func randomHash() string {
	hashSeq++
	return "h" + strconv.Itoa(hashSeq)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/packs -run TestMutate -v`
Expected: FAIL to build — `undefined: beforeWriteHook`, `undefined: writeState`.
Once those exist (Step 3), the *first* test must fail on
`the concurrent writer's trust was lost` before the retry is added, which is
the behaviour being fixed.

- [ ] **Step 3: Split the write out, and add compare-and-retry**

In `internal/packs/trust.go`, replace `mutate` with:

```go
// beforeWriteHook fires between mutate's read and its write. Nil in
// production; a test sets it to drive the interleaving that loses a delta,
// rather than hoping for it.
var beforeWriteHook func()

// writeState persists st atomically. Split out of mutate so a test can stand
// in for another process's write.
func writeState(path string, st state) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Written through a temp file in the same directory and renamed, the way
	// everything else in the dotdir is, so a crash mid-write cannot leave a
	// half-written record of what somebody approved.
	trustSeq++
	tmp := path + ".tmp" + strconv.Itoa(trustSeq)
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// mutate is every write to the file: re-read, change, write atomically.
//
// trustMu serialises writers inside this process. It cannot serialise another
// process — `be-code packs add` in a second terminal while a session is live
// — so the read and the write are guarded by comparing the file against what
// this attempt read: if it moved, the delta is applied again to the new state
// and both writers' intent survives.
//
// This is the mechanical race of work-ledger spec §2, and it deliberately
// does not prompt: both deltas were wanted, nothing is lost by retrying, and
// a modal raised on a microsecond interleaving is noise nobody can answer.
func mutate(path string, f func(*state)) error {
	trustMu.Lock()
	defer trustMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	const attempts = 3
	for i := 0; i < attempts; i++ {
		before := rawState(path)
		st := readState(path)
		f(&st)
		if beforeWriteHook != nil {
			beforeWriteHook()
		}
		if !bytes.Equal(before, rawState(path)) {
			continue // somebody else wrote between our read and our write
		}
		return writeState(path, st)
	}
	return fmt.Errorf("%s kept changing under %d attempts to update it; nothing was written",
		filepath.Base(path), attempts)
}

// rawState is the file's exact bytes, or nil. The comparison is on bytes
// rather than on a parsed state so a change this version does not understand
// still counts as a change.
func rawState(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}
```

Add `"bytes"` and `"fmt"` to the file's imports if they are not already there.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/packs -run TestMutate -v`
Expected: PASS, 2 tests.

- [ ] **Step 5: Run the whole package, which is where a regression would show**

Run: `go test ./internal/packs`
Expected: ok. Every pack approval and forced-state test goes through `mutate`.

- [ ] **Step 6: Commit**

```bash
git add internal/packs/trust.go internal/packs/race_test.go
git commit -m "packs: mutate compares before it writes, and retries

trustMu serialises writers inside one process and cannot serialise another —
be-code packs add in a second terminal while a session is live — so a genuine
interleaving dropped the other writer's key. The read and the write are now
guarded by comparing the file's bytes against what the attempt read.

It retries rather than prompting, deliberately: this is the mechanical race of
the work-ledger spec's §2, both deltas were wanted, and a modal raised on a
microsecond interleaving is noise nobody can answer. The test drives the
interleaving through a hook rather than hoping for it."
```

---

### Task 6: Resolving a conflict

**Files:**
- Create: `internal/workledger/resolve.go`
- Create: `internal/workledger/resolve_test.go`

**Interfaces:**
- Consumes: `Seen` (Task 2), `KeepOld`, `Restore`, `RestoreNone` (Task 4),
  `Others`, `Record` (Task 1).
- Produces:
  - `type Decision int` with `KeepMine`, `KeepTheirs`
  - `type Asker func(action, detail string) bool`
  - `type Conflict struct { Path, Workspace string; Theirs []byte; Others []Record }`
  - `func (c Conflict) Detail() string`
  - `func Resolve(ctx context.Context, c Conflict, ask Asker, fired bool, notice func(string)) (Decision, error)`

- [ ] **Step 1: Write the failing test**

Create `internal/workledger/resolve_test.go`:

```go
package workledger

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func conflictOn(t *testing.T, ws string) Conflict {
	t.Helper()
	f := filepath.Join(ws, "doc.md")
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return Conflict{Path: f, Workspace: ws, Theirs: []byte("theirs\n")}
}

func TestResolveAsksAndKeepsMineOnYes(t *testing.T) {
	ws := gitRepo(t)
	var asked string
	ask := func(action, detail string) bool { asked = action; return true }

	got, err := Resolve(context.Background(), conflictOn(t, ws), ask, false, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if got != KeepMine {
		t.Errorf("decision = %v, want KeepMine", got)
	}
	if asked != "work_conflict" {
		t.Errorf("asked %q, want work_conflict", asked)
	}
}

func TestResolveKeepsTheirsOnNo(t *testing.T) {
	ws := gitRepo(t)
	got, err := Resolve(context.Background(), conflictOn(t, ws),
		func(string, string) bool { return false }, false, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if got != KeepTheirs {
		t.Errorf("decision = %v, want KeepTheirs", got)
	}
}

// Spec §7: with nobody to ask, write last — but snapshot first and say where
// the old version went. Never refuse.
func TestResolveDuringAFiredTurnWritesAfterKeepingTheOldVersion(t *testing.T) {
	ws := gitRepo(t)
	var notices []string
	got, err := Resolve(context.Background(), conflictOn(t, ws),
		func(string, string) bool { t.Fatal("a fired turn must not be asked"); return false },
		true, func(s string) { notices = append(notices, s) })
	if err != nil {
		t.Fatal(err)
	}
	if got != KeepMine {
		t.Errorf("decision = %v, want KeepMine: an unattended turn finishes its work", got)
	}
	if len(notices) == 0 {
		t.Fatal("an unattended overwrite must say where the old version went")
	}
    joined := strings.Join(notices, " ")
	if !strings.Contains(joined, "be-code/pre-overwrite/") {
		t.Errorf("the notice must name the restore point: %q", joined)
	}
	if !strings.Contains(joined, "git restore") {
		t.Errorf("the notice must give the command that restores it: %q", joined)
	}
}

// A nil asker is the same situation: nobody to ask.
func TestResolveWithNoAskerWritesAfterKeepingTheOldVersion(t *testing.T) {
	ws := gitRepo(t)
	var notices []string
	got, err := Resolve(context.Background(), conflictOn(t, ws), nil, false,
		func(s string) { notices = append(notices, s) })
	if err != nil {
		t.Fatal(err)
	}
	if got != KeepMine {
		t.Errorf("decision = %v, want KeepMine", got)
	}
	if len(notices) == 0 {
		t.Error("an unattended overwrite must say where the old version went")
	}
}

// Review Focus 5: the "write last" premise is that the old version survives.
// With nowhere to keep it, the write must not proceed.
func TestResolveUnattendedRefusesWhenNothingCanBeKept(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	ws := t.TempDir() // not a repo, so a sidecar is the only option
	sub := filepath.Join(ws, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(sub, "doc.md")
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sub, 0o755) })

	c := Conflict{Path: f, Workspace: ws, Theirs: []byte("theirs\n")}
	_, err := Resolve(context.Background(), c, nil, true, func(string) {})
	if err == nil {
		t.Fatal("an unattended overwrite proceeded with nowhere to keep the old version")
	}
}

// The detail is what a person reads to choose. It has to name the file, what
// each answer does, and — when a record says so — who changed it.
func TestDetailNamesTheFileTheOptionsAndWhoChangedIt(t *testing.T) {
	c := Conflict{
		Path:      "/w/.be-code/tasks/003-thing.md",
		Workspace: "/w",
		Theirs:    []byte("theirs\n"),
		Others:    []Record{{PID: 48231, Branch: "feat/thing", Task: "003"}},
	}
	d := c.Detail()
	for _, want := range []string{"003-thing.md", "48231", "feat/thing", "keep mine", "keep theirs", "show me"} {
		if !strings.Contains(d, want) {
			t.Errorf("the detail should mention %q:\n%s", want, d)
		}
	}
}

// A conflict with no work record still has to explain itself: the change may
// have come from a person's editor rather than another session.
func TestDetailWithNoRecordStillSaysWhatHappened(t *testing.T) {
	c := Conflict{Path: "/w/a.go", Workspace: "/w", Theirs: []byte("x")}
	d := c.Detail()
	if !strings.Contains(d, "a.go") {
		t.Errorf("the detail must name the file:\n%s", d)
	}
	if strings.Contains(d, "pid 0") {
		t.Errorf("no record means no pid to name:\n%s", d)
	}
	if !strings.Contains(d, "changed since") {
		t.Errorf("the detail must say what happened:\n%s", d)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/workledger -run 'TestResolve|TestDetail' -v`
Expected: FAIL to build — `undefined: Conflict`, `undefined: Resolve`,
`undefined: KeepMine`.

- [ ] **Step 3: Write the implementation**

Create `internal/workledger/resolve.go`:

```go
package workledger

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// Decision is what to do about a conflict.
type Decision int

const (
	// KeepMine writes this session's version. The other version is
	// recoverable: unattended, that is guaranteed before the write.
	KeepMine Decision = iota
	// KeepTheirs abandons this write.
	KeepTheirs
)

// Asker is tools.ApproveFunc's shape, taken as a plain function so this
// package does not import internal/tools — which would be the wrong
// direction, since tools reaches this through an injected hook.
type Asker func(action, detail string) bool

// Conflict is one file that changed since this session read it.
type Conflict struct {
	Path      string
	Workspace string
	Theirs    []byte
	// Others is the live work records for this workspace, so the detail can
	// say who changed it when that is knowable.
	Others []Record
}

// Detail is what a person reads to choose. It names the file, who changed it
// where a record says so, and what each answer does — a question whose
// options are not spelled out is a question people answer by habit.
func (c Conflict) Detail() string {
	var b strings.Builder
	rel := c.Path
	if r, err := filepath.Rel(c.Workspace, c.Path); err == nil && !strings.HasPrefix(r, "..") {
		rel = filepath.ToSlash(r)
	}
	fmt.Fprintf(&b, "%s changed since this session read it\n", rel)
	switch {
	case len(c.Others) == 1:
		o := c.Others[0]
		fmt.Fprintf(&b, "  another session (pid %d", o.PID)
		if o.Branch != "" {
			fmt.Fprintf(&b, ", %s", o.Branch)
		}
		if o.Task != "" {
			fmt.Fprintf(&b, ", task %s", o.Task)
		}
		b.WriteString(") is working in this workspace\n")
	case len(c.Others) > 1:
		fmt.Fprintf(&b, "  %d other sessions are working in this workspace\n", len(c.Others))
	default:
		b.WriteString("  no other session is recorded, so it was changed outside BE-Code\n")
	}
	b.WriteString("\n  y  keep mine   — write this session's version; theirs is kept and the\n")
	b.WriteString("                   notice says where\n")
	b.WriteString("  n  keep theirs — abandon this write\n")
	b.WriteString("  d  show me     — print the difference and ask again\n")
	return b.String()
}

// Resolve decides what happens to one conflict.
//
// With a person there, it asks. With nobody there — a fired scheduled turn,
// or no approver at all — it writes, but only after the old version is
// recoverable, and it says where that is. Refusing was the alternative and
// the owner ruled against it: an unattended turn should finish its work, and
// nothing should be unrecoverable.
//
// The one case it refuses is the one where that promise cannot be kept: if
// nothing can be saved, the write does not proceed.
func Resolve(ctx context.Context, c Conflict, ask Asker, fired bool, notice func(string)) (Decision, error) {
	if ask != nil && !fired {
		if ask("work_conflict", c.Detail()) {
			// A person chose to overwrite, having seen what they were
			// overwriting. Keep the old version anyway when it is free to do
			// so, but a failure here is not worth refusing their choice.
			if r, err := KeepOld(ctx, c.Workspace, c.Path); err == nil && r.Kind != RestoreNone && notice != nil {
				notice(keptNotice(c.Path, r))
			}
			return KeepMine, nil
		}
		return KeepTheirs, nil
	}

	r, err := KeepOld(ctx, c.Workspace, c.Path)
	if err != nil {
		return KeepTheirs, fmt.Errorf(
			"%s changed since this session read it, and the old version could not be kept, "+
				"so it was not overwritten: %w", filepath.Base(c.Path), err)
	}
	if notice != nil && r.Kind != RestoreNone {
		notice(keptNotice(c.Path, r))
	}
	return KeepMine, nil
}

func keptNotice(path string, r Restore) string {
	where := r.Branch
	if r.Kind == RestoreSidecar {
		where = r.Sidecar
	}
	return fmt.Sprintf("wrote %s over a change this session did not make; the old version is at %s\n  %s",
		filepath.Base(path), where, r.HowTo)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/workledger -v`
Expected: PASS, every test in the package.

- [ ] **Step 5: Commit**

```bash
git add internal/workledger/resolve.go internal/workledger/resolve_test.go
git commit -m "workledger: resolve a conflict, or keep the old version and write

With a person there it asks, with the three options spelled out — a question
whose options are not spelled out is one people answer by habit. With nobody
there it writes, but only after the old version is recoverable, and it says
where that is.

The single refusal is the case where that promise cannot be kept: with nowhere
to save the old version, an unattended write does not proceed, because
overwriting somebody's work without asking is only acceptable while it is
recoverable."
```

---

### Task 7: The `work_conflict` approval, in all five places

**Files:**
- Modify: `internal/tui/view.go` (the no-"always" group in `handleAskKey`, and the title/hint switch)
- Modify: `internal/ui/repl.go` (`noAlwaysAction`, and the display switch)
- Modify: `cmd/commands.go` (`headlessApprover`'s two branches)
- Create: `cmd/workconflict_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `work_conflict` recognised as an action with no "always" in every
  approver.

- [ ] **Step 1: Write the failing test**

Create `cmd/workconflict_test.go`:

```go
package cmd

import (
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
)

// -y means "run the commands I chose", not "discard the other session's
// work". With nobody to ask, workledger.Resolve writes after keeping the old
// version — it never needs an answer, so there is nothing for -y to give.
func TestHeadlessNeverApprovesWorkConflict(t *testing.T) {
	cfg := config.Default()
	cfg.AutoApproveShell = true
	cfg.AutoApproveBrowser = true

	var ok bool
	out := captureStderr(t, func() {
		ok = headlessApprover(cfg)("work_conflict", "doc.md changed since this session read it")
	})
	if ok {
		t.Fatal("-y approved work_conflict")
	}
	if !strings.Contains(out, "refused work_conflict") {
		t.Errorf("work_conflict was refused by the wrong branch:\n%s", out)
	}
}

func TestHeadlessNonInteractiveWorkConflictDoesNotPromiseYes(t *testing.T) {
	cfg := config.Default()
	saved := stdinIsTTY
	stdinIsTTY = func() bool { return false }
	defer func() { stdinIsTTY = saved }()

	out := captureStderr(t, func() {
		headlessApprover(cfg)("work_conflict", "doc.md changed since this session read it")
	})
	if strings.Contains(out, "use -y") {
		t.Errorf("work_conflict always asks a person, so the hint must not promise -y:\n%s", out)
	}
}
```

And in `internal/ui`, create `internal/ui/workconflict_test.go`:

```go
package ui

import "testing"

// Each conflict is about a specific pair of changes, so a session-wide
// "always keep mine" is a licence to lose work silently — the thing this
// feature exists to stop.
func TestWorkConflictHasNoAlways(t *testing.T) {
	if !noAlwaysAction("work_conflict") {
		t.Error("work_conflict must be listed as having no \"always\"")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd -run TestHeadless.*WorkConflict -v && go test ./internal/ui -run TestWorkConflictHasNoAlways -v`
Expected: the `cmd` tests FAIL (an unclassified action falls through to the
non-TTY branch, so the message is the wrong one), and the `ui` test FAILS with
`work_conflict must be listed`.

The TUI's own guard also fails once the action is added to `handleAskKey`
without a display case — that is Step 4's check, and it is the guard working.

- [ ] **Step 3: Name the action in all five places**

`internal/ui/repl.go`, in `noAlwaysAction`, add `"work_conflict"` to the case
list, and in the display switch beside `case "pack_install":`:

```go
	case "work_conflict":
		fmt.Printf("%s\n%s\n", yell("another session changed this file:"), detail)
```

`internal/tui/view.go`, in `handleAskKey`'s no-"always" condition, add
`|| a.Action == "work_conflict"`, and in the title/hint switch:

```go
	case "work_conflict":
		// No "a": each conflict is a specific pair of changes, and a
		// session-wide "always keep mine" loses work silently.
		title = "Another session changed this file"
		hint = "y keep mine · n keep theirs · d show me · ↑↓ scroll"
		compactHint = "y/n/d · ↑↓"
```

`cmd/commands.go`, in `headlessApprover`, add `action == "work_conflict" ||`
to the `-y` group **and** to the non-TTY hint list — both, or the hint
promises that `-y` would work when it never does. Extend the comment:

```go
		// -y never approves work_conflict either (work ledger spec §8): with
		// nobody to ask, Resolve writes after making the old version
		// recoverable, so there is no answer for -y to give — only a record
		// to keep.
```

- [ ] **Step 4: Run the tests and the guard**

Run: `go test ./cmd ./internal/ui ./internal/tui -run 'WorkConflict|TestEveryNoAlwaysAction' -v`
Expected: PASS. `TestEveryNoAlwaysActionHasItsOwnTitleAndHint` is the guard
that the display case was not forgotten; if it names `work_conflict`, Step 3's
TUI case is missing or misspelled.

- [ ] **Step 5: Commit**

```bash
git add internal/tui/view.go internal/ui/repl.go internal/ui/workconflict_test.go cmd/commands.go cmd/workconflict_test.go
git commit -m "ui: work_conflict is an approval with no always

Each conflict is about a specific pair of changes, so a session-wide 'always
keep mine' would be a licence to lose work silently — the thing this feature
exists to stop. Named in both of headlessApprover's branches, because the
non-TTY one promises 'use -y to auto-approve' and -y never answers this."
```

---

### Task 8: Detection in the file tools

**Files:**
- Modify: `internal/tools/tool.go` (the `Registry` struct: add `OnConflict`)
- Modify: `internal/tools/fs.go` (`writeFileTool.Run`, `editFileTool.Run`, `readFileTool.Run`)
- Create: `internal/tools/conflict_test.go`

**Interfaces:**
- Consumes: nothing from `internal/workledger` — the seam is a hook, so this
  task adds no import.
- Produces:
  - `Registry.OnConflict func(ctx context.Context, absPath string) (keepMine bool, err error)`
  - `Registry.OnRead func(absPath string, content []byte)`
  - Both nil-safe: a nil hook is "no work ledger", which is the behaviour
    before this feature.

- [ ] **Step 1: Write the failing test**

Create `internal/tools/conflict_test.go`:

```go
package tools

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func regWithHooks(t *testing.T) (*Registry, string) {
	t.Helper()
	root := t.TempDir()
	r, err := NewRegistry(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	r.ApproveFileWrites = false
	return r, root
}

// A write asks the hook, and a "keep theirs" answer must not write.
func TestWriteStopsWhenTheConflictHookSaysKeepTheirs(t *testing.T) {
	r, root := regWithHooks(t)
	f := filepath.Join(root, "a.txt")
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	asked := ""
	r.OnConflict = func(_ context.Context, p string) (bool, error) {
		asked = p
		return false, nil
	}
	res := r.Dispatch(context.Background(), "write_file",
		map[string]any{"path": "a.txt", "content": "mine\n"})

	if asked != f {
		t.Errorf("the hook was asked about %q, want %q", asked, f)
	}
	body, _ := os.ReadFile(f)
	if string(body) != "theirs\n" {
		t.Errorf("the file was overwritten anyway: %q", body)
	}
	if !res.IsError {
		t.Error("a declined write should report that it did not happen")
	}
	if !strings.Contains(res.Content, "a.txt") {
		t.Errorf("the result should name the file: %q", res.Content)
	}
}

func TestWriteProceedsWhenTheHookSaysKeepMine(t *testing.T) {
	r, root := regWithHooks(t)
	f := filepath.Join(root, "a.txt")
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.OnConflict = func(context.Context, string) (bool, error) { return true, nil }

	if res := r.Dispatch(context.Background(), "write_file",
		map[string]any{"path": "a.txt", "content": "mine\n"}); res.IsError {
		t.Fatalf("the write was refused: %s", res.Content)
	}
	body, _ := os.ReadFile(f)
	if string(body) != "mine\n" {
		t.Errorf("the file holds %q", body)
	}
}

// The hook's error is the "nothing could be kept" case: do not write, and say
// why in words a person can act on.
func TestWriteStopsWhenTheHookErrors(t *testing.T) {
	r, root := regWithHooks(t)
	f := filepath.Join(root, "a.txt")
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.OnConflict = func(context.Context, string) (bool, error) {
		return false, errors.New("the old version could not be kept")
	}
	res := r.Dispatch(context.Background(), "write_file",
		map[string]any{"path": "a.txt", "content": "mine\n"})
	if !res.IsError {
		t.Fatal("a write whose restore point failed must not be reported as done")
	}
	if !strings.Contains(res.Content, "could not be kept") {
		t.Errorf("the reason should reach the model: %q", res.Content)
	}
	body, _ := os.ReadFile(f)
	if string(body) != "theirs\n" {
		t.Errorf("the file was overwritten: %q", body)
	}
}

// A nil hook is the behaviour before this feature: no detection, no change.
func TestNoHookWritesAsBefore(t *testing.T) {
	r, root := regWithHooks(t)
	f := filepath.Join(root, "a.txt")
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := r.Dispatch(context.Background(), "write_file",
		map[string]any{"path": "a.txt", "content": "mine\n"}); res.IsError {
		t.Fatalf("a write with no work ledger failed: %s", res.Content)
	}
	body, _ := os.ReadFile(f)
	if string(body) != "mine\n" {
		t.Errorf("the file holds %q", body)
	}
}

// edit_file goes through the same gate: it is a write.
func TestEditAsksTheConflictHookToo(t *testing.T) {
	r, root := regWithHooks(t)
	f := filepath.Join(root, "a.txt")
	if err := os.WriteFile(f, []byte("hello theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	called := false
	r.OnConflict = func(context.Context, string) (bool, error) { called = true; return false, nil }
	r.Dispatch(context.Background(), "edit_file",
		map[string]any{"path": "a.txt", "old": "theirs", "new": "mine"})
	if !called {
		t.Error("edit_file did not go through the conflict gate")
	}
}

// read_file feeds the ledger what it saw, or Changed can never fire.
func TestReadFeedsTheLedger(t *testing.T) {
	r, root := regWithHooks(t)
	f := filepath.Join(root, "a.txt")
	if err := os.WriteFile(f, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var sawPath string
	var sawBody []byte
	r.OnRead = func(p string, b []byte) { sawPath, sawBody = p, b }

	if res := r.Dispatch(context.Background(), "read_file",
		map[string]any{"path": "a.txt"}); res.IsError {
		t.Fatalf("read failed: %s", res.Content)
	}
	if sawPath != f {
		t.Errorf("OnRead saw %q, want %q", sawPath, f)
	}
	if string(sawBody) != "v1\n" {
		t.Errorf("OnRead saw %q", sawBody)
	}
}

// And a write records what it wrote, or our own next write looks like
// somebody else's change.
func TestWriteFeedsTheLedgerWhatItWrote(t *testing.T) {
	r, root := regWithHooks(t)
	var sawBody []byte
	r.OnRead = func(_ string, b []byte) { sawBody = b }
	if res := r.Dispatch(context.Background(), "write_file",
		map[string]any{"path": "a.txt", "content": "mine\n"}); res.IsError {
		t.Fatalf("write failed: %s", res.Content)
	}
	if string(sawBody) != "mine\n" {
		t.Errorf("after a write the ledger saw %q, want what we wrote", sawBody)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/tools -run 'TestWrite|TestEditAsks|TestReadFeeds|TestNoHook' -v`
Expected: FAIL to build — `r.OnConflict undefined`, `r.OnRead undefined`.

- [ ] **Step 3: Add the hooks to the Registry**

In `internal/tools/tool.go`, beside `OnBeforeWrite`:

```go
	// OnConflict is asked before a write when the work ledger is on: the file
	// changed since this session read it. true writes, false does not, and an
	// error means the old version could not be kept so the write must not
	// proceed. Nil is "no work ledger", which is the behaviour before that
	// feature.
	//
	// A hook rather than an import: internal/workledger reads internal/live
	// and internal/gitctx, and this package must not depend on either. It is
	// assigned in cmd/root.go, the same seam ReviewerFactory and
	// LoaderFactory use.
	OnConflict func(ctx context.Context, absPath string) (keepMine bool, err error)

	// OnRead records what this session last saw at a path — after a read, and
	// after a write, because what we wrote is what we last saw. Without it
	// the conflict check can never fire.
	OnRead func(absPath string, content []byte)
```

- [ ] **Step 4: Wire them in `fs.go`**

Add one helper beside `beforeWrite`:

```go
// conflict asks the work ledger about absPath before a write. The order
// matters: after approval, so a person is not asked twice about a write they
// already declined, and before beforeWrite, so the checkpointer does not
// snapshot a file this write is about to abandon.
func (r *Registry) conflict(ctx context.Context, absPath string) (Result, bool) {
	if r.OnConflict == nil {
		return Result{}, true
	}
	keepMine, err := r.OnConflict(ctx, absPath)
	if err != nil {
		rel, _ := filepath.Rel(r.Root, absPath)
		return Result{IsError: true, Content: fmt.Sprintf("%s was not written: %v", rel, err)}, false
	}
	if !keepMine {
		rel, _ := filepath.Rel(r.Root, absPath)
		return Result{IsError: true, Content: fmt.Sprintf(
			"%s was not written: it changed since this session read it and the other version was kept", rel)}, false
	}
	return Result{}, true
}

// noteRead tells the work ledger what this session has seen at absPath.
func (r *Registry) noteRead(absPath string, content []byte) {
	if r.OnRead != nil {
		r.OnRead(absPath, content)
	}
}
```

In `writeFileTool.Run`, between `approveWrite` and `beforeWrite`:

```go
	if res, ok := t.r.conflict(ctx, p); !ok {
		return res
	}
```

and after the successful `os.WriteFile`:

```go
	t.r.noteRead(p, []byte(content))
```

In `editFileTool.Run`, the same two additions at the same two points (after
its `approveWrite`, and after its write).

In `readFileTool.Run`, after the file is read successfully, call
`t.r.noteRead(p, raw)` with the **whole file's** bytes, not the range the tool
returns — the hash is of the file, and a ranged read that recorded only its
slice would make every later write look like a conflict.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/tools -run 'TestWrite|TestEditAsks|TestReadFeeds|TestNoHook' -v`
Expected: PASS, 7 tests.

- [ ] **Step 6: Run the whole package**

Run: `go test ./internal/tools`
Expected: ok. Every write in the tool suite now passes through `conflict`
with a nil hook, which must change nothing.

- [ ] **Step 7: Commit**

```bash
git add internal/tools/tool.go internal/tools/fs.go internal/tools/conflict_test.go
git commit -m "tools: a conflict gate on writes, and a read hook, both injected

A hook rather than an import: internal/workledger reads internal/live and
internal/gitctx, and this package must not depend on either, so cmd/root.go
assigns it — the seam ReviewerFactory and LoaderFactory already use.

The gate sits after approval and before the checkpoint: a person is not asked
twice about a write they already declined, and the checkpointer does not
snapshot a file the write is about to abandon. A nil hook is the behaviour
before this feature.

read_file records the whole file's bytes, not the range it returns, or a
ranged read would make every later write look like a conflict."
```

---

### Task 9: Wiring, the startup notice, and the config key

**Files:**
- Create: `internal/workledger/session.go`
- Create: `internal/workledger/session_test.go`
- Modify: `internal/config/config.go` (add `WorkConfig`, the field, the default)
- Modify: `cmd/root.go` (build the ledger in `buildAgent`, assign the hooks, the notice)
- Modify: `cmd/root.go`'s `finishSession` (remove the record, flush the hashes)
- Modify: `internal/packs/trust.go:readState` (feed `Seen`) — via the same hook pattern
- Create: `internal/config/work_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1, 2, 4 and 6.
- Produces:
  - `type Ledger struct{ ... }`
  - `func Open(workspace string, cfg Options) (*Ledger, error)`
  - `type Options struct { Enabled bool; KeepOverwritten time.Duration; PID int; Session, Model string }`
  - `func (l *Ledger) Others() []Record`
  - `func (l *Ledger) NoticeLine() string`
  - `func (l *Ledger) OnRead(path string, content []byte)`
  - `func (l *Ledger) OnConflict(ctx context.Context, path string, ask Asker, fired bool, notice func(string)) (bool, error)`
  - `func (l *Ledger) Turn(branch, task string, writing []string)`
  - `func (l *Ledger) Close() error`
  - `config.WorkConfig{Enabled bool; KeepOverwritten string}`

- [ ] **Step 1: Write the failing test**

Create `internal/workledger/session_test.go`:

```go
package workledger

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func openFor(t *testing.T, ws string) *Ledger {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // never the real ~/.be-code
	l, err := Open(ws, Options{Enabled: true, KeepOverwritten: time.Hour, PID: 4242, Session: "ABC123", Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	return l
}

func TestOpenWritesThisSessionsRecordAndCloseRemovesIt(t *testing.T) {
	ws := t.TempDir()
	l := openFor(t, ws)

	dir, err := Dir()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d records, want this session's one: %+v", len(entries), entries)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	entries, _ = os.ReadDir(dir)
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".seen.json") {
			t.Errorf("Close left %s behind", e.Name())
		}
	}
}

// Disabled means the behaviour before this feature: no record, no detection.
func TestDisabledWritesNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	l, err := Open(ws, Options{Enabled: false, PID: 4242})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if dir, derr := Dir(); derr == nil {
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Errorf("a disabled ledger wrote %d files", len(entries))
		}
	}
	f := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(f, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.OnRead(f, []byte("v1\n"))
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keepMine, err := l.OnConflict(context.Background(), f, nil, false, nil)
	if err != nil || !keepMine {
		t.Errorf("a disabled ledger must never block a write: %v %v", keepMine, err)
	}
}

// The notice says what the other session is doing, not merely that it exists:
// it competes with every other startup notice, and "a session exists" is not
// worth reading.
func TestNoticeLineNamesWhatTheOtherSessionIsDoing(t *testing.T) {
	ws := t.TempDir()
	l := openFor(t, ws)
	dir, _ := Dir()
	stubAlive(t, map[int]bool{4242: true, 99: true})
	other := Record{
		PID: 99, Workspace: ws, Branch: "feat/thing", Task: "003",
		Started: time.Now().UTC().Format(time.RFC3339),
		Updated: time.Now().UTC().Format(time.RFC3339),
	}
	if err := Save(dir, other); err != nil {
		t.Fatal(err)
	}

	line := l.NoticeLine()
	for _, want := range []string{"99", "feat/thing", "003"} {
		if !strings.Contains(line, want) {
			t.Errorf("the notice should mention %q: %q", want, line)
		}
	}
}

// A session alone says nothing at all: a notice on every single session would
// be noise that teaches people to skip notices.
func TestNoticeLineIsEmptyWhenAlone(t *testing.T) {
	l := openFor(t, t.TempDir())
	stubAlive(t, map[int]bool{4242: true})
	if line := l.NoticeLine(); line != "" {
		t.Errorf("a session alone said %q", line)
	}
}

// End to end through the Ledger: read, somebody else writes, our write asks.
func TestConflictEndToEnd(t *testing.T) {
	ws := gitRepo(t)
	l := openFor(t, ws)
	f := filepath.Join(ws, "doc.md")
	if err := os.WriteFile(f, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.OnRead(f, []byte("v1\n"))
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	asked := 0
	keepMine, err := l.OnConflict(context.Background(), f,
		func(string, string) bool { asked++; return true }, false, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if asked != 1 {
		t.Errorf("asked %d times, want 1", asked)
	}
	if !keepMine {
		t.Error("a yes should keep mine")
	}
}

// An unchanged file is never a conflict and never a question.
func TestNoConflictAsksNothing(t *testing.T) {
	ws := gitRepo(t)
	l := openFor(t, ws)
	f := filepath.Join(ws, "doc.md")
	if err := os.WriteFile(f, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	l.OnRead(f, []byte("v1\n"))

	keepMine, err := l.OnConflict(context.Background(), f,
		func(string, string) bool { t.Fatal("asked about a file nobody changed"); return false },
		false, nil)
	if err != nil || !keepMine {
		t.Errorf("an unchanged file must pass: %v %v", keepMine, err)
	}
}
```

Create `internal/config/work_test.go`:

```go
package config

import "testing"

func TestWorkDefaults(t *testing.T) {
	c := Default()
	if !c.Work.Enabled {
		t.Error("the work ledger should be on by default")
	}
	if c.Work.KeepOverwritten != "168h" {
		t.Errorf("keep_overwritten = %q, want 168h", c.Work.KeepOverwritten)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/workledger -run 'TestOpen|TestDisabled|TestNotice|TestConflictEnd|TestNoConflict' -v && go test ./internal/config -run TestWorkDefaults -v`
Expected: FAIL to build — `undefined: Open`, `undefined: Ledger`, and
`c.Work undefined`.

- [ ] **Step 3: Write the session type**

Create `internal/workledger/session.go`:

```go
package workledger

import (
	"context"
	"fmt"
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
	opt     Options
	ws      string
	dir     string
	rec     Record
	seen    *Seen
	warned  bool
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
		// once through Warn.
		l.seen = NewSeen(filepath.Join(os.TempDir(), fmt.Sprintf("be-code-seen-%d.json", opt.PID)))
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
	l.seen = NewSeen(strings.TrimSuffix(l.rec.Path(dir), ".json") + ".seen.json")
	if err := Save(dir, l.rec); err != nil {
		l.note("could not record this session's work: " + err.Error())
	}
	if opt.KeepOverwritten > 0 {
		PruneSidecars(dir, opt.KeepOverwritten)
	}
	return l, nil
}

func (l *Ledger) note(s string) {
	if l.warned || l.Warn == nil {
		return
	}
	l.warned = true // one notice, not one per attempt
	l.Warn(s)
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

// OnRead records what this session last saw at path.
func (l *Ledger) OnRead(path string, content []byte) {
	if !l.opt.Enabled || l.seen == nil {
		return
	}
	l.seen.Note(path, content)
}

// OnConflict is the tools hook: has path changed since this session read it,
// and if so what happens. true means write.
func (l *Ledger) OnConflict(ctx context.Context, path string, ask Asker, fired bool, notice func(string)) (bool, error) {
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
	}, ask, fired, notice)
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
```

- [ ] **Step 4: Add the config section**

In `internal/config/config.go`, beside `PacksConfig`:

```go
// WorkConfig covers the record of what other sessions are doing, and what
// happens when two writers meet.
type WorkConfig struct {
	// Enabled writes this session's record and reads others'. Off means no
	// record, no startup notice and no conflict detection — the behaviour
	// before this feature.
	Enabled bool `json:"enabled"`
	// KeepOverwritten is how long a dotdir sidecar of an overwritten file is
	// kept, as a duration. Empty means the default.
	KeepOverwritten string `json:"keep_overwritten"`
}
```

Add the field to `Config` beside `Packs`:

```go
	Work             WorkConfig       `json:"work"`
```

and to `Default()`:

```go
		Work:             WorkConfig{Enabled: true, KeepOverwritten: "168h"},
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/workledger ./internal/config -v 2>&1 | tail -20`
Expected: PASS, every test in both packages.

- [ ] **Step 6: Wire it in `cmd/root.go`**

In `buildAgent`, after the registry exists and before `agent.New` (so the
hooks are set before anything can write), and keeping the ledger on the same
variable the session teardown can reach — `ideSession` is the existing
precedent for a per-run object `finishSession` closes:

```go
	keep := 168 * time.Hour
	if d, err := time.ParseDuration(cfg.Work.KeepOverwritten); err == nil && d > 0 {
		keep = d
	}
	workLedger, _ = workledger.Open(reg.Root, workledger.Options{
		Enabled: cfg.Work.Enabled,
		KeepOverwritten: keep,
		PID:     os.Getpid(),
		Model:   cfg.Model,
	})
	workLedger.Warn = func(s string) { fmt.Fprintln(os.Stderr, "note: "+s) }
	reg.OnRead = workLedger.OnRead
	reg.OnConflict = func(ctx context.Context, p string) (bool, error) {
		// The asker is the registry's own seam, so the question reaches
		// whichever UI is running; fired turns take the unattended path.
		return workLedger.OnConflict(ctx, p, workledger.Asker(reg.Approve), reg.Fired(), ag.QueueNotice)
	}
```

Declare `var workLedger *workledger.Ledger` beside the other package-level
per-run objects (`ideSession`).

In `finishSession`, beside `StopAllSubAgents` and `StopSchedules`:

```go
	if workLedger != nil {
		_ = workLedger.Close()
	}
```

And after `ExplainBudget`, the notice:

```go
	if line := workLedger.NoticeLine(); line != "" {
		ag.QueueNotice(line)
	}
```

- [ ] **Step 7: Feed `Seen` from the dotdir readers**

`internal/packs/trust.go:readState` is the pack trust file's one reader. It
cannot import `workledger`, so it gets the same hook treatment — one package
var, assigned in `cmd/root.go`:

```go
// OnStateRead is told the bytes of the trust file whenever it is read, so the
// work ledger can tell a later outside change from our own write. Nil in
// production until cmd/root.go assigns it.
var OnStateRead func(path string, content []byte)
```

Call it at the end of `readState` with the bytes it read, and in `cmd/root.go`:

```go
	packs.OnStateRead = workLedger.OnRead
```

- [ ] **Step 8: Run everything**

Run: `make -f build.mk verify`
Expected: vet clean, build ok, every package ok.

- [ ] **Step 9: Commit**

```bash
git add internal/workledger/session.go internal/workledger/session_test.go internal/config/config.go internal/config/work_test.go internal/packs/trust.go cmd/root.go
git commit -m "workledger: wire the ledger into a session

Open writes this session's record, finishSession removes it, and the startup
notice says what another session is *doing* rather than that one exists — it
competes with every other startup notice, and a notice on every second
terminal would teach people to skip them.

A disabled ledger is inert, which is the behaviour before this feature, and
nothing here can fail a session: a record that cannot be written costs one
notice and detection carries on in memory."
```

---

### Task 10: The remaining read sites — the engine's documents and the session store

**Files:**
- Modify: `internal/engine/store.go:437` (`loadDocs`), `:479` and `:938` (where `s.docs[name]` is set)
- Modify: `internal/store/sessions.go:127` (`Save`), `:147` (`Load`)
- Create: `internal/engine/seen_test.go`
- Create: `internal/store/seen_test.go`

**Interfaces:**
- Consumes: nothing from `internal/workledger` — both packages get the same
  package-var hook `internal/packs` got in Task 9, for the same reason.
- Produces:
  - `engine.OnDocHash func(path, hash string)`
  - `store.OnSessionRead func(path string, content []byte)`

**Why this task exists.** Spec §6 names five read sites and §10 names three
wiring places. Tasks 8 and 9 covered `fs.go`'s read and write and
`packs.readState`. These are the other two, and they are not optional: a file
this session never *saw* is never a conflict, so a missing `Seen` is a
detection that silently never fires — exactly the class of defect the pack
loader shipped when `Checks` kept gating on `detect`.

The engine's case is the one the spec calls deliberate reuse: it already
computes `hashBytes` for every document it loads and writes
(`store.go:479`, `:938`) for precisely this purpose. A feeds *that* hash
rather than computing a second one that could disagree — and where they ever
differ, `state.Docs` is the truth, because it is the one the engine acts on.

- [ ] **Step 1: Write the failing test for the engine**

Create `internal/engine/seen_test.go`:

```go
package engine

import "testing"

// The engine already hashes every document it loads and writes so an outside
// edit is recognisable (store.go's package comment). The work ledger needs
// the same hash, and must get the engine's rather than computing its own —
// two hashes of one file that could disagree is worse than one.
func TestLoadDocsReportsItsHashes(t *testing.T) {
	seen := map[string]string{}
	saved := OnDocHash
	OnDocHash = func(path, hash string) { seen[path] = hash }
	t.Cleanup(func() { OnDocHash = saved })

	st := storeWithTasks(t, map[string]string{
		"001-thing.md": "# 001 — thing\n\n- [ ] 1. thing\n",
	})
	_ = st

	if len(seen) != 1 {
		t.Fatalf("loadDocs reported %d hashes, want 1: %v", len(seen), seen)
	}
	for path, hash := range seen {
		if hash == "" {
			t.Errorf("%s was reported with an empty hash", path)
		}
		if !hasSuffix(path, "001-thing.md") {
			t.Errorf("the path should be the document's: %q", path)
		}
	}
}

func hasSuffix(s, suf string) bool { return len(s) >= len(suf) && s[len(s)-len(suf):] == suf }

// A nil hook is the behaviour before this feature.
func TestLoadDocsWithNoHookIsFine(t *testing.T) {
	saved := OnDocHash
	OnDocHash = nil
	t.Cleanup(func() { OnDocHash = saved })
	storeWithTasks(t, map[string]string{"001-thing.md": "# 001 — thing\n\n- [ ] 1. thing\n"})
}
```

`storeWithTasks` is this package's own fixture helper for a store with task
documents on disk. **Read `internal/engine`'s existing tests for its real
name before writing this file** — the package has several store fixtures, and
using the wrong one is a build error, not a silent problem. If none takes a
map of documents, build the store the way the nearest existing test does and
write the one document by hand.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/engine -run TestLoadDocs -v`
Expected: FAIL to build — `undefined: OnDocHash`.

- [ ] **Step 3: Add the engine's hook**

In `internal/engine/store.go`, beside the package's other vars:

```go
// OnDocHash is told each task document's path and the hash the engine
// computed for it, so the work ledger can tell a later outside change from
// the engine's own write. Nil in production until cmd/root.go assigns it.
//
// It is the engine's hash deliberately: internal/engine already keeps one per
// document "so an outside edit is recognisable", and a second hash of the
// same bytes could disagree with it. Where they ever differ, this one wins —
// it is the one the engine acts on.
var OnDocHash func(path, hash string)
```

At `store.go:479`, where `s.docs[name] = hashBytes(b)`:

```go
		s.docs[name] = hashBytes(b)
		if OnDocHash != nil {
			OnDocHash(filepath.Join(dir, name), s.docs[name])
		}
```

And at `:938`, where `s.docs[name] = hashBytes([]byte(w.body))` after a write,
the same two lines — what the engine wrote is what the session last saw, or
the next write would look like somebody else's change.

- [ ] **Step 4: Run it to verify it passes**

Run: `go test ./internal/engine -run TestLoadDocs -v && go test ./internal/engine`
Expected: PASS, then the whole package ok. The engine is where a wrong hash
would be felt, so the full package matters here.

- [ ] **Step 5: Write the failing test for the session store**

Create `internal/store/seen_test.go`:

```go
package store

import (
	"strings"
	"testing"
)

// Spec §10: the session file's load and save are a read site too. The
// HostPID guard already handles the dangerous half — a different live host
// owning the file disables autosave — and this only feeds the ledger what it
// saw, so a notice can name what that host is doing.
func TestSaveAndLoadReportWhatTheySaw(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var paths []string
	saved := OnSessionRead
	OnSessionRead = func(p string, _ []byte) { paths = append(paths, p) }
	t.Cleanup(func() { OnSessionRead = saved })

	s := &Session{ID: "sess-seen-test"}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("Save reported %d reads, want 1: %v", len(paths), paths)
	}
	if !strings.Contains(paths[0], "sess-seen-test") {
		t.Errorf("the path should be the session's: %q", paths[0])
	}

	paths = nil
	if _, err := Load("sess-seen-test"); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Errorf("Load reported %d reads, want 1: %v", len(paths), paths)
	}
}

func TestNoHookIsFine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	saved := OnSessionRead
	OnSessionRead = nil
	t.Cleanup(func() { OnSessionRead = saved })
	s := &Session{ID: "sess-nohook"}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 6: Run it to verify it fails**

Run: `go test ./internal/store -run 'TestSaveAndLoadReport|TestNoHookIsFine' -v`
Expected: FAIL to build — `undefined: OnSessionRead`.

- [ ] **Step 7: Add the store's hook**

In `internal/store/sessions.go`:

```go
// OnSessionRead is told the session file's bytes whenever it is written or
// read, so the work ledger can tell a later outside change from our own
// write. Nil in production until cmd/root.go assigns it.
var OnSessionRead func(path string, content []byte)
```

Call it in `Save` with the bytes it wrote and the path it wrote them to, and
in `Load` with the bytes it read. Both are nil-safe.

- [ ] **Step 8: Run both packages**

Run: `go test ./internal/store ./internal/engine`
Expected: ok, both.

- [ ] **Step 9: Assign both hooks in `cmd/root.go`**

Beside `packs.OnStateRead = workLedger.OnRead` from Task 9:

```go
	store.OnSessionRead = workLedger.OnRead
	// The engine hands its own hash rather than its bytes, so the ledger
	// records the hash the engine acts on rather than one of its own.
	engine.OnDocHash = func(path, hash string) { workLedger.NoteHash(path, hash) }
```

`Ledger` needs the matching method, beside `OnRead` in
`internal/workledger/session.go`:

```go
// NoteHash records a hash computed elsewhere — the engine's, for a task
// document it already hashed.
func (l *Ledger) NoteHash(path, hash string) {
	if !l.opt.Enabled || l.seen == nil {
		return
	}
	l.seen.NoteHash(path, hash)
}
```

- [ ] **Step 10: Run everything**

Run: `make -f build.mk verify`
Expected: vet clean, build ok, every package ok.

- [ ] **Step 11: Commit**

```bash
git add internal/engine/store.go internal/engine/seen_test.go internal/store/sessions.go internal/store/seen_test.go internal/workledger/session.go cmd/root.go
git commit -m "engine, store: feed the work ledger what they read

The last two of the spec's five read sites. A file this session never saw is
never a conflict, so a missing Seen is a detection that silently never fires.

The engine hands over the hash it already computed for each document rather
than its bytes: it keeps one per document so an outside edit is recognisable,
and a second hash of the same bytes could disagree with the one the engine
acts on.

Both are package-var hooks, like packs.OnStateRead: neither package may
depend on internal/workledger, which reads internal/live and internal/gitctx.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_013uAqvM8PBXHSGRrh7SsBjK"
```

---

### Task 11: Documentation

**Files:**
- Modify: `README.md` (the config reference, and a section on the work ledger)
- Modify: `CLAUDE.md` (an architecture section)

**Interfaces:**
- Consumes: everything.
- Produces: nothing in code.

This task exists because the pack loader shipped without it and the reviewer
was right to call it out: none of that feature's invariants were written where
the next session would read them.

- [ ] **Step 1: The README config entries**

Beside `packs.source` in the config reference:

```markdown
- `work.enabled` (true) — records what this session is working on under
  `~/.be-code/work/`, says once at startup when another session is working in
  the same workspace, and asks before a write would overwrite a change that
  arrived since this session read the file. `false` is the behaviour before
  the work ledger: no record, no notice, no detection.
- `work.keep_overwritten` (`168h`) — how long a copy of an overwritten dotdir
  file is kept beside it. Workspace files go to a `be-code/pre-overwrite/…`
  git branch instead, which is never pruned.
```

- [ ] **Step 2: The README section**

After the packs section, a short one:

```markdown
### Another session is working here

BE-Code records what each running session is doing — workspace, branch, the
task it is on — under `~/.be-code/work/`, and says so once when you open a
second session in the same workspace. The records are advisory, not locks: a
lock that outlives a crashed process is worse than a lost update.

When a write would overwrite a change that arrived since this session read the
file, it asks: **keep mine**, **keep theirs**, or **show me** the difference.
With nobody to ask — a scheduled event, or a headless run — it writes, but
first makes the old version recoverable: a `be-code/pre-overwrite/<stamp>`
branch for a file in the workspace, a timestamped copy beside it for anything
else, and the notice names which and gives the command that restores it. The
one time it refuses is when the old version could not be kept at all, because
overwriting unattended is only acceptable while it is recoverable.
```

- [ ] **Step 3: The CLAUDE.md architecture section**

After the pack sections, with the invariants a future session must not undo:

```markdown
### The work ledger (A)

`internal/workledger` records what each session is doing
(`~/.be-code/work/<pid>-<started>.json`, 0600, pruned by `procattr.Alive`) and
decides what happens when two writers meet. Spec:
`docs/superpowers/specs/2026-10-09-work-ledger-design.md`.

**Two different events are called a conflict and only one prompts.** A
*mechanical race* — two processes' read-modify-write interleaving on a delta
file — loses nothing that re-reading recovers, so `internal/packs/trust.go`'s
`mutate` compares the file's bytes against what the attempt read and retries,
three times, then errors. It never prompts: a modal raised on a microsecond
interleaving is noise nobody can answer, and people who learn to dismiss it
will dismiss the one that matters. A *write over somebody's decision or edit*
raises `work_conflict`, which has no "always" and which `-y` never answers.

**Detection is `Seen`/`Changed`**, a hash of each file as this session last
read or wrote it — the same thing `internal/engine`'s `state.Docs` already
does for task documents, and the engine feeds its hash in through `NoteHash`
rather than hashing twice. **A file this session never saw is never a
conflict**, so a missed `Note` is a detection that silently never fires; the
call sites are `fs.go`'s read and write paths and `packs.OnStateRead`.

**With nobody to ask, write last — but keep the old version first.**
`workledger.Resolve` with a nil asker or during a fired turn calls `KeepOld`,
which snapshots a workspace file to a `gitctx.SnapshotPrefixOverwrite` branch
and copies anything else to a sidecar, then writes and notices where it went.
**If `KeepOld` fails, the write does not proceed** — the unattended path is
only acceptable because it is recoverable, so losing the restore point turns
it back into a refusal.

`internal/tools` must never import `internal/workledger`: the seams are
`Registry.OnConflict` and `Registry.OnRead`, assigned in `cmd/root.go`, the
same dodge `ReviewerFactory` and `LoaderFactory` use. The ledger reads
`internal/live` (`SameWorkspace`, because two shells spell one folder
differently) and `internal/gitctx`, neither of which `tools` may depend on.

Sub-projects **B** (sub-agent scope overlap) and **C** (teammates through the
repo) are not built; C is deferred with its reasoning in the spec's §3.
```

- [ ] **Step 4: Check the claims**

Run: `make -f build.mk verify && grep -n 'work.enabled\|work.keep_overwritten' README.md && grep -n 'The work ledger' CLAUDE.md`
Expected: verify green, and both documents carry what Steps 1-3 added. Read
the two new sections once against the code: every file path, function name and
default named in them must exist.

- [ ] **Step 5: Commit**

```bash
git add README.md CLAUDE.md
git commit -m "docs: the work ledger, and the invariants that keep it honest

The pack loader shipped with none of its invariants written where the next
session reads them, and the reviewer was right to call it out. This one
records the two that are easiest to undo by accident: a mechanical race
retries and never prompts, and an unattended write that cannot keep the old
version does not happen."
```

---

## Notes for whoever executes this

- **Run `make -f build.mk verify` before calling any task done.**
- **No test may write to the real `~/.be-code`.** Every test that can reach
  `config.Dir()` pins `HOME` with `t.Setenv`. Check
  `stat -c %Y ~/.be-code/config.json` before and after a full run.
- **`internal/agent` is the slowest package and sensitive to CPU contention.**
  A timeout there under load is usually contention; re-run it alone before
  believing a failure.
- Tasks 1–4 and 6 are pure additions to a new package and touch nothing
  existing. Tasks 5, 7, 8, 9 and 10 modify shared code; run the whole package
  after each, not just the new test.
- Task 10's engine test needs this package's own store fixture by its real
  name. Read `internal/engine`'s existing tests first: the package has several
  and guessing is a build error.
- Task 5's test drives the interleaving through `beforeWriteHook` rather than
  hoping for it. If that hook is ever removed, the race it tests becomes
  untestable.
