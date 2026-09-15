# Task Handling Engine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop BE-Code's primary model re-reading files after compaction by giving the harness a per-workspace working memory (file digests, task ledger, lookup cache, durable notes) that it captures on its own, re-injects into the system prompt, and backs with git-based lookup tools.

**Architecture:** A new `internal/engine` package owns the store (four files under `~/.be-code/engine/<key>/`, one mutex, atomic writes). The agent calls it from three seams: `dispatch` (observe every tool result, serve cached lookups, append footers), `composeSystem` (render the `Working memory` block after the repo map), and `Compact` (digested reads become one-line stubs; the summary's `files:` block feeds notes back). A flat `task` tool and four read-only git tools (`lookup`, `history`, `show`, `changes`) live in `internal/tools`; `cmd/root.go` opens the store once the session id is known and registers the tools; both UIs gain `/task` and `/notes`.

**Tech Stack:** Go 1.22+, stdlib only (`crypto/sha256`, `encoding/json`, `os/exec` via `tools.RunShell`); the repo map's regex extractors; git on PATH for the git tools (with fallbacks).

**Spec:** `docs/superpowers/specs/2026-09-15-task-engine-design.md`

## Global Constraints

- Every task leaves `make -f build.mk verify` green; tests touching HOME rely on the package `TestMain` guards (present in agent, tui, ui, cmd, config, store, ide) and never write to the real `~/.be-code`; new packages that touch HOME add the same guard (copy `internal/agent/testmain_test.go`).
- Store location `~/.be-code/engine/<key>/`, key = first 16 hex chars of SHA-256 of the cleaned absolute workspace path. Files: `digests.json`, `ledger.json`, `lookups.json` (session scope), `notes.md` (durable). Atomic writes (temp + rename).
- Bounds: 200 digests and 256 KiB of `digests.json` (evict least-recently-touched); `notes.md` 4096 bytes trimmed at a line boundary, oldest first; 20 lookups.
- Exact strings: footer `already read at turn N (unchanged); outline and notes are in your context`; footer `(cached; files unchanged)`; markers `[changed since read]` and `[you edited this]`; block header `Working memory`; compaction instruction `Do not restate anything already in Working memory.`; digested stub `(read <path> lines a–b; digested)`; task errors `task needs an action (plan, step, note)`, `step %d does not exist (%d steps)`, `note needs text`; engine notice `engine: %v; continuing without working memory`; git error `not a git repository`; corrupt file suffix `.broken-<yyyymmdd-hhmmss>`.
- Config: `engine {enabled: true, budget: 6144, notes_cap: 4096, tools: "full"}`; `Load()` fills zero budgets and an empty `tools`; `tools: "minimal"` registers only `task` and `lookup`.
- `engine` must not import `agent` or `tools`; `tools` must not import `agent` or `engine` (tools talks to the store through small interfaces it defines).
- Git tools: 20 s timeout per call, confined to `Registry.Root`, never prompt, output capped at `Registry.MaxOutput`; all four join `planAgent`'s subset.
- Version 0.10.0; CHANGELOG entry `## v0.10.0 — working memory`.

---

## File structure

- Create `internal/repomap/outline.go` — `Outline(name string, data []byte) []string`: the per-file symbol extraction the `Build` loop already does, exported.
- Create `internal/engine/store.go` — types, `Open`/`OpenAt`, load/save, session scoping, eviction, notes cap, corrupt recovery.
- Create `internal/engine/observe.go` — `Observe`, `Cached`, digest and lookup capture, footers.
- Create `internal/engine/render.go` — `Render` (the block), `LedgerText`, `StoppedAt`, `ApplyFileNotes`, `ParsePlanSteps`.
- Create `internal/engine/store_test.go`, `observe_test.go`, `render_test.go`, `testmain_test.go`.
- Modify `internal/config/config.go` — `EngineConfig`.
- Modify `internal/agent/loop.go` — `Engine` field, `SetEngine`, `composeSystem` block, `dispatch` observe/cached, turn counter + flush, `RunFull` seeding + baseline, `Compact` changes.
- Modify `internal/agent/handoff.go` — `Resume` rebuild, `Stopped at:` in `WriteHandoff`.
- Modify `internal/agent/extras.go` — plan-step seeding in `ExecutePlan`; subset gains the git tools.
- Modify `internal/agent/prompt.go` — task guidance sentences.
- Modify `internal/gitctx/gitctx.go` — `Head`, `Porcelain`.
- Create `internal/tools/task.go`, `internal/tools/gittools.go` (+ tests).
- Modify `cmd/root.go` — open the store, register tools, flush on exit.
- Modify `internal/ui/common.go`, `internal/ui/repl.go`, `internal/tui/view.go` — `/task`, `/notes`.
- Modify `test/e2e/mock_server.py`, `test/e2e/run_e2e.sh`, `README.md`, `CHANGELOG.md`, `build.mk`, `docs/live-checklist.md`, root `CLAUDE.md`.

---

### Task 1: The store

**Files:**
- Create: `internal/repomap/outline.go`, `internal/engine/store.go`, `internal/engine/store_test.go`, `internal/engine/testmain_test.go`
- Modify: `internal/repomap/repomap.go:75-130` (call `Outline` from `Build`), `internal/config/config.go` (`EngineConfig`)
- Test: `internal/engine/store_test.go`, `internal/config/config_test.go` (append)

**Interfaces:**
- Consumes: `repomap` extractors table.
- Produces:
  - `repomap.Outline(name string, data []byte) []string`
  - `config.EngineConfig{Enabled bool; Budget, NotesCap int; Tools string}` as `Config.Engine` (`json:"engine"`), defaults `{true, 6144, 4096, "full"}`.
  - `engine.Key(root string) string`; `engine.Open(root, sessionID string, resumed bool, notesCap int) (*Store, error)`; `engine.OpenAt(dir, root, sessionID string, resumed bool, notesCap int) (*Store, error)`.
  - `engine.Range{From, To int}`, `engine.Digest{Path, Hash string; Size, ModTime int64; Outline []string; Ranges []Range; Note string; Turn int; Edited bool}`, `engine.Step{Text, Status string}`, `engine.Baseline{Head, Dirty string}`, `engine.Ledger{Task string; Steps []Step; Decisions, Facts []string; Baseline Baseline; Session string}`, `engine.Hit{File string; Line int; Text string}`, `engine.Lookup{Tool, Query string; Hits []Hit; Hashes map[string]string; Turn int}`.
  - `(*Store) Flush() error`, `Dir() string`, `NextTurn() int`, `Turn() int`, `Ledger() Ledger` (copy), `SetPlan(task string, steps []string)`, `EnsureTask(text string)`, `SetStep(i int, status string) error`, `AddNote(text, file string, decision, keep bool) error`, `SetBaseline(b Baseline)`, `Notes() string`, `AddNoteLine(text string)`, `DropNote(n int) error`, `ClearNotes()`, `ClearSession()`, `Digests() []Digest` (copy, most recently touched first), `Lookups() []Lookup` (copy, newest first).

- [ ] **Step 1: Export the per-file outline**

Create `internal/repomap/outline.go`:

```go
package repomap

import (
	"path/filepath"
	"strings"
)

// Outline extracts the symbol names Build would list for one file. name is
// used for its extension only; an extension with no extractor yields nil.
func Outline(name string, data []byte) []string {
	exts, ok := extractors[strings.ToLower(filepath.Ext(name))]
	if !ok || len(data) > maxFileSize {
		return nil
	}
	src := string(data)
	var syms []string
	seen := map[string]bool{}
	for _, ex := range exts {
		for _, match := range ex.re.FindAllStringSubmatch(src, -1) {
			n := match[len(match)-1]
			if n == "" || seen[n] || n == "init" || n == "main" && len(syms) > 0 {
				continue
			}
			seen[n] = true
			syms = append(syms, n)
			if len(syms) >= maxSymbolsFile {
				return syms
			}
		}
	}
	return syms
}
```

In `Build`, replace the inline extraction loop (from `src := string(data)` through the closing of the `for _, ex := range exts` loop) with `syms := Outline(d.Name(), data)`. Run `go test ./internal/repomap/` — PASS (behaviour unchanged).

- [ ] **Step 2: Config**

In `internal/config/config.go`, after `CoworkConfig`:

```go
// EngineConfig tunes the working-memory engine (internal/engine).
type EngineConfig struct {
	Enabled bool `json:"enabled"`
	// Budget caps the Working memory block in the system prompt, in bytes.
	Budget int `json:"budget"`
	// NotesCap caps the durable notes.md, in bytes.
	NotesCap int `json:"notes_cap"`
	// Tools is "full" (task, lookup, history, show, changes) or "minimal"
	// (task and lookup only) for tight compat-mode prompts.
	Tools string `json:"tools"`
}
```

Add `Engine EngineConfig \`json:"engine"\`` to `Config` after `Cowork`; in `Default()` set `Engine: EngineConfig{Enabled: true, Budget: 6144, NotesCap: 4096, Tools: "full"}`; in `Load()` after the cowork fills:

```go
	if cfg.Engine.Budget == 0 {
		cfg.Engine.Budget = 6144
	}
	if cfg.Engine.NotesCap == 0 {
		cfg.Engine.NotesCap = 4096
	}
	if cfg.Engine.Tools == "" {
		cfg.Engine.Tools = "full"
	}
```

Append to `internal/config/config_test.go`:

```go
func TestEngineDefaultsAndLoadFill(t *testing.T) {
	d := Default()
	if !d.Engine.Enabled || d.Engine.Budget != 6144 || d.Engine.NotesCap != 4096 || d.Engine.Tools != "full" {
		t.Fatalf("defaults: %+v", d.Engine)
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".be-code"), 0o755)
	os.WriteFile(filepath.Join(home, ".be-code", "config.json"), []byte(`{"engine":{"enabled":false,"budget":0,"tools":""}}`), 0o600)
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Engine.Enabled || cfg.Engine.Budget != 6144 || cfg.Engine.NotesCap != 4096 || cfg.Engine.Tools != "full" {
		t.Fatalf("load fill: %+v", cfg.Engine)
	}
}
```

Run: `go test ./internal/config/` — PASS.

- [ ] **Step 3: Write the failing store tests**

Create `internal/engine/testmain_test.go` as a copy of `internal/agent/testmain_test.go` (package `engine`). Create `internal/engine/store_test.go`:

```go
package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func openTest(t *testing.T, session string, resumed bool) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(t.TempDir(), "engine")
	s, err := OpenAt(dir, root, session, resumed, 4096)
	if err != nil {
		t.Fatal(err)
	}
	return s, root
}

func TestKeyIsStableAndShort(t *testing.T) {
	a, b := Key("/tmp/x/../y"), Key("/tmp/y")
	if a != b || len(a) != 16 {
		t.Fatalf("key %q vs %q", a, b)
	}
}

func TestLedgerAndNotesRoundTrip(t *testing.T) {
	s, root := openTest(t, "s1", false)
	s.SetPlan("add a flag", []string{"parse", "wire", "test"})
	if err := s.SetStep(2, "doing"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStep(4, "done"); err == nil || err.Error() != "step 4 does not exist (3 steps)" {
		t.Fatalf("bad step error: %v", err)
	}
	if err := s.AddNote("config lives in internal/config", "", false, true); err != nil {
		t.Fatal(err)
	}
	if err := s.AddNote("", "", false, false); err == nil || err.Error() != "note needs text" {
		t.Fatalf("empty note: %v", err)
	}
	s.AddNote("use cobra", "", true, false)
	s.SetBaseline(Baseline{Head: "abc", Dirty: "d1"})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	again, err := OpenAt(s.Dir(), root, "s1", true, 4096)
	if err != nil {
		t.Fatal(err)
	}
	l := again.Ledger()
	if l.Task != "add a flag" || len(l.Steps) != 3 || l.Steps[1].Status != "doing" || l.Steps[0].Status != "todo" {
		t.Fatalf("ledger: %+v", l)
	}
	if len(l.Decisions) != 1 || l.Decisions[0] != "use cobra" || len(l.Facts) != 1 || l.Baseline.Head != "abc" {
		t.Fatalf("ledger: %+v", l)
	}
	if !strings.Contains(again.Notes(), "config lives in internal/config") {
		t.Fatalf("notes: %q", again.Notes())
	}
}

func TestSessionScopeClearsOnNewSessionButKeepsNotes(t *testing.T) {
	s, root := openTest(t, "s1", false)
	s.SetPlan("t", []string{"a"})
	s.AddNoteLine("durable fact")
	s.Flush()
	fresh, err := OpenAt(s.Dir(), root, "s2", false, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Ledger().Task != "" || len(fresh.Digests()) != 0 {
		t.Fatal("session files survived a new session")
	}
	if fresh.Notes() != "durable fact\n" {
		t.Fatalf("notes lost: %q", fresh.Notes())
	}
	// A resume of the same session keeps everything.
	s.SetPlan("t2", nil)
	s.Flush()
	kept, _ := OpenAt(s.Dir(), root, "s1", true, 4096)
	if kept.Ledger().Task != "t2" {
		t.Fatal("resume lost the ledger")
	}
}

func TestNotesCapTrimsOldestLines(t *testing.T) {
	s, _ := openTest(t, "s1", false)
	s.notesCap = 40
	s.AddNoteLine("first line that is long enough")
	s.AddNoteLine("second line that is long enough")
	n := s.Notes()
	if strings.Contains(n, "first") || !strings.Contains(n, "second") || len(n) > 40 {
		t.Fatalf("cap: %q", n)
	}
	if err := s.DropNote(1); err != nil || s.Notes() != "" {
		t.Fatalf("drop: %v %q", err, s.Notes())
	}
	if err := s.DropNote(1); err == nil {
		t.Fatal("drop past end must error")
	}
}

func TestCorruptFileIsRenamedAndStartedFresh(t *testing.T) {
	s, root := openTest(t, "s1", false)
	s.SetPlan("t", nil)
	s.Flush()
	os.WriteFile(filepath.Join(s.Dir(), "ledger.json"), []byte("{not json"), 0o600)
	again, err := OpenAt(s.Dir(), root, "s1", true, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if again.Ledger().Task != "" {
		t.Fatal("corrupt ledger was not reset")
	}
	entries, _ := os.ReadDir(s.Dir())
	found := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "ledger.json.broken-") {
			found = true
		}
	}
	if !found {
		t.Fatal("corrupt file not preserved")
	}
}

func TestDigestEvictionKeepsMostRecentlyTouched(t *testing.T) {
	s, _ := openTest(t, "s1", false)
	for i := 0; i < maxDigests+5; i++ {
		s.putDigest(&Digest{Path: fmt.Sprintf("f%d.go", i), Turn: i})
	}
	if len(s.Digests()) != maxDigests {
		t.Fatalf("digests = %d", len(s.Digests()))
	}
	if s.Digests()[0].Turn != maxDigests+4 {
		t.Fatalf("newest first expected, got turn %d", s.Digests()[0].Turn)
	}
}
```

- [ ] **Step 4: Run to verify they fail**

Run: `go test ./internal/engine/ 2>&1 | head -5`
Expected: build failure (`undefined: OpenAt`).

- [ ] **Step 5: Implement the store**

Create `internal/engine/store.go`:

```go
// Package engine is BE-Code's working memory: what the model has read,
// looked up and decided during a task, kept by the harness and put back in
// front of the model after compaction and on resume. It never edits the
// user's project and never imports the agent.
package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
)

const (
	maxDigests      = 200
	maxDigestBytes  = 256 * 1024
	maxLookups      = 20
	defaultNotesCap = 4096
)

type Range struct {
	From int `json:"from"`
	To   int `json:"to"`
}

type Digest struct {
	Path    string   `json:"path"`
	Hash    string   `json:"hash"`
	Size    int64    `json:"size"`
	ModTime int64    `json:"mtime"`
	Outline []string `json:"outline,omitempty"`
	Ranges  []Range  `json:"ranges,omitempty"`
	Note    string   `json:"note,omitempty"`
	Turn    int      `json:"turn"`
	Edited  bool     `json:"edited,omitempty"`
}

type Step struct {
	Text   string `json:"text"`
	Status string `json:"status"` // todo | doing | done | skip
}

type Baseline struct {
	Head  string `json:"head,omitempty"`
	Dirty string `json:"dirty,omitempty"` // hash of git status --porcelain
}

type Ledger struct {
	Task      string   `json:"task"`
	Steps     []Step   `json:"steps"`
	Decisions []string `json:"decisions"`
	Facts     []string `json:"facts"`
	Baseline  Baseline `json:"baseline"`
	Session   string   `json:"session"`
}

type Hit struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

type Lookup struct {
	Tool   string            `json:"tool"`
	Query  string            `json:"query"`
	Hits   []Hit             `json:"hits"`
	Hashes map[string]string `json:"hashes"`
	Turn   int               `json:"turn"`
}

// Store is one workspace's working memory. One mutex guards everything;
// readers take snapshots (Ledger, Digests, Lookups, Notes return copies).
type Store struct {
	mu       sync.Mutex
	dir      string
	root     string
	digests  map[string]*Digest
	ledger   Ledger
	lookups  []Lookup // newest last
	notes    string
	notesCap int
	turn     int
	dirty    bool
}

// Key derives the store directory name for a workspace root.
func Key(root string) string {
	abs, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		abs = filepath.Clean(root)
	}
	sum := sha256.Sum256([]byte(abs))
	return hex.EncodeToString(sum[:])[:16]
}

// Open opens (or creates) the store for root under ~/.be-code/engine.
func Open(root, sessionID string, resumed bool, notesCap int) (*Store, error) {
	base, err := config.Dir()
	if err != nil {
		return nil, err
	}
	return OpenAt(filepath.Join(base, "engine", Key(root)), root, sessionID, resumed, notesCap)
}

// OpenAt is Open with an explicit directory (tests). A session id that
// differs from the stored one on a non-resume start clears the session
// files; notes.md always survives.
func OpenAt(dir, root, sessionID string, resumed bool, notesCap int) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if notesCap <= 0 {
		notesCap = defaultNotesCap
	}
	s := &Store{dir: dir, root: root, digests: map[string]*Digest{}, notesCap: notesCap}
	var digests []Digest
	loadJSON(filepath.Join(dir, "digests.json"), &digests)
	loadJSON(filepath.Join(dir, "ledger.json"), &s.ledger)
	loadJSON(filepath.Join(dir, "lookups.json"), &s.lookups)
	if b, err := os.ReadFile(filepath.Join(dir, "notes.md")); err == nil {
		s.notes = string(b)
	}
	if !resumed && s.ledger.Session != sessionID {
		digests, s.lookups = nil, nil
		s.ledger = Ledger{}
		s.dirty = true
	}
	s.ledger.Session = sessionID
	for i := range digests {
		d := digests[i]
		s.digests[d.Path] = &d
		if d.Turn > s.turn {
			s.turn = d.Turn
		}
	}
	return s, nil
}

// loadJSON decodes path into v; a corrupt file is moved aside with a
// .broken-<stamp> suffix and v is left zero. A missing file is fine.
func loadJSON(path string, v any) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if err := json.Unmarshal(b, v); err != nil {
		os.Rename(path, path+".broken-"+time.Now().Format("20060102-150405"))
	}
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Dir is the store directory.
func (s *Store) Dir() string { return s.dir }

// Flush writes the session files and notes when anything changed.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	digests := s.digestsLocked()
	for {
		b, err := json.Marshal(digests)
		if err != nil {
			return err
		}
		if len(b) <= maxDigestBytes || len(digests) == 0 {
			if err := writeAtomic(filepath.Join(s.dir, "digests.json"), b); err != nil {
				return err
			}
			break
		}
		// Over the byte cap: drop the least recently touched (last).
		delete(s.digests, digests[len(digests)-1].Path)
		digests = digests[:len(digests)-1]
	}
	lb, _ := json.Marshal(s.ledger)
	if err := writeAtomic(filepath.Join(s.dir, "ledger.json"), lb); err != nil {
		return err
	}
	kb, _ := json.Marshal(s.lookups)
	if err := writeAtomic(filepath.Join(s.dir, "lookups.json"), kb); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(s.dir, "notes.md"), []byte(s.notes)); err != nil {
		return err
	}
	s.dirty = false
	return nil
}

// NextTurn advances the turn counter (one per model call) and returns it.
func (s *Store) NextTurn() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turn++
	return s.turn
}

// Turn is the current turn number.
func (s *Store) Turn() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turn
}

// digestsLocked returns the digests most recently touched first.
func (s *Store) digestsLocked() []Digest {
	out := make([]Digest, 0, len(s.digests))
	for _, d := range s.digests {
		out = append(out, *d)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Turn != out[j].Turn {
			return out[i].Turn > out[j].Turn
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// Digests is a snapshot, most recently touched first.
func (s *Store) Digests() []Digest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.digestsLocked()
}

// putDigest stores d, evicting the least recently touched past the cap.
func (s *Store) putDigest(d *Digest) {
	s.digests[d.Path] = d
	s.dirty = true
	if len(s.digests) <= maxDigests {
		return
	}
	all := s.digestsLocked()
	for _, old := range all[maxDigests:] {
		delete(s.digests, old.Path)
	}
}

// Lookups is a snapshot, newest first.
func (s *Store) Lookups() []Lookup {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Lookup, 0, len(s.lookups))
	for i := len(s.lookups) - 1; i >= 0; i-- {
		out = append(out, s.lookups[i])
	}
	return out
}

// Ledger is a snapshot of the task ledger.
func (s *Store) Ledger() Ledger {
	s.mu.Lock()
	defer s.mu.Unlock()
	l := s.ledger
	l.Steps = append([]Step(nil), l.Steps...)
	l.Decisions = append([]string(nil), l.Decisions...)
	l.Facts = append([]string(nil), l.Facts...)
	return l
}

// SetPlan replaces the task line and steps (all todo).
func (s *Store) SetPlan(task string, steps []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ledger.Task = strings.TrimSpace(task)
	s.ledger.Steps = nil
	for _, st := range steps {
		if st = strings.TrimSpace(st); st != "" {
			s.ledger.Steps = append(s.ledger.Steps, Step{Text: st, Status: "todo"})
		}
	}
	s.dirty = true
}

// EnsureTask sets the task line only when the ledger has none.
func (s *Store) EnsureTask(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ledger.Task != "" {
		return
	}
	line := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	if len(line) > 200 {
		line = line[:200]
	}
	s.ledger.Task = line
	s.dirty = true
}

// SetStep marks 1-based step i with a status (already normalised by the tool).
func (s *Store) SetStep(i int, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 1 || i > len(s.ledger.Steps) {
		return fmt.Errorf("step %d does not exist (%d steps)", i, len(s.ledger.Steps))
	}
	s.ledger.Steps[i-1].Status = status
	s.dirty = true
	return nil
}

// AddNote records a fact or decision; with file it also becomes that
// digest's note (a digest is created when none exists); with keep it is
// appended to the durable notes as well.
func (s *Store) AddNote(text, file string, decision, keep bool) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("note needs text")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if decision {
		s.ledger.Decisions = append(s.ledger.Decisions, text)
	} else {
		s.ledger.Facts = append(s.ledger.Facts, text)
	}
	if file = strings.TrimSpace(file); file != "" {
		d, ok := s.digests[filepath.ToSlash(filepath.Clean(file))]
		if !ok {
			d = &Digest{Path: filepath.ToSlash(filepath.Clean(file))}
			s.digests[d.Path] = d
		}
		d.Note = text
		d.Turn = s.turn
	}
	if keep {
		s.addNoteLineLocked(text)
	}
	s.dirty = true
	return nil
}

// SetBaseline records where the task began.
func (s *Store) SetBaseline(b Baseline) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ledger.Baseline = b
	s.dirty = true
}

// Notes is the durable notes text.
func (s *Store) Notes() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notes
}

// AddNoteLine appends one durable note, trimming the oldest lines past the cap.
func (s *Store) AddNoteLine(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.addNoteLineLocked(text)
	s.dirty = true
}

func (s *Store) addNoteLineLocked(text string) {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	if text == "" || strings.Contains(s.notes, text+"\n") {
		return
	}
	s.notes += text + "\n"
	for len(s.notes) > s.notesCap {
		nl := strings.IndexByte(s.notes, '\n')
		if nl < 0 {
			s.notes = ""
			break
		}
		s.notes = s.notes[nl+1:]
	}
}

// DropNote removes 1-based line n.
func (s *Store) DropNote(n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	lines := strings.Split(strings.TrimRight(s.notes, "\n"), "\n")
	if s.notes == "" || n < 1 || n > len(lines) {
		return fmt.Errorf("note %d does not exist", n)
	}
	lines = append(lines[:n-1], lines[n:]...)
	s.notes = ""
	if len(lines) > 0 {
		s.notes = strings.Join(lines, "\n") + "\n"
	}
	s.dirty = true
	return nil
}

// ClearNotes empties the durable notes.
func (s *Store) ClearNotes() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes = ""
	s.dirty = true
}

// ClearSession drops digests, lookups and the ledger (never the notes).
func (s *Store) ClearSession() {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.ledger.Session
	s.digests = map[string]*Digest{}
	s.lookups = nil
	s.ledger = Ledger{Session: session}
	s.dirty = true
}
```

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/engine/ ./internal/config/ ./internal/repomap/`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/engine internal/repomap internal/config
git commit -m "engine: the working-memory store — digests, ledger, lookups, durable notes"
```

---

### Task 2: Observe, cache and render

**Files:**
- Create: `internal/engine/observe.go`, `internal/engine/render.go`
- Test: `internal/engine/observe_test.go`, `internal/engine/render_test.go`

**Interfaces:**
- Consumes: Task 1's store; `repomap.Outline`.
- Produces:
  - `engine.Event{Tool string; Args map[string]any; Content string; IsError bool}`
  - `(*Store) Observe(ev Event) (footer string)` — records; returns `already read at turn N (unchanged); outline and notes are in your context` for a redundant read, else "".
  - `(*Store) Cached(tool string, args map[string]any) (content string, ok bool)` — a repeated lookup whose files are unchanged; the returned content already ends with `\n(cached; files unchanged)`.
  - `engine.LookupKey(tool string, args map[string]any) string` (canonical `tool + " " + sorted-key JSON`).
  - `(*Store) HasDigest(path string) (Range, bool)` — the union range for Compact's stub.
  - `(*Store) Render(budget int, inMap func(path string) bool) string` — the block body (no header when empty).
  - `(*Store) LedgerText() string` (for `/task`), `(*Store) StoppedAt() string` (text of the `doing` step or ""), `(*Store) ApplyFileNotes(block string)`, `engine.ParsePlanSteps(plan string) []string`, `engine.SplitFilesBlock(summary string) (body, filesBlock string)`.

- [ ] **Step 1: Write the failing tests**

`internal/engine/observe_test.go`:

```go
package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func numbered(content string, from int) string {
	var b strings.Builder
	for i, l := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		fmt.Fprintf(&b, "%5d\t%s\n", from+i, l)
	}
	return b.String()
}

func TestObserveReadDigestsAndFlagsRedundantReads(t *testing.T) {
	s, root := openTest(t, "s1", false)
	src := "package x\n\nfunc A() {}\n\nfunc B() {}\n"
	writeFile(t, root, "x.go", src)
	s.NextTurn()
	ev := Event{Tool: "read_file", Args: map[string]any{"path": "x.go"}, Content: numbered(src, 1)}
	if f := s.Observe(ev); f != "" {
		t.Fatalf("first read footer %q", f)
	}
	d := s.Digests()[0]
	if d.Path != "x.go" || len(d.Outline) != 2 || d.Ranges[0] != (Range{1, 5}) || d.Turn != 1 {
		t.Fatalf("digest %+v", d)
	}
	s.NextTurn()
	if f := s.Observe(ev); f != "already read at turn 1 (unchanged); outline and notes are in your context" {
		t.Fatalf("second read footer %q", f)
	}
	// A range not yet seen is not redundant.
	writeFile(t, root, "big.go", strings.Repeat("x\n", 50))
	s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "big.go", "offset": 1, "limit": 10}, Content: numbered(strings.Repeat("x\n", 10), 1)})
	if f := s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "big.go", "offset": 11, "limit": 10}, Content: numbered(strings.Repeat("x\n", 10), 11)}); f != "" {
		t.Fatalf("unseen range flagged: %q", f)
	}
	if f := s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "big.go", "offset": 5, "limit": 10}, Content: numbered(strings.Repeat("x\n", 10), 5)}); f == "" {
		t.Fatal("covered range not flagged")
	}
	// A changed file is never redundant and the digest is refreshed.
	writeFile(t, root, "x.go", src+"\nfunc C() {}\n")
	if f := s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "x.go"}, Content: numbered(src+"\nfunc C() {}\n", 1)}); f != "" {
		t.Fatalf("changed file flagged: %q", f)
	}
	if len(s.Digests()[0].Outline) != 3 {
		t.Fatal("outline not refreshed")
	}
	// Errors are ignored.
	if f := s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "nope.go"}, Content: "open: no such file", IsError: true}); f != "" {
		t.Fatal("error observed")
	}
}

func TestObserveWriteMarksEditedAndResetsRanges(t *testing.T) {
	s, root := openTest(t, "s1", false)
	writeFile(t, root, "y.go", "package y\n")
	s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "y.go"}, Content: numbered("package y\n", 1)})
	writeFile(t, root, "y.go", "package y\n\nfunc New() {}\n")
	s.Observe(Event{Tool: "write_file", Args: map[string]any{"path": "y.go", "content": "package y\n\nfunc New() {}\n"}, Content: "wrote y.go"})
	d := s.Digests()[0]
	if !d.Edited || d.Outline[0] != "New" || d.Ranges[0] != (Range{1, 3}) {
		t.Fatalf("digest after write %+v", d)
	}
}

func TestLookupCacheServesUnchangedRepeats(t *testing.T) {
	s, root := openTest(t, "s1", false)
	writeFile(t, root, "a.go", "package a\nfunc Hit() {}\n")
	args := map[string]any{"pattern": "Hit", "glob": "*.go"}
	if _, ok := s.Cached("search", args); ok {
		t.Fatal("cache hit before any lookup")
	}
	s.Observe(Event{Tool: "search", Args: args, Content: "a.go:2:func Hit() {}"})
	got, ok := s.Cached("search", map[string]any{"glob": "*.go", "pattern": "Hit"}) // key order irrelevant
	if !ok || !strings.HasSuffix(got, "\n(cached; files unchanged)") || !strings.HasPrefix(got, "a.go:2:") {
		t.Fatalf("cached: %v %q", ok, got)
	}
	writeFile(t, root, "a.go", "package a\nfunc Hit() {}\nfunc Hit2() {}\n")
	if _, ok := s.Cached("search", args); ok {
		t.Fatal("cache served after the file changed")
	}
	if len(s.Lookups()) != 1 || s.Lookups()[0].Hits[0].Line != 2 {
		t.Fatalf("lookups %+v", s.Lookups())
	}
}
```

`internal/engine/render_test.go`:

```go
package engine

import (
	"strings"
	"testing"
)

func TestRenderIsEmptyForAFreshStore(t *testing.T) {
	s, _ := openTest(t, "s1", false)
	if out := s.Render(6144, func(string) bool { return false }); out != "" {
		t.Fatalf("fresh render %q", out)
	}
}

func TestRenderPriorityMarkersAndBudget(t *testing.T) {
	s, root := openTest(t, "s1", false)
	s.AddNoteLine("tests need go on PATH")
	s.SetPlan("add flag", []string{"parse", "wire"})
	s.SetStep(1, "done")
	s.SetStep(2, "doing")
	s.AddNote("cobra owns flags", "", true, false)
	writeFile(t, root, "cmd/root.go", "package cmd\nfunc Execute() {}\n")
	s.NextTurn()
	s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "cmd/root.go"}, Content: numbered("package cmd\nfunc Execute() {}\n", 1)})
	s.AddNote("flags are parsed here", "cmd/root.go", false, false)
	writeFile(t, root, "cmd/root.go", "package cmd\nfunc Execute() {}\n// changed\n")
	s.Observe(Event{Tool: "search", Args: map[string]any{"pattern": "Execute"}, Content: "cmd/root.go:2:func Execute() {}"})
	out := s.Render(6144, func(p string) bool { return p == "cmd/root.go" })
	want := []string{
		"tests need go on PATH",
		"Task: add flag",
		"doing: 2. wire",
		"done: 1. parse",
		"decisions:\n- cobra owns flags",
		"facts:\n- flags are parsed here",
		"cmd/root.go (lines 1–2) — flags are parsed here [changed since read]",
		`search "Execute": cmd/root.go:2`,
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Fatalf("render lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "outline:") {
		t.Fatal("outline repeated although the repo map has the file")
	}
	// Outline appears when the map does not carry the file.
	out2 := s.Render(6144, func(string) bool { return false })
	if !strings.Contains(out2, "outline: Execute") {
		t.Fatalf("outline missing:\n%s", out2)
	}
	// Budget trims at a line boundary, notes and task first to survive.
	small := s.Render(120, func(string) bool { return false })
	if !strings.HasPrefix(small, "tests need go on PATH") || len(small) > 120 || strings.Contains(small, "search") {
		t.Fatalf("budgeted render:\n%s", small)
	}
	if idx := strings.Index(out, "Task:"); idx < strings.Index(out, "tests need") {
		t.Fatal("notes must come before the task")
	}
}

func TestLedgerTextStoppedAtAndFileNotes(t *testing.T) {
	s, root := openTest(t, "s1", false)
	if s.StoppedAt() != "" {
		t.Fatal("stopped-at on an empty ledger")
	}
	s.SetPlan("t", []string{"one", "two"})
	s.SetStep(2, "doing")
	if s.StoppedAt() != "two" {
		t.Fatalf("stopped at %q", s.StoppedAt())
	}
	if lt := s.LedgerText(); !strings.Contains(lt, "[ ] 1. one") || !strings.Contains(lt, "[>] 2. two") {
		t.Fatalf("ledger text:\n%s", lt)
	}
	writeFile(t, root, "a.go", "package a\n")
	s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "a.go"}, Content: numbered("package a\n", 1)})
	s.ApplyFileNotes("- a.go — entry point\n- unknown.go — ignored\n")
	if s.Digests()[0].Note != "entry point" || len(s.Digests()) != 1 {
		t.Fatalf("file notes %+v", s.Digests())
	}
}

func TestSplitFilesBlockAndParsePlanSteps(t *testing.T) {
	body, files := SplitFilesBlock("Summary text.\nMore.\n\nfiles:\n- a.go — main\n- b.go — helper\n")
	if body != "Summary text.\nMore." || !strings.Contains(files, "a.go — main") {
		t.Fatalf("split: %q | %q", body, files)
	}
	if b, f := SplitFilesBlock("no block"); b != "no block" || f != "" {
		t.Fatalf("no block: %q %q", b, f)
	}
	steps := ParsePlanSteps("Plan:\n1. parse flags\n2) wire cobra\n- write tests\n* docs\nnot a step\n")
	if len(steps) != 4 || steps[0] != "parse flags" || steps[3] != "docs" {
		t.Fatalf("steps %v", steps)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/engine/ 2>&1 | head -5`
Expected: build failure (`undefined: Event`).

- [ ] **Step 3: Implement observe.go**

```go
package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/brown-enterprises/be-code/internal/repomap"
)

// Event is one tool call and its result, as dispatch saw them.
type Event struct {
	Tool    string
	Args    map[string]any
	Content string
	IsError bool
}

const (
	footerAlreadyRead = "already read at turn %d (unchanged); outline and notes are in your context"
	footerCached      = "(cached; files unchanged)"
)

var numberedLine = regexp.MustCompile(`(?m)^\s*(\d+)\t`)
var hitLine = regexp.MustCompile(`^([^\s:][^:]*):(\d+)[:-](.*)$`)

func argStr(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func relPath(p string) string { return filepath.ToSlash(filepath.Clean(p)) }

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// fileState reads a workspace file and returns its hash, size and mtime;
// ok is false when it cannot be read.
func (s *Store) fileState(rel string) (data []byte, hash string, size, mtime int64, ok bool) {
	abs := filepath.Join(s.root, filepath.FromSlash(rel))
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		return nil, "", 0, 0, false
	}
	data, err = os.ReadFile(abs)
	if err != nil {
		return nil, "", 0, 0, false
	}
	return data, hashBytes(data), info.Size(), info.ModTime().UnixNano(), true
}

// Observe records what a tool result revealed and returns a footer for the
// model when the read was redundant. Never errors; never blocks on I/O
// beyond one stat and one read of a file the model just read.
func (s *Store) Observe(ev Event) string {
	if ev.IsError {
		return ""
	}
	switch ev.Tool {
	case "read_file":
		return s.observeRead(ev)
	case "write_file", "edit_file":
		s.observeWrite(ev)
	case "search", "lookup", "history":
		s.observeLookup(ev)
	}
	return ""
}

// seenRange works out which lines a read_file result covered.
func seenRange(ev Event, total int) Range {
	nums := numberedLine.FindAllStringSubmatch(ev.Content, -1)
	if len(nums) == 0 {
		return Range{1, total}
	}
	from, _ := strconv.Atoi(nums[0][1])
	to, _ := strconv.Atoi(nums[len(nums)-1][1])
	if from < 1 {
		from = 1
	}
	if to < from {
		to = from
	}
	return Range{from, to}
}

func covered(ranges []Range, r Range) bool {
	// Ranges are merged on insert, so one of them must contain r entirely.
	for _, have := range ranges {
		if have.From <= r.From && have.To >= r.To {
			return true
		}
	}
	return false
}

func mergeRange(ranges []Range, r Range) []Range {
	ranges = append(ranges, r)
	sort.Slice(ranges, func(i, j int) bool { return ranges[i].From < ranges[j].From })
	out := ranges[:1]
	for _, n := range ranges[1:] {
		last := &out[len(out)-1]
		if n.From <= last.To+1 {
			if n.To > last.To {
				last.To = n.To
			}
			continue
		}
		out = append(out, n)
	}
	return out
}

func (s *Store) observeRead(ev Event) string {
	rel := relPath(argStr(ev.Args, "path", "file", "filename"))
	if rel == "" || rel == "." {
		return ""
	}
	data, hash, size, mtime, ok := s.fileState(rel)
	if !ok {
		return ""
	}
	total := strings.Count(string(data), "\n")
	if len(data) > 0 && data[len(data)-1] != '\n' {
		total++
	}
	r := seenRange(ev, total)
	s.mu.Lock()
	defer s.mu.Unlock()
	d, exists := s.digests[rel]
	if exists && d.Hash == hash && covered(d.Ranges, r) {
		turn := d.Turn
		d.Turn = s.turn
		s.dirty = true
		return fmt.Sprintf(footerAlreadyRead, turn)
	}
	if !exists || d.Hash != hash {
		nd := &Digest{Path: rel}
		if exists {
			nd.Note = d.Note
		}
		nd.Hash, nd.Size, nd.ModTime = hash, size, mtime
		nd.Outline = repomap.Outline(rel, data)
		nd.Ranges = []Range{r}
		nd.Turn = s.turn
		s.putDigest(nd)
		return ""
	}
	d.Ranges = mergeRange(d.Ranges, r)
	d.Turn = s.turn
	s.dirty = true
	return ""
}

func (s *Store) observeWrite(ev Event) {
	rel := relPath(argStr(ev.Args, "path", "file", "filename"))
	if rel == "" || rel == "." {
		return
	}
	data, hash, size, mtime, ok := s.fileState(rel)
	if !ok {
		return
	}
	total := strings.Count(string(data), "\n")
	if len(data) > 0 && data[len(data)-1] != '\n' {
		total++
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d, exists := s.digests[rel]
	if !exists {
		d = &Digest{Path: rel}
	}
	d.Hash, d.Size, d.ModTime = hash, size, mtime
	d.Outline = repomap.Outline(rel, data)
	d.Ranges = []Range{{1, total}}
	d.Edited = true
	d.Turn = s.turn
	s.putDigest(d)
}

// LookupKey canonicalises a lookup so argument order never misses the cache.
func LookupKey(tool string, args map[string]any) string {
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(tool)
	b.WriteByte(' ')
	for _, k := range keys {
		v, _ := json.Marshal(args[k])
		b.WriteString(k)
		b.WriteByte('=')
		b.Write(v)
		b.WriteByte(';')
	}
	return b.String()
}

// parseHits reads file:line:text (or file-line-text, git grep's context
// form) lines out of a search or lookup result.
func parseHits(content string) []Hit {
	var hits []Hit
	for _, line := range strings.Split(content, "\n") {
		m := hitLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		n, _ := strconv.Atoi(m[2])
		text := strings.TrimSpace(m[3])
		if len(text) > 120 {
			text = text[:120]
		}
		hits = append(hits, Hit{File: relPath(m[1]), Line: n, Text: text})
		if len(hits) >= 50 {
			break
		}
	}
	return hits
}

func (s *Store) observeLookup(ev Event) {
	key := LookupKey(ev.Tool, ev.Args)
	hits := parseHits(ev.Content)
	hashes := map[string]string{}
	for _, h := range hits {
		if _, ok := hashes[h.File]; ok {
			continue
		}
		if _, hash, _, _, ok := s.fileState(h.File); ok {
			hashes[h.File] = hash
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.lookups {
		if s.lookups[i].Query == key {
			s.lookups = append(s.lookups[:i], s.lookups[i+1:]...)
			break
		}
	}
	s.lookups = append(s.lookups, Lookup{Tool: ev.Tool, Query: key, Hits: hits, Hashes: hashes, Turn: s.turn})
	if len(s.lookups) > maxLookups {
		s.lookups = s.lookups[len(s.lookups)-maxLookups:]
	}
	s.cache(key, ev.Content)
	s.dirty = true
}

// The cached contents live in memory only (a session's worth of lookups is
// small); the hits and hashes are what persist.
func (s *Store) cache(key, content string) {
	if s.cached == nil {
		s.cached = map[string]string{}
	}
	s.cached[key] = content
}

// Cached answers a repeated lookup whose hit files are unchanged.
func (s *Store) Cached(tool string, args map[string]any) (string, bool) {
	key := LookupKey(tool, args)
	s.mu.Lock()
	var lk *Lookup
	for i := range s.lookups {
		if s.lookups[i].Query == key {
			lk = &s.lookups[i]
		}
	}
	content, have := s.cached[key]
	s.mu.Unlock()
	if lk == nil || !have {
		return "", false
	}
	for file, hash := range lk.Hashes {
		if _, now, _, _, ok := s.fileState(file); !ok || now != hash {
			return "", false
		}
	}
	return strings.TrimRight(content, "\n") + "\n" + footerCached, true
}

// HasDigest reports the union of ranges seen for a path.
func (s *Store) HasDigest(path string) (Range, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.digests[relPath(path)]
	if !ok || len(d.Ranges) == 0 {
		return Range{}, false
	}
	return Range{d.Ranges[0].From, d.Ranges[len(d.Ranges)-1].To}, true
}
```

Add the field `cached map[string]string` to `Store` in `store.go`.

- [ ] **Step 4: Implement render.go**

```go
package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Render builds the Working memory block body: notes, task, files read,
// recent lookups — in that order, so the budget drops the least valuable
// part first. inMap reports whether the repository map already lists a
// file's symbols (then the outline is not repeated). Empty when nothing
// has been learned yet.
func (s *Store) Render(budget int, inMap func(path string) bool) string {
	s.mu.Lock()
	notes := s.notes
	l := s.ledger
	digests := s.digestsLocked()
	lookups := make([]Lookup, len(s.lookups))
	copy(lookups, s.lookups)
	s.mu.Unlock()

	var parts []string
	if strings.TrimSpace(notes) != "" {
		parts = append(parts, strings.TrimRight(notes, "\n"))
	}
	if l.Task != "" || len(l.Steps) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "Task: %s\n", l.Task)
		var done []string
		for i, st := range l.Steps {
			switch st.Status {
			case "doing":
				fmt.Fprintf(&b, "doing: %d. %s\n", i+1, st.Text)
			case "done", "skip":
				done = append(done, fmt.Sprintf("%d. %s", i+1, st.Text))
			}
		}
		if len(done) > 0 {
			fmt.Fprintf(&b, "done: %s\n", strings.Join(done, "; "))
		}
		if todo := countStatus(l.Steps, "todo"); todo > 0 {
			fmt.Fprintf(&b, "todo: %d step(s)\n", todo)
		}
		if len(l.Decisions) > 0 {
			b.WriteString("decisions:\n- " + strings.Join(l.Decisions, "\n- ") + "\n")
		}
		if len(l.Facts) > 0 {
			b.WriteString("facts:\n- " + strings.Join(l.Facts, "\n- ") + "\n")
		}
		parts = append(parts, strings.TrimRight(b.String(), "\n"))
	}
	if len(digests) > 0 {
		var b strings.Builder
		b.WriteString("Files read:\n")
		for _, d := range digests {
			b.WriteString(s.digestRow(d, inMap))
			b.WriteByte('\n')
		}
		parts = append(parts, strings.TrimRight(b.String(), "\n"))
	}
	if len(lookups) > 0 {
		var b strings.Builder
		b.WriteString("Recent lookups:\n")
		for i := len(lookups) - 1; i >= 0; i-- {
			b.WriteString(lookupRow(lookups[i]))
			b.WriteByte('\n')
		}
		parts = append(parts, strings.TrimRight(b.String(), "\n"))
	}
	if len(parts) == 0 {
		return ""
	}
	return trimLines(strings.Join(parts, "\n\n"), budget)
}

func countStatus(steps []Step, status string) int {
	n := 0
	for _, st := range steps {
		if st.Status == status {
			n++
		}
	}
	return n
}

func (s *Store) digestRow(d Digest, inMap func(string) bool) string {
	var b strings.Builder
	b.WriteString(d.Path)
	if len(d.Ranges) > 0 {
		var rs []string
		for _, r := range d.Ranges {
			rs = append(rs, fmt.Sprintf("%d–%d", r.From, r.To))
		}
		fmt.Fprintf(&b, " (lines %s)", strings.Join(rs, ", "))
	}
	if d.Note != "" {
		b.WriteString(" — " + d.Note)
	}
	if d.Hash != "" && s.stale(d) {
		b.WriteString(" [changed since read]")
	}
	if d.Edited {
		b.WriteString(" [you edited this]")
	}
	if len(d.Outline) > 0 && (inMap == nil || !inMap(d.Path)) {
		b.WriteString("\n  outline: " + strings.Join(d.Outline, ", "))
	}
	return b.String()
}

// stale checks size and mtime first and hashes only when they moved.
func (s *Store) stale(d Digest) bool {
	info, err := os.Stat(filepath.Join(s.root, filepath.FromSlash(d.Path)))
	if err != nil || info.IsDir() {
		return true
	}
	if info.Size() == d.Size && info.ModTime().UnixNano() == d.ModTime {
		return false
	}
	_, hash, _, _, ok := s.fileState(d.Path)
	return !ok || hash != d.Hash
}

func lookupRow(l Lookup) string {
	q := l.Query
	if i := strings.Index(q, " "); i >= 0 {
		q = q[i+1:]
	}
	q = summariseKey(q)
	var refs []string
	seen := map[string]bool{}
	for _, h := range l.Hits {
		ref := fmt.Sprintf("%s:%d", h.File, h.Line)
		if seen[ref] {
			continue
		}
		seen[ref] = true
		refs = append(refs, ref)
		if len(refs) >= 8 {
			break
		}
	}
	if len(refs) == 0 {
		refs = []string{"no hits"}
	}
	return fmt.Sprintf("%s %s: %s", l.Tool, q, strings.Join(refs, ", "))
}

// summariseKey turns the canonical key back into a readable "query" for
// the block: the first quoted string value it finds.
func summariseKey(k string) string {
	for _, part := range strings.Split(k, ";") {
		if i := strings.Index(part, "=\""); i >= 0 && strings.HasSuffix(part, "\"") {
			return part[i+1:]
		}
	}
	return `""`
}

// trimLines cuts s to at most budget bytes at a line boundary.
func trimLines(s string, budget int) string {
	if budget <= 0 || len(s) <= budget {
		return s
	}
	cut := s[:budget]
	if nl := strings.LastIndexByte(cut, '\n'); nl > 0 {
		cut = cut[:nl]
	}
	return strings.TrimRight(cut, "\n")
}

// LedgerText is the /task listing.
func (s *Store) LedgerText() string {
	l := s.Ledger()
	if l.Task == "" && len(l.Steps) == 0 {
		return "no task recorded"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "task: %s\n", l.Task)
	for i, st := range l.Steps {
		mark := "[ ]"
		switch st.Status {
		case "doing":
			mark = "[>]"
		case "done":
			mark = "[x]"
		case "skip":
			mark = "[-]"
		}
		fmt.Fprintf(&b, "%s %d. %s\n", mark, i+1, st.Text)
	}
	for _, d := range l.Decisions {
		fmt.Fprintf(&b, "decision: %s\n", d)
	}
	for _, f := range l.Facts {
		fmt.Fprintf(&b, "fact: %s\n", f)
	}
	return strings.TrimRight(b.String(), "\n")
}

// StoppedAt is the text of the step in progress, or "".
func (s *Store) StoppedAt() string {
	for _, st := range s.Ledger().Steps {
		if st.Status == "doing" {
			return st.Text
		}
	}
	return ""
}

var fileNoteLine = regexp.MustCompile(`(?m)^\s*(?:-\s*)?([^\s—]+)\s+—\s+(.+)$`)

// ApplyFileNotes stores "path — note" lines from a compaction summary's
// files: block for paths that already have a digest.
func (s *Store) ApplyFileNotes(block string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range fileNoteLine.FindAllStringSubmatch(block, -1) {
		if d, ok := s.digests[relPath(m[1])]; ok {
			d.Note = strings.TrimSpace(m[2])
			s.dirty = true
		}
	}
}

// SplitFilesBlock separates a summary's trailing "files:" block from its body.
func SplitFilesBlock(summary string) (body, files string) {
	i := strings.LastIndex(summary, "\nfiles:")
	if i < 0 {
		if strings.HasPrefix(summary, "files:") {
			i = 0
		} else {
			return strings.TrimSpace(summary), ""
		}
	}
	return strings.TrimSpace(summary[:i]), strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(summary[i:]), "files:"))
}

var planStep = regexp.MustCompile(`(?m)^\s*(?:\d+[.)]|[-*])\s+(.+?)\s*$`)

// ParsePlanSteps pulls numbered or bulleted lines out of an approved plan.
func ParsePlanSteps(plan string) []string {
	var steps []string
	for _, m := range planStep.FindAllStringSubmatch(plan, -1) {
		steps = append(steps, m[1])
		if len(steps) >= 20 {
			break
		}
	}
	return steps
}
```

- [ ] **Step 5: Run the package**

Run: `go test -race ./internal/engine/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/engine
git commit -m "engine: observe tool results, cache lookups, render the Working memory block"
```

---

### Task 3: Agent wiring

**Files:**
- Modify: `internal/agent/loop.go` (`Engine` field, `SetEngine`, `composeSystem`, `dispatch`, `run`, `RunFull`), `internal/agent/handoff.go` (`Resume`, `WriteHandoff`), `internal/agent/extras.go` (`ExecutePlan`), `internal/gitctx/gitctx.go` (`Head`, `Porcelain`)
- Test: `internal/agent/engine_test.go` (new), `internal/gitctx/gitctx_test.go` (append)

**Interfaces:**
- Consumes: Task 2's `Store` API.
- Produces: `Agent.Engine *engine.Store` (exported field, nil = disabled), `(*Agent) SetEngine(s *engine.Store)`, `gitctx.Head(ctx, root) string`, `gitctx.Porcelain(ctx, root) string`; the system prompt block `Working memory:\n<body>`; `Agent.inRepoMap(path string) bool` (unexported).

- [ ] **Step 1: Write the failing tests**

Append to `internal/gitctx/gitctx_test.go`:

```go
func TestHeadAndPorcelain(t *testing.T) {
	dir := gitRepo(t)
	ctx := context.Background()
	if h := Head(ctx, dir); len(h) != 40 {
		t.Fatalf("head %q", h)
	}
	if p := Porcelain(ctx, dir); p != "" {
		t.Fatalf("clean tree porcelain %q", p)
	}
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x"), 0o644)
	if p := Porcelain(ctx, dir); !strings.Contains(p, "b.txt") {
		t.Fatalf("porcelain %q", p)
	}
	if Head(ctx, t.TempDir()) != "" {
		t.Fatal("head outside a repo must be empty")
	}
}
```

Create `internal/agent/engine_test.go`:

```go
package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/tools"
)

func withEngine(t *testing.T, ag *Agent) *engine.Store {
	t.Helper()
	st, err := engine.OpenAt(filepath.Join(t.TempDir(), "eng"), ag.Tools.Root, "s1", false, 4096)
	if err != nil {
		t.Fatal(err)
	}
	ag.SetEngine(st)
	return st
}

func TestReadsAreDigestedAndTheBlockReachesTheSystemPrompt(t *testing.T) {
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		switch calls {
		case 1:
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "1", Name: "read_file", Arguments: `{"path":"a.go"}`}}}, nil
		case 2:
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "2", Name: "read_file", Arguments: `{"path":"a.go"}`}}}, nil
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}}
	ag, dir := newTestAgent(t, p, nil)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o644)
	st := withEngine(t, ag)
	if _, err := ag.Run(context.Background(), "look at a.go"); err != nil {
		t.Fatal(err)
	}
	if len(st.Digests()) != 1 || st.Digests()[0].Path != "a.go" {
		t.Fatalf("digests %+v", st.Digests())
	}
	// The second read got the footer; the third request's system prompt has the block.
	second := p.reqs[2].Messages
	var toolMsg string
	for _, m := range second {
		if m.Role == provider.RoleTool && m.ToolCallID == "2" {
			toolMsg = m.Content
		}
	}
	if !strings.Contains(toolMsg, "already read at turn 1 (unchanged)") {
		t.Fatalf("no footer:\n%s", toolMsg)
	}
	sys := p.reqs[2].Messages[0].Content
	if !strings.Contains(sys, "Working memory:") || !strings.Contains(sys, "a.go (lines 1–3)") {
		t.Fatalf("system prompt lacks the block:\n%s", sys)
	}
	if !strings.Contains(sys, "Task: look at a.go") {
		t.Fatalf("RunFull/Run did not seed the task line:\n%s", sys)
	}
	// Flushed at turn end.
	if _, err := os.Stat(filepath.Join(st.Dir(), "digests.json")); err != nil {
		t.Fatal("store not flushed at turn end")
	}
}

func TestRepeatedSearchIsServedFromTheCache(t *testing.T) {
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls <= 2 {
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "s", Name: "search", Arguments: `{"pattern":"func A"}`}}}, nil
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}}
	ag, dir := newTestAgent(t, p, nil)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o644)
	withEngine(t, ag)
	var seen []string
	ag.Events.OnToolEnd = func(name string, res tools.Result) { seen = append(seen, res.Content) }
	ag.Run(context.Background(), "find A")
	if len(seen) != 2 || strings.Contains(seen[0], "(cached") || !strings.HasSuffix(seen[1], "(cached; files unchanged)") {
		t.Fatalf("tool results: %q", seen)
	}
}

func TestRunFullRecordsBaselineAndExecutePlanSeedsSteps(t *testing.T) {
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok"}, nil
	}}
	ag, _ := newTestAgent(t, p, nil)
	st := withEngine(t, ag)
	if _, _, err := ag.ExecutePlan(context.Background(), "add a flag", "Plan\n1. parse\n2. wire\n"); err != nil {
		t.Fatal(err)
	}
	l := st.Ledger()
	if l.Task != "add a flag" || len(l.Steps) != 2 || l.Steps[1].Text != "wire" {
		t.Fatalf("ledger %+v", l)
	}
	// Not a git repo: baseline stays empty rather than erroring.
	if l.Baseline.Head != "" {
		t.Fatalf("baseline %+v", l.Baseline)
	}
}

func TestResumeRebuildsTheBlockAndHandoffCarriesStoppedAt(t *testing.T) {
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok"}, nil
	}}
	ag, _ := newTestAgent(t, p, nil)
	st := withEngine(t, ag)
	st.SetPlan("t", []string{"one", "two"})
	st.SetStep(2, "doing")
	ag.SetSession(store.NewSession("p", "m", ag.Tools.Root))
	ag.Run(context.Background(), "x")
	h, err := ag.WriteHandoff(context.Background(), false)
	if err != nil || !strings.Contains(h, "Stopped at: two") {
		t.Fatalf("handoff %v:\n%s", err, h)
	}
	ag.Resume(ag.Session)
	if !strings.Contains(ag.History.System.Content, "doing: 2. two") {
		t.Fatal("resume did not rebuild the block")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/agent/ -run 'TestReadsAreDigested|TestRepeatedSearch|TestRunFullRecords|TestResumeRebuilds' 2>&1 | head -5`
Expected: build failure (`ag.SetEngine undefined`).

- [ ] **Step 3: gitctx helpers**

Append to `internal/gitctx/gitctx.go`:

```go
// Head is the full HEAD commit hash, or "" outside a repository or before
// the first commit.
func Head(ctx context.Context, root string) string {
	if !IsRepo(ctx, root) {
		return ""
	}
	out, err := git(ctx, root, "rev-parse HEAD")
	if err != nil || len(out) != 40 {
		return ""
	}
	return out
}

// Porcelain is `git status --porcelain`, or "" outside a repository.
func Porcelain(ctx context.Context, root string) string {
	if !IsRepo(ctx, root) {
		return ""
	}
	out, _ := git(ctx, root, "status --porcelain")
	return out
}
```

- [ ] **Step 4: Agent changes**

In `internal/agent/loop.go`:

1. Import `internal/engine` and `crypto/sha256`/`encoding/hex` (for the dirty hash). Add to `Agent` after `Stats`:

```go
	// Engine is the working-memory store (nil when disabled): what the
	// model has read, looked up and decided, kept by the harness and put
	// back in the system prompt after compaction. See internal/engine.
	Engine *engine.Store
```

and

```go
// SetEngine attaches the working-memory store and recomposes the prompt.
func (a *Agent) SetEngine(s *engine.Store) {
	a.Engine = s
	a.History.System.Content = a.composeSystem("")
}

// inRepoMap reports whether the repository map lists a file's symbols.
func (a *Agent) inRepoMap(path string) bool {
	return a.repoMap != "" && (strings.HasPrefix(a.repoMap, path+":") || strings.Contains(a.repoMap, "\n"+path+":"))
}
```

2. In `composeSystem`, after the repo map block and before the handoff:

```go
	if a.Engine != nil && a.systemOverride == "" {
		if wm := a.Engine.Render(a.Cfg.Engine.Budget, a.inRepoMap); wm != "" {
			sys += "\n\nWorking memory:\n" + wm
		}
	}
```

3. In `run`, at the top of the `for turn := 0; …` loop body (before `a.deliverInbox()`): `if a.Engine != nil { a.Engine.NextTurn() }`. Immediately before `req := provider.ChatRequest{…}` add `a.History.System.Content = a.composeSystem(a.lastGitInfo)` — and make `run` keep the per-turn git summary in a new field `a.lastGitInfo string` where it currently calls `composeSystem(gi)` (set `a.lastGitInfo = gi` there, and `a.lastGitInfo = ""` at the start of `run`). After the loop's final return paths — simplest: wrap with `defer func() { if a.Engine != nil { if err := a.Engine.Flush(); err != nil { a.notice("engine: %v; continuing without working memory", err) } } }()` at the top of `run`.

4. In `dispatch`, replace `res := a.Tools.Dispatch(ctx, call)` with:

```go
	var res tools.Result
	served := false
	if a.Engine != nil && (call.Name == "search" || call.Name == "lookup" || call.Name == "history") {
		if args, ok := parseArgs(call.Arguments); ok {
			if cached, hit := a.Engine.Cached(call.Name, args); hit {
				res, served = tools.Result{Content: cached}, true
			}
		}
	}
	if !served {
		res = a.Tools.Dispatch(ctx, call)
	}
	if a.Engine != nil && !served {
		if args, ok := parseArgs(call.Arguments); ok {
			if footer := a.observe(engine.Event{Tool: call.Name, Args: args, Content: res.Content, IsError: res.IsError}); footer != "" {
				res.Content = strings.TrimRight(res.Content, "\n") + "\n" + footer
			}
		}
	}
```

with, elsewhere in loop.go:

```go
func parseArgs(raw string) (map[string]any, bool) {
	args := map[string]any{}
	if strings.TrimSpace(raw) == "" {
		return args, true
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		return nil, false
	}
	return args, true
}

// observe hands a tool result to the engine; a panic there must not take
// the run down, so it is fenced.
func (a *Agent) observe(ev engine.Event) (footer string) {
	defer func() {
		if r := recover(); r != nil {
			a.notice("engine: %v; continuing without working memory", r)
			footer = ""
		}
	}()
	return a.Engine.Observe(ev)
}
```

5. In `RunFull`, after `a.autoVerifyUsed = false`:

```go
	if a.Engine != nil {
		a.Engine.EnsureTask(userInput)
		if head := gitctx.Head(ctx, a.Tools.Root); head != "" {
			sum := sha256.Sum256([]byte(gitctx.Porcelain(ctx, a.Tools.Root)))
			a.Engine.SetBaseline(engine.Baseline{Head: head, Dirty: hex.EncodeToString(sum[:8])})
		}
	}
```

`Run` (not only `RunFull`) must also seed the task line, since the tests and plain `run` call `Run`: put the `EnsureTask` call in `run` under `if newTurn && a.Engine != nil`.

6. In `internal/agent/extras.go`, `ExecutePlan` becomes:

```go
func (a *Agent) ExecutePlan(ctx context.Context, request, plan string) (string, *ReviewedReport, error) {
	if a.Engine != nil {
		a.Engine.SetPlan(request, engine.ParsePlanSteps(plan))
	}
	return a.RunFull(ctx, fmt.Sprintf(planExecutePrefix, request, plan))
}
```

7. In `internal/agent/handoff.go`, `Resume` already calls `composeSystem("")`, which now renders the block — no change needed beyond the test. In `WriteHandoff`, before `a.Session.Handoff = strings.TrimSpace(h)`:

```go
	if a.Engine != nil {
		if at := a.Engine.StoppedAt(); at != "" {
			h = strings.TrimSpace(h) + "\n\nStopped at: " + at
		}
	}
```

- [ ] **Step 5: Run the package**

Run: `go test -race ./internal/agent/ ./internal/gitctx/`
Expected: PASS (existing tests unaffected: `Engine` is nil in every other test).

- [ ] **Step 6: Commit**

```bash
git add internal/agent internal/gitctx
git commit -m "agent: working memory in the prompt, observed reads and cached lookups, task seeding, stopped-at in the handoff"
```

---

### Task 4: Compaction keeps digests instead of stubs

**Files:**
- Modify: `internal/agent/loop.go` (`Compact`, `compactSystemPrompt`)
- Test: `internal/agent/engine_test.go` (append)

**Interfaces:**
- Consumes: `Engine.HasDigest`, `Engine.Render`, `engine.SplitFilesBlock`, `Engine.ApplyFileNotes`.
- Produces: none new.

- [ ] **Step 1: Write the failing test**

```go
func TestCompactUsesDigestsAndFeedsFileNotesBack(t *testing.T) {
	var summaryReq provider.ChatRequest
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		if strings.HasPrefix(req.Messages[0].Content, "Summarize this coding-agent") {
			summaryReq = req
			return &provider.ChatResponse{Content: "The task is x.\n\nfiles:\n- a.go — defines A\n"}, nil
		}
		return &provider.ChatResponse{Content: "ok"}, nil
	}}
	ag, dir := newTestAgent(t, p, nil)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\nfunc A() {}\n"), 0o644)
	st := withEngine(t, ag)
	st.NextTurn()
	st.Observe(engine.Event{Tool: "read_file", Args: map[string]any{"path": "a.go"}, Content: "    1\tpackage a\n    2\tfunc A() {}\n"})
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "read a.go"})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "1", Name: "read_file", Arguments: `{"path":"a.go"}`}}})
	ag.History.Add(provider.Message{Role: provider.RoleTool, ToolCallID: "1", Name: "read_file", Content: "    1\tpackage a\n    2\tfunc A() {}\n"})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "A is defined."})
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "next"})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "ok"})
	if err := ag.Compact(context.Background()); err != nil {
		t.Fatal(err)
	}
	u := summaryReq.Messages[1].Content
	if !strings.Contains(u, "(read a.go lines 1–2; digested)") || strings.Contains(u, "package a") {
		t.Fatalf("summary transcript still carries the read:\n%s", u)
	}
	if !strings.Contains(u, "Working memory:") || !strings.Contains(summaryReq.Messages[0].Content, "Do not restate anything already in Working memory.") {
		t.Fatalf("summary request lacks the block or the instruction:\n%s\n%s", summaryReq.Messages[0].Content, u)
	}
	if st.Digests()[0].Note != "defines A" {
		t.Fatalf("file note not applied: %+v", st.Digests()[0])
	}
	if strings.Contains(ag.History.Messages[0].Content, "files:") {
		t.Fatalf("files block not stripped from the stored summary:\n%s", ag.History.Messages[0].Content)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/agent/ -run TestCompactUsesDigests 2>&1 | grep -E "^(---|FAIL|ok)" | head`
Expected: FAIL (`summary transcript still carries the read`).

- [ ] **Step 3: Implement**

In `Compact`'s transcript loop, track pending calls and stub digested reads:

```go
	pending := map[string]string{} // tool call id → path (read_file only)
	for i, m := range head {
		if i == 0 && strings.HasPrefix(m.Content, summaryPrefix) {
			prior = strings.TrimPrefix(m.Content, summaryPrefix)
			continue
		}
		if task == "" && m.Role == provider.RoleUser && !isToolResult(m) {
			task = m.Content
		}
		for _, tc := range m.ToolCalls {
			if tc.Name == "read_file" {
				if args, ok := parseArgs(tc.Arguments); ok {
					if p, _ := args["path"].(string); p != "" {
						pending[tc.ID] = p
					}
				}
			}
		}
		if a.Engine != nil && m.Role == provider.RoleTool {
			if p, ok := pending[m.ToolCallID]; ok {
				if r, has := a.Engine.HasDigest(p); has {
					fmt.Fprintf(&b, "[tool] (read %s lines %d–%d; digested)\n", p, r.From, r.To)
					continue
				}
			}
		}
		fmt.Fprintf(&b, "[%s] %.600s\n", m.Role, m.Content)
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&b, "  (called %s %.200s)\n", tc.Name, tc.Arguments)
		}
	}
```

(Embedded-mode results are `RoleUser` `<tool_result>` blocks with no call id; they keep the stub — a compat session still benefits from the block itself.)

When building the user message `u`, after the `Original task` and prior summary sections and before the transcript, add:

```go
	if a.Engine != nil {
		if wm := a.Engine.Render(a.Cfg.Engine.Budget, a.inRepoMap); wm != "" {
			fmt.Fprintf(&u, "Working memory (already kept; do not restate):\n%s\n\n", wm)
		}
	}
```

Change the system prompt:

```go
const compactSystemPrompt = "Summarize this coding-agent conversation for context compression. Preserve, in this order: the original task; every requirement, constraint or convention the user stated; key decisions and why; files created or modified and how; current state; outstanding work. Under 400 words. Plain text. Do not restate anything already in Working memory. End with a line `files:` followed by one line per file that mattered, as `- path — what matters in it`."
```

After `summary` is cleaned and before it is stored:

```go
	if a.Engine != nil {
		body, files := engine.SplitFilesBlock(summary)
		if files != "" {
			a.Engine.ApplyFileNotes(files)
		}
		summary = body
		if err := a.Engine.Flush(); err != nil {
			a.notice("engine: %v; continuing without working memory", err)
		}
	}
```

- [ ] **Step 4: Run the package**

Run: `go test -race ./internal/agent/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agent
git commit -m "agent: compaction stubs digested reads and feeds the summary's file notes back into working memory"
```

---

### Task 5: The `task` tool and guidance

**Files:**
- Create: `internal/tools/task.go`, `internal/tools/task_test.go`
- Modify: `internal/agent/prompt.go` (guidance when `task` is registered), `internal/agent/extras.go` (`planAgent` subset)
- Test: `internal/agent/engine_test.go` (append one prompt test)

**Interfaces:**
- Consumes: `engine.Store` methods `SetPlan`, `SetStep`, `AddNote` (through an interface defined in `tools`).
- Produces: `tools.TaskLedger` interface `{ SetPlan(task string, steps []string); SetStep(i int, status string) error; AddNote(text, file string, decision, keep bool) error }`; `tools.NewTask(l TaskLedger) Tool` (name `task`).

- [ ] **Step 1: Write the failing tests**

`internal/tools/task_test.go`:

```go
package tools

import (
	"context"
	"strings"
	"testing"
)

type fakeLedger struct {
	task  string
	steps []string
	marks []string
	notes []string
	err   error
}

func (f *fakeLedger) SetPlan(task string, steps []string) { f.task, f.steps = task, steps }
func (f *fakeLedger) SetStep(i int, status string) error {
	f.marks = append(f.marks, strings.Repeat("x", i)+status)
	return f.err
}
func (f *fakeLedger) AddNote(text, file string, decision, keep bool) error {
	if text == "" {
		return errNoteText
	}
	f.notes = append(f.notes, text+"|"+file+"|"+boolStr(decision)+boolStr(keep))
	return nil
}
func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func TestTaskToolActionsAndAliases(t *testing.T) {
	f := &fakeLedger{}
	tool := NewTask(f)
	ctx := context.Background()
	if r := tool.Run(ctx, map[string]any{}); !r.IsError || r.Content != "task needs an action (plan, step, note)" {
		t.Fatalf("no action: %+v", r)
	}
	r := tool.Run(ctx, map[string]any{"action": "plan", "text": "add flag", "steps": "parse\nwire\n"})
	if r.IsError || f.task != "add flag" || len(f.steps) != 2 || !strings.Contains(r.Content, "2 steps") {
		t.Fatalf("plan: %+v %+v", r, f)
	}
	tool.Run(ctx, map[string]any{"action": "plan", "text": "t", "steps": []any{"a", "b", "c"}})
	if len(f.steps) != 3 {
		t.Fatal("array steps")
	}
	tool.Run(ctx, map[string]any{"action": "step", "step": 2, "status": "progress"})
	tool.Run(ctx, map[string]any{"action": "step", "step": "3", "status": "finished"})
	if f.marks[0] != "xxdoing" || f.marks[1] != "xxxdone" {
		t.Fatalf("marks %v", f.marks)
	}
	if r := tool.Run(ctx, map[string]any{"action": "step", "step": 1, "status": "bogus"}); !r.IsError || !strings.Contains(r.Content, "status must be doing, done or skip") {
		t.Fatalf("bad status: %+v", r)
	}
	tool.Run(ctx, map[string]any{"action": "decision", "text": "use cobra"})
	tool.Run(ctx, map[string]any{"action": "note", "text": "flags here", "file": "cmd/root.go", "remember": true})
	if f.notes[0] != "use cobra||10" || f.notes[1] != "flags here|cmd/root.go|01" {
		t.Fatalf("notes %v", f.notes)
	}
	if r := tool.Run(ctx, map[string]any{"action": "note"}); !r.IsError || r.Content != "note needs text" {
		t.Fatalf("empty note: %+v", r)
	}
	if !strings.Contains(tool.Description(), "plan") || !strings.Contains(string(tool.Schema()), `"action"`) {
		t.Fatal("description/schema")
	}
}
```

Add `var errNoteText = fmt.Errorf("note needs text")` in the test file (import `fmt`).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/tools/ -run TestTaskTool 2>&1 | head -3`
Expected: build failure (`undefined: NewTask`).

- [ ] **Step 3: Implement `internal/tools/task.go`**

```go
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// TaskLedger is what the task tool writes to: the engine's store, behind an
// interface so tools never imports engine.
type TaskLedger interface {
	SetPlan(task string, steps []string)
	SetStep(i int, status string) error
	AddNote(text, file string, decision, keep bool) error
}

type taskTool struct{ l TaskLedger }

// NewTask builds the task tool over a ledger.
func NewTask(l TaskLedger) Tool { return &taskTool{l: l} }

func (t *taskTool) Name() string { return "task" }

func (t *taskTool) Description() string {
	return "Keep your working memory. action=plan records the task and its steps before a multi-step change; action=step marks a step doing/done/skip as you go; action=note records a fact or decision (with file: what matters in that file, so you need not read it again; with keep: remember it for future sessions). The ledger is always shown to you under Working memory."
}

func (t *taskTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"action":{"type":"string","enum":["plan","step","note"],"description":"plan | step | note"},
		"text":{"type":"string","description":"plan: the task in one line; note: the fact or decision"},
		"steps":{"type":"array","items":{"type":"string"},"description":"plan: the steps in order"},
		"step":{"type":"integer","description":"step: 1-based step number"},
		"status":{"type":"string","enum":["doing","done","skip"],"description":"step: the new status"},
		"file":{"type":"string","description":"note: the file this note is about"},
		"keep":{"type":"boolean","description":"note: also remember this across sessions"}},
		"required":["action"]}`)
}

func (t *taskTool) Run(_ context.Context, args map[string]any) Result {
	action := strings.ToLower(strings.TrimSpace(argString(args, "action", "op", "do")))
	switch action {
	case "plan":
		steps := argStrings(args, "steps")
		if len(steps) == 0 {
			if s := argString(args, "steps"); s != "" {
				for _, line := range strings.Split(s, "\n") {
					if line = strings.TrimSpace(strings.TrimLeft(line, "-*0123456789.) ")); line != "" {
						steps = append(steps, line)
					}
				}
			}
		}
		t.l.SetPlan(argString(args, "text", "task", "title"), steps)
		return Result{Content: fmt.Sprintf("plan recorded (%d steps)", len(steps))}
	case "step":
		i := argInt(args, 0, "step", "n", "index")
		if s := argString(args, "step"); i == 0 && s != "" {
			fmt.Sscanf(s, "%d", &i)
		}
		status := strings.ToLower(strings.TrimSpace(argString(args, "status", "state")))
		switch status {
		case "doing", "progress", "in_progress", "started":
			status = "doing"
		case "done", "finished", "complete", "completed":
			status = "done"
		case "skip", "skipped":
			status = "skip"
		default:
			return Result{IsError: true, Content: "status must be doing, done or skip"}
		}
		if err := t.l.SetStep(i, status); err != nil {
			return Result{IsError: true, Content: err.Error()}
		}
		return Result{Content: fmt.Sprintf("step %d: %s", i, status)}
	case "note", "decision", "fact":
		keep := argBool(args, false, "keep", "remember", "durable")
		if err := t.l.AddNote(argString(args, "text", "note", "fact"), argString(args, "file", "path"), action == "decision", keep); err != nil {
			return Result{IsError: true, Content: err.Error()}
		}
		return Result{Content: "noted"}
	}
	return Result{IsError: true, Content: "task needs an action (plan, step, note)"}
}

// argStrings reads a string array argument (a single string is not one).
func argStrings(args map[string]any, keys ...string) []string {
	for _, k := range keys {
		if v, ok := args[k].([]any); ok {
			var out []string
			for _, e := range v {
				if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, strings.TrimSpace(s))
				}
			}
			return out
		}
	}
	return nil
}
```

(`argInt` with a `string` value: check `argInt`'s implementation in `tool.go`; if it already parses numeric strings, drop the `Sscanf` fallback. If a helper named `argStrings` already exists in `tools`, reuse it instead of adding one.)

- [ ] **Step 4: Guidance and plan subset**

In `internal/agent/prompt.go`, `BuildSystemPrompt(specs, compat, notes)`: where the tool guidance paragraph is assembled, if any spec's name is `task`, append the sentences:

```
Before a change that takes several steps, record a plan with the task tool and mark steps as you go. When a file matters for later, note what matters in it (task note with file) instead of planning to read it again. Working memory below lists what you have already read; do not read those files again unless they are marked changed.
```

In `internal/agent/extras.go`, `planAgent`'s `Subset` gains `"task", "lookup", "history", "show", "changes"`.

Append to `internal/agent/engine_test.go`:

```go
func TestPromptCarriesTaskGuidanceWhenTheToolExists(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	if strings.Contains(ag.History.System.Content, "record a plan with the task tool") {
		t.Fatal("guidance without the tool")
	}
	st := withEngine(t, ag)
	ag.Tools.AddTool(tools.NewTask(st))
	ag.RefreshSystem()
	if !strings.Contains(ag.History.System.Content, "record a plan with the task tool") {
		t.Fatal("guidance missing")
	}
}
```

- [ ] **Step 5: Run**

Run: `go test -race ./internal/tools/ ./internal/agent/`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/tools/task.go internal/tools/task_test.go internal/agent
git commit -m "tools: the task tool (plan, step, note) and its prompt guidance"
```

---

### Task 6: Git-backed lookup tools

**Files:**
- Create: `internal/tools/gittools.go`, `internal/tools/gittools_test.go`

**Interfaces:**
- Consumes: `RunShell`, `Registry.resolve`, the `search` tool (fallback).
- Produces: `tools.BaselineFunc func() (head, dirty string)`; `tools.NewGitTools(r *Registry, baseline BaselineFunc, minimal bool) []Tool` returning `lookup` (always) and, unless `minimal`, `history`, `show`, `changes`. `dirty` is the porcelain text recorded at the task start (cmd passes a closure reading the engine's ledger baseline; engine stores only its hash, so cmd also keeps the text — see Task 7).

- [ ] **Step 1: Write the failing tests**

```go
package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitWorkspace(t *testing.T) *Registry {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	ctx := context.Background()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nfunc Alpha() int {\n\treturn 1\n}\n"), 0o644)
	for _, c := range []string{"git init -q -b main", "git config user.email t@t", "git config user.name t", "git add -A", "git commit -qm one"} {
		if out, err := RunShell(ctx, dir, c, 30*time.Second); err != nil {
			t.Fatalf("%s: %v %s", c, err, out)
		}
	}
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nfunc Alpha() int {\n\treturn 2\n}\n\nfunc Beta() {}\n"), 0o644)
	for _, c := range []string{"git add -A", "git commit -qm two"} {
		RunShell(ctx, dir, c, 30*time.Second)
	}
	reg, err := NewRegistry(dir, func(string, string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func byName(ts []Tool, name string) Tool {
	for _, t := range ts {
		if t.Name() == name {
			return t
		}
	}
	return nil
}

func TestLookupFindsSymbolsWithFunctionContext(t *testing.T) {
	reg := gitWorkspace(t)
	ts := NewGitTools(reg, func() (string, string) { return "", "" }, false)
	if len(ts) != 4 {
		t.Fatalf("%d tools", len(ts))
	}
	r := byName(ts, "lookup").Run(context.Background(), map[string]any{"query": "Alpha", "symbol": true})
	if r.IsError || !strings.Contains(r.Content, "a.go:3") || !strings.Contains(r.Content, "return 2") {
		t.Fatalf("lookup: %+v", r)
	}
	if r := byName(ts, "lookup").Run(context.Background(), map[string]any{}); !r.IsError || r.Content != "lookup needs a query" {
		t.Fatalf("empty: %+v", r)
	}
	if r := byName(ts, "lookup").Run(context.Background(), map[string]any{"query": "Alp.a", "regex": true}); r.IsError || !strings.Contains(r.Content, "a.go:3") {
		t.Fatalf("regex: %+v", r)
	}
}

func TestHistoryModes(t *testing.T) {
	reg := gitWorkspace(t)
	h := byName(NewGitTools(reg, nil, false), "history")
	ctx := context.Background()
	if r := h.Run(ctx, map[string]any{"path": "a.go", "symbol": "Alpha"}); r.IsError || !strings.Contains(r.Content, "two") || !strings.Contains(r.Content, "return 2") {
		t.Fatalf("symbol: %+v", r)
	}
	if r := h.Run(ctx, map[string]any{"path": "a.go", "query": "Beta"}); r.IsError || !strings.Contains(r.Content, "two") || strings.Contains(r.Content, "one\n") {
		t.Fatalf("pickaxe: %+v", r)
	}
	if r := h.Run(ctx, map[string]any{"path": "a.go", "blame": true, "lines": "3,4"}); r.IsError || !strings.Contains(r.Content, "return 2") {
		t.Fatalf("blame: %+v", r)
	}
	if r := h.Run(ctx, map[string]any{"path": "a.go"}); !r.IsError || !strings.Contains(r.Content, "one of symbol, lines, query or blame") {
		t.Fatalf("no mode: %+v", r)
	}
}

func TestShowAndChanges(t *testing.T) {
	reg := gitWorkspace(t)
	ts := NewGitTools(reg, func() (string, string) {
		out, _ := RunShell(context.Background(), reg.Root, "git rev-parse HEAD~1", 10*time.Second)
		return strings.TrimSpace(out), ""
	}, false)
	ctx := context.Background()
	if r := byName(ts, "show").Run(ctx, map[string]any{"rev": "HEAD~1", "path": "a.go"}); r.IsError || !strings.Contains(r.Content, "return 1") || strings.Contains(r.Content, "Beta") {
		t.Fatalf("show: %+v", r)
	}
	if r := byName(ts, "show").Run(ctx, map[string]any{"rev": "HEAD~1", "path": "../etc/passwd"}); !r.IsError {
		t.Fatal("show escaped the root")
	}
	r := byName(ts, "changes").Run(ctx, map[string]any{})
	if r.IsError || !strings.Contains(r.Content, "a.go") || !strings.Contains(r.Content, "1 file changed") {
		t.Fatalf("changes since baseline: %+v", r)
	}
	r = byName(ts, "changes").Run(ctx, map[string]any{"path": "a.go"})
	if r.IsError || !strings.Contains(r.Content, "+func Beta") {
		t.Fatalf("changes diff: %+v", r)
	}
	if r := byName(ts, "changes").Run(ctx, map[string]any{"since": "HEAD"}); r.IsError || !strings.Contains(r.Content, "no changes") {
		t.Fatalf("clean: %+v", r)
	}
}

func TestGitToolsOutsideARepository(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\nfunc Only() {}\n"), 0o644)
	reg, _ := NewRegistry(dir, func(string, string) bool { return true })
	ts := NewGitTools(reg, nil, false)
	if r := byName(ts, "lookup").Run(context.Background(), map[string]any{"query": "Only"}); r.IsError || !strings.Contains(r.Content, "x.go:2") {
		t.Fatalf("lookup fallback: %+v", r)
	}
	for _, name := range []string{"history", "show", "changes"} {
		if r := byName(ts, name).Run(context.Background(), map[string]any{"path": "x.go", "symbol": "Only"}); !r.IsError || r.Content != "not a git repository" {
			t.Fatalf("%s outside repo: %+v", name, r)
		}
	}
	if len(NewGitTools(reg, nil, true)) != 1 {
		t.Fatal("minimal must register lookup only")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/tools/ -run 'TestLookup|TestHistory|TestShowAnd|TestGitTools' 2>&1 | head -3`
Expected: build failure (`undefined: NewGitTools`).

- [ ] **Step 3: Implement `internal/tools/gittools.go`**

```go
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Git-backed lookups: read-only, confined to the workspace, never prompt.
// They return the function, the hunk or the delta the model needs instead
// of whole files. Outside a repository lookup falls back to the walk-based
// search and the others say so.

const gitTimeout = 20 * time.Second

// BaselineFunc reports where the current task began: the HEAD hash and the
// porcelain status at that moment ("" when unknown).
type BaselineFunc func() (head, dirty string)

// NewGitTools builds lookup, history, show and changes (lookup only when minimal).
func NewGitTools(r *Registry, baseline BaselineFunc, minimal bool) []Tool {
	if baseline == nil {
		baseline = func() (string, string) { return "", "" }
	}
	ts := []Tool{&lookupTool{r: r}}
	if minimal {
		return ts
	}
	return append(ts, &historyTool{r: r}, &showTool{r: r}, &changesTool{r: r, baseline: baseline})
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func (r *Registry) git(ctx context.Context, args ...string) (string, error) {
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = shellQuote(a)
	}
	out, err := RunShell(ctx, r.Root, "git "+strings.Join(quoted, " "), gitTimeout)
	return strings.TrimRight(out, "\n"), err
}

func (r *Registry) isRepo(ctx context.Context) bool {
	out, err := r.git(ctx, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

// relInRoot confines a user path to the workspace and returns it relative.
func (r *Registry) relInRoot(p string) (string, error) {
	abs, err := r.resolve(p)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(r.Root, abs)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

func gitErr(err error, out string) Result {
	msg := strings.TrimSpace(out)
	if msg == "" {
		msg = err.Error()
	}
	return Result{IsError: true, Content: msg}
}

// ---- lookup ----------------------------------------------------------------

type lookupTool struct{ r *Registry }

func (t *lookupTool) Name() string { return "lookup" }
func (t *lookupTool) Description() string {
	return "Find where something is defined or used, fast: git grep over tracked files. Returns file:line: text. symbol=true returns the whole enclosing function for each hit, which is usually what you needed the file for."
}
func (t *lookupTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"query":{"type":"string","description":"Text to find (literal unless regex=true)"},
		"path":{"type":"string","description":"Limit to a directory or file"},
		"symbol":{"type":"boolean","description":"Show the enclosing function for each hit"},
		"regex":{"type":"boolean","description":"Treat query as a regular expression"}},
		"required":["query"]}`)
}
func (t *lookupTool) Run(ctx context.Context, args map[string]any) Result {
	q := strings.TrimSpace(argString(args, "query", "pattern", "q", "text"))
	if q == "" {
		return Result{IsError: true, Content: "lookup needs a query"}
	}
	if !t.r.isRepo(ctx) {
		if s, ok := t.r.byName["search"]; ok {
			pat := q
			if !argBool(args, false, "regex") {
				pat = regexpQuote(q)
			}
			fallback := map[string]any{"pattern": pat}
			if p := argString(args, "path"); p != "" {
				fallback["path"] = p
			}
			return s.Run(ctx, fallback)
		}
		return Result{IsError: true, Content: "not a git repository"}
	}
	gargs := []string{"grep", "-n", "-I", "--no-color"}
	if !argBool(args, false, "regex") {
		gargs = append(gargs, "-F")
	} else {
		gargs = append(gargs, "-E")
	}
	if argBool(args, false, "symbol", "context", "function") {
		gargs = append(gargs, "-W")
	}
	gargs = append(gargs, "-e", q, "--")
	if p := argString(args, "path", "dir"); p != "" {
		rel, err := t.r.relInRoot(p)
		if err != nil {
			return Result{IsError: true, Content: err.Error()}
		}
		gargs = append(gargs, rel)
	} else {
		gargs = append(gargs, ".")
	}
	out, err := t.r.git(ctx, gargs...)
	if err != nil && strings.TrimSpace(out) == "" {
		return Result{Content: "no matches"}
	}
	if err != nil {
		return gitErr(err, out)
	}
	return Result{Content: truncate(out, t.r.MaxOutput)}
}

func regexpQuote(s string) string {
	var b strings.Builder
	for _, c := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, c) {
			b.WriteByte('\\')
		}
		b.WriteRune(c)
	}
	return b.String()
}

// ---- history ---------------------------------------------------------------

type historyTool struct{ r *Registry }

func (t *historyTool) Name() string { return "history" }
func (t *historyTool) Description() string {
	return "Why is the code this way: git history for one file. Give path plus one of symbol (that function's own history), lines \"a,b\" (that range's history), query (commits that added or removed the text), or blame=true with lines."
}
func (t *historyTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"path":{"type":"string","description":"File to look at"},
		"symbol":{"type":"string","description":"Function or type name: its own history"},
		"lines":{"type":"string","description":"Line range a,b"},
		"query":{"type":"string","description":"Text whose introduction or removal you want to find"},
		"blame":{"type":"boolean","description":"Who last touched each line in lines"},
		"limit":{"type":"integer","description":"Newest N entries (default 10)"}},
		"required":["path"]}`)
}
func (t *historyTool) Run(ctx context.Context, args map[string]any) Result {
	if !t.r.isRepo(ctx) {
		return Result{IsError: true, Content: "not a git repository"}
	}
	rel, err := t.r.relInRoot(argString(args, "path", "file"))
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	limit := argInt(args, 10, "limit", "n")
	symbol, lines, query := argString(args, "symbol", "function"), argString(args, "lines", "range"), argString(args, "query", "text")
	var gargs []string
	switch {
	case argBool(args, false, "blame") && lines != "":
		gargs = []string{"blame", "-L", lines, "--date=short", "--", rel}
	case symbol != "":
		gargs = []string{"log", "-L", ":" + symbol + ":" + rel, "--date=short", "--format=%h %ad %an %s", "-n", strconv.Itoa(limit)}
	case lines != "":
		gargs = []string{"log", "-L", lines + ":" + rel, "--date=short", "--format=%h %ad %an %s", "-n", strconv.Itoa(limit)}
	case query != "":
		gargs = []string{"log", "-S", query, "--date=short", "--format=%h %ad %an %s", "-n", strconv.Itoa(limit), "--", rel}
	default:
		return Result{IsError: true, Content: "history needs one of symbol, lines, query or blame with lines"}
	}
	out, err := t.r.git(ctx, gargs...)
	if err != nil {
		return gitErr(err, out)
	}
	if strings.TrimSpace(out) == "" {
		out = "no history"
	}
	return Result{Content: truncate(out, t.r.MaxOutput)}
}

// ---- show ------------------------------------------------------------------

type showTool struct{ r *Registry }

func (t *showTool) Name() string { return "show" }
func (t *showTool) Description() string {
	return "Read a file as it was at a revision (default HEAD), without touching the working tree. Use offset/limit like read_file."
}
func (t *showTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"path":{"type":"string","description":"File path"},
		"rev":{"type":"string","description":"Commit, branch or tag (default HEAD)"},
		"offset":{"type":"integer","description":"1-based first line"},
		"limit":{"type":"integer","description":"Max lines (default 400)"}},
		"required":["path"]}`)
}
func (t *showTool) Run(ctx context.Context, args map[string]any) Result {
	if !t.r.isRepo(ctx) {
		return Result{IsError: true, Content: "not a git repository"}
	}
	rel, err := t.r.relInRoot(argString(args, "path", "file"))
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	rev := argString(args, "rev", "revision", "commit")
	if rev == "" {
		rev = "HEAD"
	}
	if strings.HasPrefix(rev, "-") {
		return Result{IsError: true, Content: "bad revision"}
	}
	out, err := t.r.git(ctx, "show", rev+":"+rel)
	if err != nil {
		return gitErr(err, out)
	}
	lines := strings.Split(out, "\n")
	offset := argInt(args, 1, "offset", "start")
	limit := argInt(args, 400, "limit", "count")
	if offset < 1 {
		offset = 1
	}
	if offset > len(lines) {
		return Result{IsError: true, Content: fmt.Sprintf("offset %d beyond end of file (%d lines)", offset, len(lines))}
	}
	end := offset - 1 + limit
	if end > len(lines) {
		end = len(lines)
	}
	var b strings.Builder
	for i := offset - 1; i < end; i++ {
		fmt.Fprintf(&b, "%5d\t%s\n", i+1, lines[i])
	}
	if end < len(lines) {
		fmt.Fprintf(&b, "... (%d more lines; call show again with offset=%d)\n", len(lines)-end, end+1)
	}
	return Result{Content: truncate(b.String(), t.r.MaxOutput)}
}

// ---- changes ---------------------------------------------------------------

type changesTool struct {
	r        *Registry
	baseline BaselineFunc
}

func (t *changesTool) Name() string { return "changes" }
func (t *changesTool) Description() string {
	return "What this task has changed so far: a diff stat against the point where the task began (or since=<rev>), and with path the full diff of one file. Use it before verifying or reviewing instead of re-reading whole files."
}
func (t *changesTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"since":{"type":"string","description":"Revision to compare against (default: where the task began)"},
		"path":{"type":"string","description":"Show the full diff of this file"}},
		"required":[]}`)
}
func (t *changesTool) Run(ctx context.Context, args map[string]any) Result {
	if !t.r.isRepo(ctx) {
		return Result{IsError: true, Content: "not a git repository"}
	}
	since := argString(args, "since", "rev", "from")
	head, dirty := "", ""
	if since == "" {
		head, dirty = t.baseline()
		since = head
	}
	if since == "" {
		since = "HEAD"
	}
	if strings.HasPrefix(since, "-") {
		return Result{IsError: true, Content: "bad revision"}
	}
	var out string
	var err error
	if p := argString(args, "path", "file"); p != "" {
		rel, rerr := t.r.relInRoot(p)
		if rerr != nil {
			return Result{IsError: true, Content: rerr.Error()}
		}
		out, err = t.r.git(ctx, "diff", "--no-color", since, "--", rel)
	} else {
		out, err = t.r.git(ctx, "diff", "--stat", "--no-color", since, "--")
	}
	if err != nil {
		return gitErr(err, out)
	}
	if strings.TrimSpace(out) == "" {
		out = "no changes since " + since
	}
	if dirty != "" {
		var pre []string
		for _, l := range strings.Split(dirty, "\n") {
			if len(l) > 3 {
				pre = append(pre, strings.TrimSpace(l[3:]))
			}
		}
		if len(pre) > 0 {
			out += "\n(already modified before this task began: " + strings.Join(pre, ", ") + ")"
		}
	}
	return Result{Content: truncate(out, t.r.MaxOutput)}
}
```

`argBool` must accept `true`/`"true"`/`1` — check `tool.go:266`; extend it if it only handles `bool`.

- [ ] **Step 4: Run**

Run: `go test -race ./internal/tools/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tools/gittools.go internal/tools/gittools_test.go
git commit -m "tools: lookup, history, show and changes — git-backed, read-only, root-confined"
```

---

### Task 7: cmd wiring and the user commands

**Files:**
- Modify: `cmd/root.go` (open the store, register tools, flush on exit), `internal/ui/common.go` (rows + busy-safe), `internal/ui/repl.go` (`/task`, `/notes`), `internal/tui/view.go` (`/task`, `/notes`)
- Test: `internal/ui/repl_test.go` (append), `internal/tui/cowork_test.go` → new `internal/tui/engine_test.go`, `cmd/engine_test.go` (new)

**Interfaces:**
- Consumes: `engine.Open`, `Agent.SetEngine`, `tools.NewTask`, `tools.NewGitTools`, `Agent.Engine`.
- Produces: `cmd` keeps the baseline porcelain text in a package var `taskBaselineDirty` set by a `BaselineFunc` closure — see Step 3.

- [ ] **Step 1: Write the failing tests**

Append to `internal/ui/repl_test.go`:

```go
func TestPlainTaskAndNotesCommands(t *testing.T) {
	r := newTestREPL(t)
	out := capture(t, func() { r.command(context.Background(), "/task") })
	if !strings.Contains(out, "working memory is off") {
		t.Fatalf("no engine:\n%s", out)
	}
	st, err := engine.OpenAt(filepath.Join(t.TempDir(), "e"), r.Agent.Tools.Root, "s", false, 4096)
	if err != nil {
		t.Fatal(err)
	}
	r.Agent.SetEngine(st)
	st.SetPlan("add flag", []string{"parse", "wire"})
	st.SetStep(1, "doing")
	out = capture(t, func() { r.command(context.Background(), "/task") })
	if !strings.Contains(out, "task: add flag") || !strings.Contains(out, "[>] 1. parse") {
		t.Fatalf("/task:\n%s", out)
	}
	capture(t, func() { r.command(context.Background(), "/notes add tests need go") })
	out = capture(t, func() { r.command(context.Background(), "/notes") })
	if !strings.Contains(out, "1. tests need go") {
		t.Fatalf("/notes:\n%s", out)
	}
	capture(t, func() { r.command(context.Background(), "/notes drop 1") })
	if out = capture(t, func() { r.command(context.Background(), "/notes") }); !strings.Contains(out, "no notes") {
		t.Fatalf("after drop:\n%s", out)
	}
	capture(t, func() { r.command(context.Background(), "/task clear") })
	if st.Ledger().Task != "" {
		t.Fatal("/task clear did not clear")
	}
}
```

Create `internal/tui/engine_test.go`:

```go
package tui

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/engine"
)

func TestTaskAndNotesAreViewLocal(t *testing.T) {
	s, a, b := twoViews(t)
	a.slashCommand("/task")
	if !strings.Contains(a.rendered.String(), "working memory is off") {
		t.Fatalf("no engine:\n%s", a.rendered.String())
	}
	st, err := engine.OpenAt(filepath.Join(t.TempDir(), "e"), s.ag.Tools.Root, "s", false, 4096)
	if err != nil {
		t.Fatal(err)
	}
	s.ag.SetEngine(st)
	st.SetPlan("add flag", []string{"parse"})
	a.slashCommand("/task")
	a.slashCommand("/notes add remember me")
	a.slashCommand("/notes")
	if !strings.Contains(a.rendered.String(), "task: add flag") || !strings.Contains(a.rendered.String(), "1. remember me") {
		t.Fatalf("view a:\n%s", a.rendered.String())
	}
	if strings.Contains(b.rendered.String(), "add flag") {
		t.Fatal("listing leaked to the other terminal")
	}
}
```

Create `cmd/engine_test.go`:

```go
package cmd

import (
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/tools"
)

func TestEngineToolsFollowTheConfig(t *testing.T) {
	reg, _ := tools.NewRegistry(t.TempDir(), func(string, string) bool { return true })
	cfg := config.Default()
	registerEngineTools(cfg, reg, nil, func() (string, string) { return "", "" })
	names := map[string]bool{}
	for _, n := range reg.Names() {
		names[n] = true
	}
	for _, want := range []string{"task", "lookup", "history", "show", "changes"} {
		if !names[want] {
			t.Fatalf("full: %s missing (%v)", want, reg.Names())
		}
	}
	reg2, _ := tools.NewRegistry(t.TempDir(), func(string, string) bool { return true })
	cfg.Engine.Tools = "minimal"
	registerEngineTools(cfg, reg2, nil, nil)
	names = map[string]bool{}
	for _, n := range reg2.Names() {
		names[n] = true
	}
	if !names["task"] || !names["lookup"] || names["history"] {
		t.Fatalf("minimal: %v", reg2.Names())
	}
	reg3, _ := tools.NewRegistry(t.TempDir(), func(string, string) bool { return true })
	cfg.Engine.Enabled = false
	registerEngineTools(cfg, reg3, nil, nil)
	if len(reg3.Names()) != len(reg.Names())-5 {
		t.Fatalf("disabled registered tools: %v", reg3.Names())
	}
}
```

(`registerEngineTools(cfg, reg, ledger tools.TaskLedger, baseline tools.BaselineFunc)` — a nil ledger still registers `task` over a no-op ledger only in tests; in production the store is always passed. Simplest: when `ledger == nil`, pass `noopLedger{}` defined in `cmd/engine.go`.)

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/ui/ ./internal/tui/ ./cmd/ 2>&1 | grep -E "undefined|FAIL" | head -3`
Expected: build failures (`SetEngine` exists, but `/task` prints `unknown command` and `registerEngineTools` is undefined).

- [ ] **Step 3: cmd wiring**

Create `cmd/engine.go`:

```go
package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/gitctx"
	"github.com/brown-enterprises/be-code/internal/tools"
)

type noopLedger struct{}

func (noopLedger) SetPlan(string, []string)                 {}
func (noopLedger) SetStep(int, string) error                { return nil }
func (noopLedger) AddNote(string, string, bool, bool) error { return nil }

// registerEngineTools adds task and the git lookups per cfg.Engine.
func registerEngineTools(cfg *config.Config, reg *tools.Registry, ledger tools.TaskLedger, baseline tools.BaselineFunc) {
	if !cfg.Engine.Enabled {
		return
	}
	if ledger == nil {
		ledger = noopLedger{}
	}
	reg.AddTool(tools.NewTask(ledger))
	for _, t := range tools.NewGitTools(reg, baseline, cfg.Engine.Tools == "minimal") {
		reg.AddTool(t)
	}
}

// attachEngine opens the workspace's working memory once the session id is
// known and wires the tools. A store that cannot be opened is a warning,
// not a failure: the session runs as it did without one.
func attachEngine(cfg *config.Config, reg *tools.Registry, ag *agent.Agent, resumed bool) {
	if !cfg.Engine.Enabled || ag.Session == nil {
		return
	}
	st, err := engine.Open(reg.Root, ag.Session.ID, resumed, cfg.Engine.NotesCap)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: engine: %v; continuing without working memory\n", err)
		return
	}
	ag.SetEngine(st)
	var dirtyText string
	baseline := func() (string, string) {
		b := st.Ledger().Baseline
		if b.Head == "" {
			return "", ""
		}
		if dirtyText == "" && b.Dirty != "" {
			dirtyText = gitctx.Porcelain(context.Background(), reg.Root)
		}
		return b.Head, dirtyText
	}
	registerEngineTools(cfg, reg, st, baseline)
	ag.RefreshSystem()
}
```

(The porcelain text at task start is re-read lazily on first `changes` call; the engine stores only its hash. This is a documented approximation: files dirtied *during* the task before the first `changes` call would be listed as pre-existing. Acceptable; the stat itself is always exact.)

In `cmd/root.go:buildAgent`, right after the `if flagResume != "" { … } else { ag.SetSession(…) }` block and before `applyBackendWindow`: `attachEngine(cfg, reg, ag, flagResume != "")`. In `finishSession`, before `WriteHandoff`: `if ag.Engine != nil { if err := ag.Engine.Flush(); err != nil { fmt.Fprintf(os.Stderr, "warn: engine: %v\n", err) } }`.

- [ ] **Step 4: Slash table and plain mode**

`internal/ui/common.go`: after the `/consult` row add `{"/task", "show the task ledger, or /task clear to reset this session's working memory", true}` and `{"/notes", "durable project notes: /notes [add <text>|drop N|clear]", true}`; add both to `busySafe` with the comment `// Listings and edits of the store; the agent reads it under its own lock.`

`internal/ui/repl.go`, in the command switch after `/consult`:

```go
	case "/task":
		if r.Agent.Engine == nil {
			fmt.Println(dim("working memory is off (engine.enabled)"))
			break
		}
		if len(fields) > 1 && fields[1] == "clear" {
			r.Agent.Engine.ClearSession()
			fmt.Println(dim("working memory cleared for this session"))
			break
		}
		fmt.Println(r.Agent.Engine.LedgerText())
	case "/notes":
		if r.Agent.Engine == nil {
			fmt.Println(dim("working memory is off (engine.enabled)"))
			break
		}
		sub := ""
		if len(fields) > 1 {
			sub = fields[1]
		}
		switch sub {
		case "add":
			text := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(line, "/notes"), " add"))
			if text == "" {
				fmt.Println("usage: /notes add <text>")
				break
			}
			r.Agent.Engine.AddNoteLine(text)
			fmt.Println(dim("noted"))
		case "drop":
			n, _ := strconv.Atoi(strings.Join(fields[2:], ""))
			if err := r.Agent.Engine.DropNote(n); err != nil {
				fmt.Printf("%s %v\n", red("error>"), err)
			}
		case "clear":
			r.Agent.Engine.ClearNotes()
			fmt.Println(dim("notes cleared"))
		default:
			notes := strings.TrimRight(r.Agent.Engine.Notes(), "\n")
			if notes == "" {
				fmt.Println(dim("no notes"))
				break
			}
			for i, l := range strings.Split(notes, "\n") {
				fmt.Printf("%d. %s\n", i+1, l)
			}
		}
```

(`line` is the full command line and `fields` its words — match the names the switch already uses.)

- [ ] **Step 5: TUI**

`internal/tui/view.go`, in `slashCommand` after `/coworkers`, mirroring the plain output through `renderLocalNote`/`renderLocalLines`:

```go
	case "/task":
		if m.ag.Engine == nil {
			m.renderLocalNote("working memory is off (engine.enabled)")
			return m, nil
		}
		if len(fields) > 1 && fields[1] == "clear" {
			m.ag.Engine.ClearSession()
			m.renderLocalNote("working memory cleared for this session")
			return m, nil
		}
		m.renderLocalLines(strings.Split(m.ag.Engine.LedgerText(), "\n"))
		return m, nil
	case "/notes":
		if m.ag.Engine == nil {
			m.renderLocalNote("working memory is off (engine.enabled)")
			return m, nil
		}
		sub := ""
		if len(fields) > 1 {
			sub = fields[1]
		}
		switch sub {
		case "add":
			text := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(text, "/notes"), " add"))
			if text == "" {
				m.renderLocalNote("usage: /notes add <text>")
				return m, nil
			}
			m.ag.Engine.AddNoteLine(text)
			m.renderLocalNote("noted")
		case "drop":
			n, _ := strconv.Atoi(strings.Join(fields[2:], ""))
			if err := m.ag.Engine.DropNote(n); err != nil {
				m.renderLocalNote("error: " + err.Error())
			}
		case "clear":
			m.ag.Engine.ClearNotes()
			m.renderLocalNote("notes cleared")
		default:
			notes := strings.TrimRight(m.ag.Engine.Notes(), "\n")
			if notes == "" {
				m.renderLocalNote("no notes")
				return m, nil
			}
			var lines []string
			for i, l := range strings.Split(notes, "\n") {
				lines = append(lines, fmt.Sprintf("%d. %s", i+1, l))
			}
			m.renderLocalLines(lines)
		}
		return m, nil
```

(`text` is `slashCommand`'s argument; `fields := strings.Fields(text)` — reuse what the function already computes.)

- [ ] **Step 6: Verify**

Run: `go build ./... && go test -race ./internal/ui/ ./internal/tui/ ./cmd/ && make -f build.mk verify 2>&1 | tail -2`
Expected: ok. Smoke, never with the real HOME: `HOME=/tmp/bch go run . --plain --new -C <the scratchpad's smoke-ws>` with stdin `/task`, `/notes add hello`, `/notes`, `read main.go` (a real model turn against the LAN Ollama; expect the second read of the same file within one turn to end with `already read at turn`), `/task`, `/quit`; then `ls $HOME/.be-code/engine/*/` shows the four files. Record the observation in the commit message.

- [ ] **Step 7: Commit**

```bash
git add cmd internal/ui internal/tui
git commit -m "cmd/ui: open working memory per workspace, register its tools, /task and /notes in both UIs"
```

---

### Task 8: E2E, docs, version

**Files:**
- Modify: `test/e2e/mock_server.py`, `test/e2e/run_e2e.sh`, `README.md`, `CHANGELOG.md`, `build.mk`, `docs/live-checklist.md`, root `CLAUDE.md` (outside the repo: edit, not committed)

- [ ] **Step 1: E2E scenario**

`mock_server.py`: add a third scenario keyed by the task text marker `engine scenario`, with its own counter, routed **after** the co-worker model check and before the primary's scripts:

```python
ENG = {"n": 0}
def engine_chunks(body):
    n = ENG["n"]; ENG["n"] += 1
    sys_prompt = body["messages"][0]["content"] or ""
    if sys_prompt.startswith("Summarize this coding-agent"):
        return [text_chunk("Task: engine scenario. Read main.go.\n\nfiles:\n- main.go — has main\n")]
    if n < 3:  # three reads: compaction needs six messages in history
        return [tool_call_chunk("read_file", {"path": "main.go"})]
    last = body["messages"][-1]["content"] or ""
    footer = "FOOTER:yes" if "already read at turn" in last else "FOOTER:no"
    wm = "WM:yes" if "Working memory:" in sys_prompt and "main.go (lines" in sys_prompt else "WM:no"
    return [text_chunk(f"{wm} {footer}")]
```

and in `do_POST`, before the existing primary logic: `if any("engine scenario" in (m.get("content") or "") for m in body["messages"] if m["role"] == "user"): chunks = engine_chunks(body)`.

`run_e2e.sh`: a third run with a config whose `context_tokens` is small enough that the three reads force a compaction before the fourth call — set `"context_tokens": 900` in a **separate** config written just for this run (the first two scenarios keep theirs): write `$HOME/.be-code/config.json` again with the same providers plus `"context_tokens":900,"engine":{"enabled":true}`, run `be-code run -y -C "$WS" "engine scenario: read main.go three times" > "$OUT3"`, `cat "$OUT3"`, then:

```sh
grep -q "WM:yes FOOTER:yes" "$OUT3"
echo "[PASS] engine"
```

before `echo "E2E PASS"`. If compaction does not trigger at 900 (the mock's tool result is small), make `main.go` in the workspace 200 lines long (`for i in $(seq 1 200); do echo "// line $i" >> "$WS/main.go"; done` before the run) so one read is around 3 KiB.

Run: `make -f build.mk build && sh test/e2e/run_e2e.sh | tail -4` — expect `[PASS] consult`, `[PASS] engine`, `E2E PASS`.

- [ ] **Step 2: Docs and version**

- `build.mk`: `VERSION := 0.10.0`.
- `CHANGELOG.md`, above v0.9.0:

```markdown
## v0.10.0 — working memory

- **Working memory.** The harness now remembers what the model has read,
  looked up and decided during a task and puts it back in the system prompt
  after every compaction and on resume, so a long task stops re-reading the
  same files. Every `read_file` is digested (symbols, lines seen, a note of
  what mattered, a content hash); a redundant read is answered in full with
  the footer `already read at turn N (unchanged)`; a repeated search whose
  files are unchanged is served from a cache. The `task` tool records a plan,
  marks steps and notes facts (`keep: true` remembers them across sessions);
  `/task` shows the ledger, `/notes` the durable notes. Compaction summaries
  no longer restate what working memory holds. Store: `~/.be-code/engine/`.
- **Git-backed lookups.** `lookup` (git grep, with the enclosing function on
  `symbol: true`), `history` (a function's own log, a range's log, pickaxe,
  blame), `show` (a file at any revision) and `changes` (the delta since the
  task began). Read-only, never prompt, fall back sensibly outside a repo.
  `engine.tools: minimal` keeps only `task` and `lookup` for tight prompts.
```

- `README.md`: new section `## Working memory` after `## Context: repo map, @mentions, compaction` covering: what is kept and where; the `already read` and `cached` footers; the `task` tool (actions, `keep`); `/task`, `/task clear`, `/notes [add|drop|clear]`; the four git tools with one line each; `engine.*` keys in the config reference (`enabled`, `budget`, `notes_cap`, `tools`).
- `docs/live-checklist.md`: item 16 — with two terminals attached, ask the model to read a file twice in one request; both terminals show the second result ending in `already read at turn N (unchanged)`; `/task` on either terminal lists the seeded task line; `/notes add x` on one is visible via `/notes` on the other (same store).
- Root `CLAUDE.md`: `### Task engine (working memory)` under Architecture after "Backend resilience": the store and its four files, `Observe` in `dispatch` with the footers and the lookup cache, the `Working memory` block in `composeSystem` (after the repo map, budget `engine.budget`, outline suppression via `inRepoMap`), `Compact`'s three changes, `task`/`lookup`/`history`/`show`/`changes` and the `minimal` set, `attachEngine` in `cmd/engine.go` after the session id is known, the advisory failure rule; and the version mentions in "Repository layout" to 0.10.0.

- [ ] **Step 3: Verify and commit**

Run: `make -f build.mk verify 2>&1 | tail -2 && sh test/e2e/run_e2e.sh | tail -3 && grep VERSION build.mk`
Expected: ok; `E2E PASS`; `VERSION := 0.10.0`.

```bash
git add -A && git commit -m "docs/e2e: working memory (0.10.0)"
```

---

## Self-review notes

- Spec §1 (store) → Task 1. §2 (auto-capture) → Task 2 (`Observe`, `Cached`), Task 3 (`dispatch`). §3 (re-injection, Compact, Resume) → Tasks 2 (`Render`), 3 (`composeSystem`, `Resume`), 4 (`Compact`). §4 (`task` tool, harness assistance, `/task`, `/notes`) → Tasks 5, 3 (`EnsureTask`, `ExecutePlan`, `Stopped at:`), 7. §5 (git tools, plan subset, `minimal`) → Tasks 6, 5 (subset), 7 (registration). §6 (config) → Task 1. §7 (error handling) → Task 1 (corrupt files), Task 3 (`observe` recover, notice string), Task 6 (git errors), Task 7 (`attachEngine` warning). §8 (testing) → each task; E2E in Task 8. §9 (docs) → Task 8.
- Names carried across tasks: `engine.Store`/`Event`/`Range`/`Baseline`/`OpenAt`/`Open`/`Render`/`Observe`/`Cached`/`HasDigest`/`LedgerText`/`StoppedAt`/`ApplyFileNotes`/`SplitFilesBlock`/`ParsePlanSteps`/`EnsureTask`/`SetPlan`/`SetStep`/`AddNote`/`AddNoteLine`/`DropNote`/`ClearNotes`/`ClearSession`/`Notes`/`Flush`/`NextTurn` (T1, T2) → T3, T4, T5, T7; `tools.TaskLedger`/`NewTask` (T5) → T7; `tools.BaselineFunc`/`NewGitTools` (T6) → T7; `Agent.Engine`/`SetEngine`/`inRepoMap`/`parseArgs`/`observe` (T3) → T4, T7; `gitctx.Head`/`Porcelain` (T3) → T7; `registerEngineTools`/`attachEngine` (T7).
- Two places the implementer must confirm against the code before writing: `argInt`/`argBool` coercion of string and numeric JSON values (Tasks 5 and 6), and the exact variable names `slashCommand`/`command` use for the command line and its fields (Task 7).
