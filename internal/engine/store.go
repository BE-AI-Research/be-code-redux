// Package engine is BE-Code's working memory: what the model has read,
// looked up and decided during a task, kept by the harness and put back in
// front of the model after compaction and on resume. It never edits the
// user's project except in .be-code/, and never imports the agent.
//
// Truth lives in the workspace, as one Markdown document per top-level task
// under <root>/.be-code/tasks/. The dotdir holds only what is not the
// user's to read: the turn counter, the verbatim buffer for the node in
// flight, the baseline, and the hash of each document so an outside edit is
// recognisable. Losing the dotdir costs the buffer, never the tree.
package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/brown-enterprises/be-code/internal/config"
)

const (
	maxLookups      = 20
	defaultNotesCap = 4096
	defaultItemCap  = 4 * 1024
	defaultNodeCap  = 32 * 1024

	// workspaceDir is the engine's folder inside the user's project.
	workspaceDir = ".be-code"

	// unfiledText names the node evidence goes to when nothing is doing.
	// Saying "unfiled" is honest; guessing a step is confidently wrong.
	unfiledText = "unfiled"
)

type Range struct {
	From int `json:"from"`
	To   int `json:"to"`
}

type Baseline struct {
	Head  string `json:"head,omitempty"`
	Dirty string `json:"dirty,omitempty"` // git status --porcelain text as the task began
}

type Hit struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

// Lookup is one cached search/lookup/history call. It is session state, not
// the record: the tree carries what was looked up, this carries what may be
// answered without calling the tool again, and it is never persisted.
type Lookup struct {
	Tool   string            `json:"tool"`
	Query  string            `json:"query"`
	Hits   []Hit             `json:"hits"`
	Hashes map[string]string `json:"hashes"`
	Turn   int               `json:"turn"`
}

// Step and Ledger are the 0.10.0 flat shapes. They survive for two reasons:
// migration reads them off disk, and the 0.10.0 prompt block and command
// surface still render through them until Tasks 4 and 5 replace both.
type Step struct {
	Text   string `json:"text"`
	Status string `json:"status"` // todo | doing | done | skip
}

type Ledger struct {
	Task      string   `json:"task"`
	Steps     []Step   `json:"steps"`
	Decisions []string `json:"decisions"`
	Facts     []string `json:"facts"`
	Baseline  Baseline `json:"baseline"`
	Session   string   `json:"session"`
}

// Digest is one file as the record knows it, flattened out of the tree's
// FileRefs for the 0.10.0 block. Task 4 renders from the tree directly.
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

// state is the dotdir half of the store: everything that is not the user's
// to read, and all of it disposable.
type state struct {
	// Active is the doing node's id, for a human reading state.json. It is
	// never read back: the document's own [>] mark is the truth, because
	// the user may have moved it there by hand.
	Active    string               `json:"active,omitempty"`
	Turn      int                  `json:"turn"`
	Raw       map[string][]RawItem `json:"raw,omitempty"`
	Docs      map[string]string    `json:"docs,omitempty"`
	Session   string               `json:"session,omitempty"`
	Baseline  Baseline             `json:"baseline,omitempty"`
	NotesHash string               `json:"notes_hash,omitempty"`
}

// Store is one workspace's working memory. One mutex guards everything;
// every getter hands back a deep copy, so no caller ever shares storage
// with the live tree.
type Store struct {
	mu   sync.Mutex
	dir  string // ~/.be-code/engine/<key>
	root string // the workspace
	lim  Limits

	tree Tree
	rec  recorder

	// docs maps a document's file name to the sha256 of the content the
	// engine last wrote, so an edit made outside is recognisable.
	docs map[string]string
	// files is the document each root is written to, by root position.
	files []string
	// extra keeps the lines a parse did not recognise, by root position, so
	// a human's own prose survives every rewrite.
	extra [][]string

	notes    string
	baseline Baseline
	session  string
	turn     int

	lookups []Lookup // newest last; in memory only
	cached  map[string]string

	dirty bool
	// mutSeq counts state changes. Flush releases the lock for its file
	// writes, so it compares the seq it snapshotted with the current one
	// before clearing dirty: a change made during those writes is never
	// swallowed by the flush that did not include it.
	mutSeq int64
}

// withDefaults fills the caps a caller left at zero.
func (l Limits) withDefaults() Limits {
	if l.NotesCap <= 0 {
		l.NotesCap = defaultNotesCap
	}
	if l.ItemCap <= 0 {
		l.ItemCap = defaultItemCap
	}
	if l.NodeCap <= 0 {
		l.NodeCap = defaultNodeCap
	}
	return l
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
func Open(root, sessionID string, resumed bool, lim Limits) (*Store, error) {
	base, err := config.Dir()
	if err != nil {
		return nil, err
	}
	return OpenAt(filepath.Join(base, "engine", Key(root)), root, sessionID, resumed, lim)
}

// OpenAt is Open with an explicit directory (tests). The tree comes from the
// workspace documents, which always win over the dotdir: they are the
// user's file and they may have been edited since we wrote them. A session
// id that differs from the stored one on a non-resume start drops the
// verbatim buffers and the active node — not the tree, which is the
// workspace's record and not the session's.
func OpenAt(dir, root, sessionID string, resumed bool, lim Limits) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := newStore(dir, root, sessionID, lim)

	var st state
	loadJSON(filepath.Join(dir, "state.json"), &st)
	if b, err := os.ReadFile(filepath.Join(dir, "notes.md")); err == nil {
		s.notes = string(b)
	}
	s.turn, s.baseline = st.Turn, st.Baseline
	for name, h := range st.Docs {
		s.docs[name] = h
	}
	s.loadDocs()
	if resumed || st.Session == sessionID {
		s.restoreRaw(st.Raw)
	} else {
		s.markDirtyLocked()
	}

	ledger := filepath.Join(dir, "ledger.json")
	if _, err := os.Stat(ledger); err == nil {
		if err := s.migrateLedger(ledger); err != nil {
			aside := dir + ".broken-" + stamp()
			if rerr := os.Rename(dir, aside); rerr != nil {
				return nil, err
			}
			fmt.Fprintf(os.Stderr, "warn: engine: could not migrate the working memory (%v); starting fresh, the old store is at %s\n", err, aside)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, err
			}
			fresh := newStore(dir, root, sessionID, lim)
			fresh.loadDocs()
			return fresh, nil
		}
	}
	return s, nil
}

func newStore(dir, root, sessionID string, lim Limits) *Store {
	s := &Store{
		dir:     dir,
		root:    root,
		lim:     lim.withDefaults(),
		docs:    map[string]string{},
		session: sessionID,
	}
	s.rec = recorder{lim: s.lim, root: root, tree: &s.tree}
	return s
}

// tasksDir is where the documents live inside the user's project.
func (s *Store) tasksDir() string { return filepath.Join(s.root, workspaceDir, "tasks") }

func stamp() string { return time.Now().Format("20060102-150405") }

// loadDocs reads every task document. A document we cannot parse is
// quarantined with its content intact and simply does not contribute a
// task: the alternative — half-parsing and rewriting — would destroy the
// user's work to save our own.
func (s *Store) loadDocs() {
	dir := s.tasksDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".md") || n == "README.md" || strings.Contains(n, ".broken-") {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		tr, extra, perr := ParseDoc(string(b))
		if perr != nil {
			base := strings.TrimSuffix(name, ".md")
			os.Rename(filepath.Join(dir, name), filepath.Join(dir, base+".broken-"+stamp()+".md"))
			delete(s.docs, name)
			s.markDirtyLocked()
			continue
		}
		if len(tr.Roots) == 0 {
			continue
		}
		s.docs[name] = hashBytes(b)
		for _, r := range tr.Roots {
			s.tree.Roots = append(s.tree.Roots, r)
			s.files = append(s.files, name)
			s.extra = append(s.extra, extra)
			extra = nil // only the first root of a document owns its prose
		}
	}
	s.repairRootIDs()
}

// repairRootIDs makes every root's id its position in the tree, noting the
// repair in the document so the user can see the engine disagreed. Only a
// document deleted or reordered by hand can make this fire.
func (s *Store) repairRootIDs() {
	for i, r := range s.tree.Roots {
		want := strconv.Itoa(i + 1)
		if r.ID == want {
			continue
		}
		if r.ID != "" {
			r.Evidence.Notes = append(r.Evidence.Notes,
				NoteRef{Text: fmt.Sprintf("id repaired from %s to %s", r.ID, want)})
		}
		renumber(r, want)
		s.markDirtyLocked()
	}
}

func renumber(n *Node, id string) {
	n.ID = id
	for i, c := range n.Children {
		renumber(c, id+"."+strconv.Itoa(i+1))
	}
}

// restoreRaw puts the verbatim buffers back on the nodes they belong to.
// A buffer whose node is gone (the user deleted the step) is dropped.
func (s *Store) restoreRaw(raw map[string][]RawItem) {
	for id, items := range raw {
		if n := s.tree.Find(id); n != nil {
			n.Evidence.Raw = items
		}
	}
}

// loadJSON decodes path into v; a corrupt file is moved aside with a
// .broken-<stamp> suffix and v is left zero. A missing file is fine.
func loadJSON(path string, v any) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	if err := json.Unmarshal(b, v); err != nil {
		os.Rename(path, path+".broken-"+stamp())
	}
}

// markDirtyLocked records that persisted state changed. Callers hold s.mu.
func (s *Store) markDirtyLocked() {
	s.dirty = true
	s.mutSeq++
}

// tmpSeq makes every temp file name unique, so two flushes of one store
// from different goroutines can never rename each other's file.
var tmpSeq uint64

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), atomic.AddUint64(&tmpSeq, 1))
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func relPath(p string) string { return filepath.ToSlash(filepath.Clean(p)) }

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// foldPath maps a model-supplied path onto the root-relative slash key
// evidence is filed under. The tools accept an absolute path inside the
// workspace, so the engine has to fold one onto the same key a relative
// read would produce, or the same file is recorded twice under two names.
// An absolute path outside the root returns "": it is not the model's work
// to remember. This is the one path resolver; the store and the recorder
// both call it.
func foldPath(root, p string) string {
	if strings.TrimSpace(p) == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		out := relPath(p)
		if out == "." {
			return ""
		}
		return out
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return ""
	}
	rel, err := filepath.Rel(abs, filepath.Clean(p))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	out := relPath(rel)
	if out == "." {
		return ""
	}
	return out
}

// fileState reads a workspace file and returns its hash, size and mtime;
// ok is false when it cannot be read. This is the one file-state reader.
func fileState(root, rel string) (data []byte, hash string, size, mtime int64, ok bool) {
	if rel == "" {
		return nil, "", 0, 0, false
	}
	abs := filepath.Join(root, filepath.FromSlash(rel))
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

// fileSnap is one workspace file as the engine last saw it.
type fileSnap struct {
	data []byte
	hash string
	size int64
	mod  int64
	ok   bool
}

func snapFile(root, rel string) fileSnap {
	data, hash, size, mod, ok := fileState(root, rel)
	return fileSnap{data: data, hash: hash, size: size, mod: mod, ok: ok}
}

func (s *Store) relTo(p string) string { return foldPath(s.root, p) }

func (s *Store) fileState(rel string) (data []byte, hash string, size, mtime int64, ok bool) {
	return fileState(s.root, rel)
}

// Dir is the dotdir store directory.
func (s *Store) Dir() string { return s.dir }

// Flush writes every task document, the dotdir state and the durable notes
// when anything changed. The whole snapshot is taken under the lock; the
// file writes happen with it released, so a slow disk never blocks a tool
// call. dirty is cleared afterwards only if every write succeeded and
// nothing changed in the meantime.
func (s *Store) Flush() error {
	s.mu.Lock()
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	seq := s.mutSeq
	dir, root := s.dir, s.root

	type docWrite struct{ name, prev, body string }
	var writes []docWrite
	docs := map[string]string{}
	for i, r := range s.tree.Roots {
		num := fmt.Sprintf("%03d", i+1)
		name := num + "-" + slug(r.Text) + ".md"
		prev := ""
		if i < len(s.files) {
			prev = s.files[i]
		}
		body := RenderDocWithExtra(num, r.Text, r, s.extraAt(i))
		writes = append(writes, docWrite{name: name, prev: prev, body: body})
		docs[name] = hashBytes([]byte(body))
	}
	stateBytes, err := json.Marshal(s.stateLocked(docs))
	if err != nil {
		s.mu.Unlock()
		return err
	}
	notes := []byte(s.notes)
	stale := map[string]string{}
	for name, h := range s.docs {
		if _, keep := docs[name]; !keep {
			stale[name] = h
		}
	}
	s.mu.Unlock()

	tasks := filepath.Join(root, workspaceDir, "tasks")
	if len(writes) > 0 {
		if err := os.MkdirAll(tasks, 0o755); err != nil {
			return err
		}
		ensureReadme(tasks)
		for _, w := range writes {
			if w.prev != "" && w.prev != w.name {
				os.Rename(filepath.Join(tasks, w.prev), filepath.Join(tasks, w.name))
			}
			if err := writeAtomic(filepath.Join(tasks, w.name), []byte(w.body), 0o644); err != nil {
				return err
			}
		}
		ensureGitignore(root)
	}
	dropStaleDocs(tasks, stale)
	if err := writeAtomic(filepath.Join(dir, "state.json"), stateBytes, 0o600); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, "notes.md"), notes, 0o600); err != nil {
		return err
	}

	s.mu.Lock()
	s.docs = docs
	s.files = s.files[:0]
	for _, w := range writes {
		s.files = append(s.files, w.name)
	}
	if s.mutSeq == seq {
		s.dirty = false
	}
	s.mu.Unlock()
	return nil
}

// dropStaleDocs removes the documents the engine wrote for tasks that no
// longer exist. A file whose content no longer matches the hash we recorded
// has been edited by hand since, so it is left alone: the engine may forget
// its own record, never someone else's writing.
func dropStaleDocs(tasks string, stale map[string]string) {
	for name, want := range stale {
		path := filepath.Join(tasks, name)
		b, err := os.ReadFile(path)
		if err != nil || hashBytes(b) != want {
			continue
		}
		os.Remove(path)
	}
}

func (s *Store) extraAt(i int) []string {
	if i < len(s.extra) {
		return s.extra[i]
	}
	return nil
}

// stateLocked snapshots the dotdir half. Callers hold s.mu.
func (s *Store) stateLocked(docs map[string]string) state {
	st := state{
		Turn:      s.turn,
		Docs:      docs,
		Session:   s.session,
		Baseline:  s.baseline,
		NotesHash: hashBytes([]byte(s.notes)),
	}
	if d := s.tree.Doing(); d != nil {
		st.Active = d.ID
	}
	raw := map[string][]RawItem{}
	s.tree.Walk(func(n *Node, _ int) {
		if len(n.Evidence.Raw) > 0 {
			raw[n.ID] = n.Evidence.Raw
		}
	})
	if len(raw) > 0 {
		st.Raw = raw
	}
	return st
}

// slug turns a task line into a file-name fragment.
func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
			continue
		}
		if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 48 {
		out = strings.Trim(out[:48], "-")
	}
	if out == "" {
		out = "task"
	}
	return out
}

// ensureGitignore adds .be-code/ to the project's .gitignore once, so the
// record stays out of the user's commits unless they choose otherwise —
// deleting the line is how they commit it. It never creates a .gitignore
// in a folder that is not a repository: there would be nothing to hide the
// record from, and a file we invented in someone's directory is rude.
func ensureGitignore(root string) {
	path := filepath.Join(root, ".gitignore")
	mode := os.FileMode(0o644)
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return
		}
		if _, gerr := os.Stat(filepath.Join(root, ".git")); gerr != nil {
			return
		}
		b = nil
	} else if info, serr := os.Stat(path); serr == nil {
		mode = info.Mode().Perm()
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == workspaceDir+"/" {
			return
		}
	}
	out := string(b)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	os.WriteFile(path, []byte(out+workspaceDir+"/\n"), mode)
}

// readmeText documents the format for both readers, human and model,
// because both edit these files.
const readmeText = `# Task records

BE-Code keeps its working memory here: one Markdown document per top-level
task, named ` + "`NNN-<slug>.md`" + `. These are ordinary files. Edit them.

## The format

    # 001 — fix the parser

    - [x] 1. fix the parser
      - [x] 1.1. find the bug
        - files: lexer.go (lines 1–120)
        - cmds: go test ./... — failed
        - error: FAIL: TestLex
      - [>] 1.2. fix and verify
      - [-] 1.3. rewrite the scanner — dropped: not needed after all

Status marks: ` + "`[ ]`" + ` todo, ` + "`[>]`" + ` doing, ` + "`[x]`" + ` done,
` + "`[!]`" + ` blocked, ` + "`[-]`" + ` dropped. A blocked or dropped step
carries its reason after an em dash.

Ids are dotted paths that follow a node's position: ` + "`2.1.3`" + ` is the
third child of the first child of the second task. Indentation is two
spaces per level. Evidence lines sit under their node with the keys
` + "`files:`" + `, ` + "`cmds:`" + `, ` + "`lookups:`" + `, ` + "`note:`" + `,
` + "`decision:`" + ` and ` + "`error:`" + `.

## Editing these files by hand

Safe to change: any node's text, its status mark, its reason, and any note
or decision line. Any line the engine does not recognise — prose, a heading
of your own, a checklist — is preserved exactly and written back untouched.

The engine repairs an id that disagrees with a node's position, and says so
in a note on that node. A document it cannot parse at all is renamed
` + "`NNN-<slug>.broken-<stamp>.md`" + ` with its content intact, and never
overwritten.

Exactly one node is ` + "`[>]`" + ` doing at a time. Deleting a document
deletes that task from the engine's memory.
`

func ensureReadme(dir string) {
	path := filepath.Join(dir, "README.md")
	if _, err := os.Stat(path); err == nil {
		return
	}
	os.WriteFile(path, []byte(readmeText), 0o644)
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

// SetBaseline records where the task began.
func (s *Store) SetBaseline(b Baseline) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.baseline = b
	s.markDirtyLocked()
}

// Baseline is where the current task began.
func (s *Store) Baseline() Baseline {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.baseline
}

// ClearSession forgets the task record — the tree, the verbatim buffers,
// the lookup cache and the baseline — and keeps only the durable notes,
// exactly as 0.10.0 did. It is only ever reached from the user's own
// "/task clear". The next Flush removes the documents this store wrote for
// the tasks that are gone; a document the user has edited since we wrote it
// is left where it is, because forgetting our own record is not licence to
// delete their writing.
func (s *Store) ClearSession() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tree = Tree{}
	s.files = nil
	s.extra = nil
	s.lookups = nil
	s.cached = nil
	s.baseline = Baseline{}
	s.markDirtyLocked()
}

// ---------------------------------------------------------------- the tree

// Plan records a task and its steps and returns the new root's id. A node
// still doing is distilled first, so its evidence is never left raw.
func (s *Store) Plan(text string, steps []string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeDoingLocked()
	n := s.tree.Add("", firstLine(text, 200))
	for _, st := range steps {
		if strings.TrimSpace(st) != "" {
			s.tree.Add(n.ID, st)
		}
	}
	s.markDirtyLocked()
	return n.ID
}

// Add adds a step under parent ("" for a new top-level task).
func (s *Store) Add(parent, text string) (string, error) {
	if strings.TrimSpace(text) == "" {
		return "", fmt.Errorf("a step needs text")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if parent != "" && s.tree.Find(parent) == nil {
		return "", fmt.Errorf("no node %s", parent)
	}
	n := s.tree.Add(parent, text)
	s.markDirtyLocked()
	return n.ID, nil
}

// SetStatus moves one node. Leaving doing, or reaching a terminal status,
// distills that node's verbatim buffer into its durable record.
func (s *Store) SetStatus(id string, status Status, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.tree.Find(id)
	if n == nil {
		return fmt.Errorf("no node %s", id)
	}
	if status == StatusDoing {
		if prev := s.tree.Doing(); prev != nil && prev != n {
			s.rec.distill(prev)
		}
	}
	s.tree.SetStatus(id, status, reason)
	if status.terminal() {
		s.rec.distill(n)
	}
	s.markDirtyLocked()
	return nil
}

// SetStatusText is SetStatus over the wire word, so internal/tools can
// satisfy TaskLedger without importing engine's Status type.
func (s *Store) SetStatusText(id, status, reason string) error {
	st, ok := parseStatus(status)
	if !ok {
		return fmt.Errorf("status must be todo, doing, done, blocked or dropped")
	}
	return s.SetStatus(id, st, reason)
}

func parseStatus(s string) (Status, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "todo", "open", "pending":
		return StatusTodo, true
	case "doing", "progress", "in_progress", "started":
		return StatusDoing, true
	case "done", "finished", "complete", "completed":
		return StatusDone, true
	case "blocked", "stuck":
		return StatusBlocked, true
	case "dropped", "drop", "skip", "skipped", "cancelled", "canceled":
		return StatusDropped, true
	}
	return "", false
}

// Note records a fact or decision against a node. With file it also becomes
// that file's note in the record; with keep it is appended to the durable
// notes as well.
func (s *Store) Note(id, text, file string, decision, keep bool) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("note needs text")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if keep {
		s.addNoteLineLocked(text)
	}
	n := s.nodeForLocked(id)
	if n == nil {
		if keep {
			s.markDirtyLocked()
			return nil
		}
		return fmt.Errorf("no node %s", id)
	}
	rel := s.relTo(file)
	n.Evidence.Notes = append(n.Evidence.Notes, NoteRef{Text: text, File: rel, Decision: decision})
	if rel != "" {
		ref := fileRefFor(n, rel)
		ref.Note = text
		if ref.Turn == 0 {
			ref.Turn = s.turn
		}
	}
	s.markDirtyLocked()
	return nil
}

// EnsureRoot opens a task from the user's message when nothing is doing and
// no task is open, so evidence always has a home. It returns the id of the
// task that is now active.
func (s *Store) EnsureRoot(text string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.activeRootLocked(); r != nil {
		return r.ID
	}
	n := s.tree.Add("", firstLine(text, 200))
	s.markDirtyLocked()
	return n.ID
}

// Tree is a deep copy of the whole record.
func (s *Store) Tree() Tree {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Tree{Roots: copyNodes(s.tree.Roots)}
}

// ShowText renders one node and everything under it, or the whole tree when
// id is empty.
func (s *Store) ShowText(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	if id == "" {
		if len(s.tree.Roots) == 0 {
			return "no tasks recorded"
		}
		for _, r := range s.tree.Roots {
			renderNode(&b, r, 0)
		}
		return strings.TrimRight(b.String(), "\n")
	}
	n := s.tree.Find(id)
	if n == nil {
		return "no node " + id
	}
	renderNode(&b, n, 0)
	return strings.TrimRight(b.String(), "\n")
}

// activeRootLocked is the task in flight: the last root that is not wholly
// finished. Callers hold s.mu.
func (s *Store) activeRootLocked() *Node {
	for i := len(s.tree.Roots) - 1; i >= 0; i-- {
		if !s.tree.Terminal(s.tree.Roots[i]) {
			return s.tree.Roots[i]
		}
	}
	return nil
}

// activeNodeLocked is where evidence goes: the node that is doing, or —
// when nothing is — an "unfiled" node under the active task. Filing
// evidence under a step the model did not name produces a report that is
// confidently wrong, which is worse than one that says unfiled.
func (s *Store) activeNodeLocked() *Node {
	if d := s.tree.Doing(); d != nil {
		return d
	}
	host := s.activeRootLocked()
	if host == nil {
		n := s.tree.Add("", unfiledText)
		n.Status = StatusDoing
		s.markDirtyLocked()
		return n
	}
	for _, c := range host.Children {
		if c.Text == unfiledText && !c.Status.terminal() {
			c.Status = StatusDoing
			s.markDirtyLocked()
			return c
		}
	}
	n := s.tree.Add(host.ID, unfiledText)
	n.Status = StatusDoing
	s.markDirtyLocked()
	return n
}

func (s *Store) nodeForLocked(id string) *Node {
	if id == "" {
		return s.activeNodeLocked()
	}
	return s.tree.Find(id)
}

func (s *Store) closeDoingLocked() {
	if prev := s.tree.Doing(); prev != nil {
		s.rec.distill(prev)
		prev.Status = StatusTodo
	}
}

// fileRefFor finds or creates n's record of one file.
func fileRefFor(n *Node, rel string) *FileRef {
	for i := range n.Evidence.Files {
		if n.Evidence.Files[i].Path == rel {
			return &n.Evidence.Files[i]
		}
	}
	n.Evidence.Files = append(n.Evidence.Files, FileRef{Path: rel})
	return &n.Evidence.Files[len(n.Evidence.Files)-1]
}

func copyNodes(ns []*Node) []*Node {
	if len(ns) == 0 {
		return nil
	}
	out := make([]*Node, 0, len(ns))
	for _, n := range ns {
		cp := *n
		cp.Evidence = copyEvidence(n.Evidence)
		cp.Children = copyNodes(n.Children)
		out = append(out, &cp)
	}
	return out
}

func copyEvidence(e Evidence) Evidence {
	out := e
	out.Files = nil
	for _, f := range e.Files {
		cf := f
		cf.Ranges = append([]Range(nil), f.Ranges...)
		cf.Outline = append([]string(nil), f.Outline...)
		out.Files = append(out.Files, cf)
	}
	out.Cmds = append([]CmdRef(nil), e.Cmds...)
	out.Lookups = nil
	for _, l := range e.Lookups {
		cl := l
		cl.Hits = append([]Hit(nil), l.Hits...)
		out.Lookups = append(out.Lookups, cl)
	}
	out.Notes = append([]NoteRef(nil), e.Notes...)
	out.Errors = append([]string(nil), e.Errors...)
	out.Raw = append([]RawItem(nil), e.Raw...)
	return out
}

// firstLine is a text's first line, bounded — never cut mid-rune, since
// this ends up in a file the user reads.
func firstLine(text string, max int) string {
	line := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	if max > 0 && len(line) > max {
		line = line[:max]
		for len(line) > 0 && !utf8.ValidString(line) {
			line = line[:len(line)-1]
		}
	}
	return line
}

// --------------------------------------------------------- the recorder seam

// Observe records what a tool result revealed and returns a footer for the
// model when the read was redundant. Never errors; the file I/O it needs is
// done before the lock is taken, the way 0.10.0 did it.
func (s *Store) Observe(ev Event) string {
	if ev.Tool == "" {
		return ""
	}
	snap := snapFor(s.root, ev)
	var hits []Hit
	var hashes map[string]string
	if !ev.IsError && isLookup(ev.Tool) {
		hits = parseHits(ev.Content)
		hashes = map[string]string{}
		for _, h := range hits {
			if _, ok := hashes[h.File]; ok {
				continue
			}
			if sn := snapFile(s.root, h.File); sn.ok {
				hashes[h.File] = sn.hash
			}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !recorded(ev.Tool) {
		return ""
	}
	n := s.activeNodeLocked()
	if n == nil {
		return ""
	}
	footer := s.rec.recordWith(n, ev, s.turn, snap)
	if !ev.IsError && isLookup(ev.Tool) {
		s.observeLookupLocked(ev, hits, hashes)
	}
	s.markDirtyLocked()
	return footer
}

// recorded is the set of tools whose results the record keeps — the same
// set distillation knows how to turn into evidence. Anything else, the
// task tool's own bookkeeping above all, is not worth a node's byte cap,
// and must never be what opens an unfiled node.
func recorded(tool string) bool {
	switch tool {
	case "read_file", "write_file", "edit_file", "shell", "process", "search", "lookup", "history":
		return true
	}
	return false
}

func isLookup(tool string) bool {
	return tool == "search" || tool == "lookup" || tool == "history"
}

// snapFor reads the file an event names, when it names one.
func snapFor(root string, ev Event) fileSnap {
	switch ev.Tool {
	case "read_file", "write_file", "edit_file":
		if rel := foldPath(root, argStr(ev.Args, "path", "file", "filename")); rel != "" {
			return snapFile(root, rel)
		}
	}
	return fileSnap{}
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

func (s *Store) observeLookupLocked(ev Event, hits []Hit, hashes map[string]string) {
	key := LookupKey(ev.Tool, ev.Args)
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
	if len(hits) > 0 {
		// A zero-hit result has no file to invalidate it against, so
		// caching it would serve "no matches" forever, even after the model
		// creates a file that would now match. Never cache it, and drop any
		// earlier (non-empty) cache entry for the same key.
		if s.cached == nil {
			s.cached = map[string]string{}
		}
		s.cached[key] = ev.Content
	} else {
		delete(s.cached, key)
	}
}

// Cached answers a repeated lookup whose hit files are unchanged.
func (s *Store) Cached(tool string, args map[string]any) (string, bool) {
	key := LookupKey(tool, args)
	s.mu.Lock()
	var lk Lookup
	found := false
	for i := range s.lookups {
		if s.lookups[i].Query == key {
			lk = s.lookups[i] // value copy: observeLookup may replace this slot
			found = true
		}
	}
	content, have := s.cached[key]
	s.mu.Unlock()
	if !found || !have {
		return "", false
	}
	for file, hash := range lk.Hashes {
		if _, now, _, _, ok := s.fileState(file); !ok || now != hash {
			return "", false
		}
	}
	return strings.TrimRight(content, "\n") + "\n" + footerCached, true
}

// DigestKey is the key a path is recorded under: root-relative and
// slash-separated, with an absolute path inside the workspace folded onto
// the same key a relative read would produce. "" for a path outside the
// root, which the store never records.
func (s *Store) DigestKey(path string) string { return s.relTo(path) }

// HasDigest reports the union of ranges the record holds for a path.
func (s *Store) HasDigest(path string) (Range, bool) {
	key := s.DigestKey(path)
	if key == "" {
		return Range{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out Range
	ok := false
	s.tree.Walk(func(n *Node, _ int) {
		for _, f := range n.Evidence.Files {
			if f.Path != key || len(f.Ranges) == 0 {
				continue
			}
			for _, r := range f.Ranges {
				if !ok || r.From < out.From {
					out.From = r.From
				}
				if !ok || r.To > out.To {
					out.To = r.To
				}
				ok = true
			}
		}
	})
	return out, ok
}

// ------------------------------------------------------------ durable notes

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
	for len(s.notes) > s.lim.NotesCap {
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
