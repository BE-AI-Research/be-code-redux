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

	// seq breaks ties between digests touched in the same model turn (Turn
	// only advances once per model call, not per tool call); it is
	// in-memory only, reset each process start, and never persisted.
	seq int64
}

type Step struct {
	Text   string `json:"text"`
	Status string `json:"status"` // todo | doing | done | skip
}

type Baseline struct {
	Head  string `json:"head,omitempty"`
	Dirty string `json:"dirty,omitempty"` // git status --porcelain text as the task began
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
	touchSeq int64
	dirty    bool
	// mutSeq counts state changes. Flush releases the lock for its file
	// writes, so it compares the seq it snapshotted with the current one
	// before clearing dirty: a change made during those writes is never
	// swallowed by the flush that did not include it.
	mutSeq int64

	// cached is filled in by Task 2 (rendering the Working memory block);
	// unused here.
	cached map[string]string
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
		s.markDirtyLocked()
	}
	s.ledger.Session = sessionID
	for i := range digests {
		d := digests[i]
		s.digests[d.Path] = &d
		if d.Turn > s.turn {
			s.turn = d.Turn
		}
	}
	for _, lk := range s.lookups {
		if lk.Turn > s.turn {
			s.turn = lk.Turn
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

// markDirtyLocked records that persisted state changed. Callers hold s.mu.
func (s *Store) markDirtyLocked() {
	s.dirty = true
	s.mutSeq++
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

// Flush writes the session files and notes when anything changed. The
// whole snapshot — including the eviction that brings the digests under
// the byte cap — is taken under the lock; the four file writes happen with
// the lock released, so a slow disk never blocks a tool call. dirty is
// cleared afterwards only if every write succeeded and nothing changed in
// the meantime.
func (s *Store) Flush() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	seq := s.mutSeq
	var db []byte
	digests := s.digestsLocked()
	for {
		b, err := json.Marshal(digests)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		if len(b) <= maxDigestBytes || len(digests) == 0 {
			db = b
			break
		}
		// Over the byte cap: drop the least recently touched (last).
		delete(s.digests, digests[len(digests)-1].Path)
		digests = digests[:len(digests)-1]
	}
	lb, _ := json.Marshal(s.ledger)
	kb, _ := json.Marshal(s.lookups)
	nb := []byte(s.notes)
	dir := s.dir
	s.mu.Unlock()

	for _, f := range []struct {
		name string
		data []byte
	}{
		{"digests.json", db},
		{"ledger.json", lb},
		{"lookups.json", kb},
		{"notes.md", nb},
	} {
		if err := writeAtomic(filepath.Join(dir, f.name), f.data); err != nil {
			return err
		}
	}

	s.mu.Lock()
	if s.mutSeq == seq {
		s.dirty = false
	}
	s.mu.Unlock()
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

// digestsLocked returns the digests most recently touched first. Ranges and
// Outline are copied so a caller's snapshot never shares storage with the
// live digest (mergeRange appends to the live digest's Ranges in place).
func (s *Store) digestsLocked() []Digest {
	out := make([]Digest, 0, len(s.digests))
	for _, d := range s.digests {
		cp := *d
		if len(d.Ranges) > 0 {
			cp.Ranges = append([]Range(nil), d.Ranges...)
		}
		if len(d.Outline) > 0 {
			cp.Outline = append([]string(nil), d.Outline...)
		}
		out = append(out, cp)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Turn != out[j].Turn {
			return out[i].Turn > out[j].Turn
		}
		if out[i].seq != out[j].seq {
			return out[i].seq > out[j].seq
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

// touchLocked bumps d's in-turn touch order so a digest touched later in
// the same model turn (Turn only advances once per model call, not once
// per tool call) still sorts before one touched earlier at the same Turn.
// Callers hold s.mu.
func (s *Store) touchLocked(d *Digest) {
	s.touchSeq++
	d.seq = s.touchSeq
}

// putDigest stores d, evicting the least recently touched past the cap.
func (s *Store) putDigest(d *Digest) {
	s.digests[d.Path] = d
	s.touchLocked(d)
	s.markDirtyLocked()
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
	return s.ledgerLocked()
}

// ledgerLocked is Ledger's deep copy for callers that already hold s.mu:
// the slices are copied so no snapshot shares storage with the live
// ledger, which grows in place as notes and steps are recorded.
func (s *Store) ledgerLocked() Ledger {
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
	s.markDirtyLocked()
}

// EnsureTask sets the task line only when the ledger has none.
func (s *Store) EnsureTask(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ledger.Task != "" {
		return
	}
	s.setTaskLocked(text)
}

// StartTask refreshes the task line for a new request: always when the
// ledger is empty, and otherwise only when no step is in progress and none
// is still to do. A plan the model is part-way through keeps its own task
// line, so the block does not start describing a side question as the task.
func (s *Store) StartTask(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.ledger.Steps {
		if st.Status == "doing" || st.Status == "todo" {
			return
		}
	}
	s.setTaskLocked(text)
}

// setTaskLocked stores text's first line, bounded, as the task.
func (s *Store) setTaskLocked(text string) {
	line := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	if len(line) > 200 {
		line = line[:200]
	}
	s.ledger.Task = line
	s.markDirtyLocked()
}

// SetStep marks 1-based step i with a status (already normalised by the tool).
func (s *Store) SetStep(i int, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i < 1 || i > len(s.ledger.Steps) {
		return fmt.Errorf("step %d does not exist (%d steps)", i, len(s.ledger.Steps))
	}
	s.ledger.Steps[i-1].Status = status
	s.markDirtyLocked()
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
		path := filepath.ToSlash(filepath.Clean(file))
		d, ok := s.digests[path]
		if !ok {
			d = &Digest{Path: path}
		}
		d.Note = text
		d.Turn = s.turn
		if ok {
			s.touchLocked(d)
		} else {
			// New digest: route through putDigest so the count cap applies.
			s.putDigest(d)
		}
	}
	if keep {
		s.addNoteLineLocked(text)
	}
	s.markDirtyLocked()
	return nil
}

// SetBaseline records where the task began.
func (s *Store) SetBaseline(b Baseline) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ledger.Baseline = b
	s.markDirtyLocked()
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
	s.markDirtyLocked()
}

func (s *Store) addNoteLineLocked(text string) {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	if text == "" {
		return
	}
	for _, line := range strings.Split(s.notes, "\n") {
		if line == text {
			return
		}
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
	s.markDirtyLocked()
	return nil
}

// ClearNotes empties the durable notes.
func (s *Store) ClearNotes() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notes = ""
	s.markDirtyLocked()
}

// ClearSession drops digests, lookups and the ledger (never the notes).
func (s *Store) ClearSession() {
	s.mu.Lock()
	defer s.mu.Unlock()
	session := s.ledger.Session
	s.digests = map[string]*Digest{}
	s.lookups = nil
	s.ledger = Ledger{Session: session}
	// The in-memory lookup cache is keyed to the lookups just dropped:
	// leaving it would answer a repeated search from a session the store
	// no longer remembers having made.
	s.cached = nil
	s.markDirtyLocked()
}
