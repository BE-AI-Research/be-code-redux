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
	"errors"
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
	// defaultStepNudge matches the prompt's "about ten tool calls" a step
	// with room to spare: twice that is a step that was not small.
	defaultStepNudge = 20
	// maxFileMemos caps the per-file hashes and outlines state.json keeps.
	maxFileMemos = 200

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

// Step and Ledger are the 0.10.0 flat shapes, kept only because migration
// reads them off disk (migrate.go) to lift a pre-0.11.0 store's ledger.json
// into the tree. Nothing else writes or reads them.
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

// Digest is one file as a 0.10.0 store recorded it. Only migration reads
// it now; the tree's own FileRef is what the record uses.
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
	// Active is the doing node's id as the engine last wrote it. The
	// document's own [>] mark is the truth about what is doing, because the
	// user may have moved it by hand; this is read back for one thing only —
	// a fresh session uses it to tell the engine's own mark, which it takes
	// back, from one the user placed, which it leaves (startFresh).
	Active   string               `json:"active,omitempty"`
	Turn     int                  `json:"turn"`
	Raw      map[string][]RawItem `json:"raw,omitempty"`
	Docs     map[string]string    `json:"docs,omitempty"`
	Session  string               `json:"session,omitempty"`
	Baseline Baseline             `json:"baseline,omitempty"`
	// Files is what a Markdown document has no business carrying: the
	// content hash and the outline of each file the record names. Without
	// it the cross-session redundant-read check cannot fire at all (it
	// refuses to answer without a hash to compare) and every resume loses
	// its outlines — ruling T3-b.
	Files map[string]fileMemo `json:"files,omitempty"`
	// Times is when each node was started and closed and how many tool calls
	// it took — facts a Markdown document has no place for. Keyed by id and
	// checked against the node's text on the way back, because ids are
	// positional and the user may have edited the document in between.
	Times map[string]nodeTimes `json:"times,omitempty"`
}

type nodeTimes struct {
	Text    string    `json:"text"`
	Started time.Time `json:"started,omitempty"`
	Closed  time.Time `json:"closed,omitempty"`
	Calls   int       `json:"calls,omitempty"`
}

type fileMemo struct {
	Hash    string   `json:"hash,omitempty"`
	Outline []string `json:"outline,omitempty"`
	Turn    int      `json:"turn,omitempty"`
	// Node and Ranges together identify the reference this hash belongs to.
	// A hash says "these ranges describe this content", so handing it to any
	// other reference would make ranges measured against text since edited
	// away read as current — the false "already read" ruling T2-b exists to
	// prevent. An id alone is not enough: ids are positional, so a step
	// deleted by hand shifts them and a stale reference can land on the
	// owner's id. The ranges have to match too.
	Node   string  `json:"node,omitempty"`
	Ranges []Range `json:"ranges,omitempty"`
}

// sameRanges reports whether two range lists are identical. Both are kept
// merged and sorted, so equality is elementwise.
func sameRanges(a, b []Range) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Store is one workspace's working memory. One mutex guards everything;
// every getter hands back a deep copy, so no caller ever shares storage
// with the live tree.
type Store struct {
	// flushMu serialises whole flushes and reloads. Flush releases mu for its
	// file writes, so without this two concurrent flushes could finish in
	// either order and leave the older snapshot on disk. It is always taken
	// before mu, never while holding it.
	flushMu sync.Mutex

	mu   sync.Mutex
	dir  string // ~/.be-code/engine/<key>
	root string // the workspace
	lim  Limits

	tree Tree
	rec  recorder

	// docs maps a document's file name to the sha256 of the content the
	// engine last saw on disk — loaded, written or merged — so an edit made
	// outside is recognisable. base is that same content as a tree: the
	// common ancestor merge.go needs to tell the user's edit from the
	// engine's own change.
	docs map[string]string
	base map[string]*Node
	// files is the document each root is written to, by root position.
	files []string
	// extra keeps the lines a parse did not recognise, by root position, so
	// a human's own prose survives every rewrite.
	extra [][]string

	// reserved are document names that exist and belong to the user but
	// carry no task — one they emptied of bullets, keeping their prose.
	reserved map[string]bool

	notes    string
	baseline Baseline
	session  string
	turn     int

	lookups []Lookup // newest last; in memory only
	cached  map[string]string

	// noWorkspace is set when .be-code or .be-code/tasks is a symbolic link:
	// the engine then reads and writes nothing under the workspace at all,
	// since every path it would touch resolves somewhere it was never given.
	noWorkspace bool
	// freshRoot is set on a session that is not a continuation of the one
	// that wrote the state: its first request opens a task of its own rather
	// than carrying on inside whatever the last session left open.
	freshRoot bool

	// warnings are things the human must hear that the engine has no way to
	// say itself: it has no UI, and its stderr is a log file in a hosted
	// session. Whoever owns the store drains them (TakeWarnings).
	warnMu   sync.Mutex
	warnings []string

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
	if l.StepNudge == 0 {
		l.StepNudge = defaultStepNudge
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
// verbatim buffers and the active node (startFresh) — not the tree, which is
// the workspace's record and not the session's.
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
	s.checkWorkspace()
	s.loadDocs()
	s.restoreFileMemos(st.Files)
	s.restoreTimes(st.Times)
	if resumed || st.Session == sessionID {
		s.restoreRaw(st.Raw)
	} else {
		s.startFresh(st.Active)
	}

	// A ledger.json present is the whole test for "not migrated yet": the
	// migration renames it aside rather than deleting it, so this is a fact
	// about the filesystem and not a flag that could lag behind the tree.
	ledger := filepath.Join(dir, "ledger.json")
	if _, err := os.Stat(ledger); err == nil {
		if err := s.migrateLedger(ledger); err != nil {
			var unsaved unsavedStateError
			if errors.As(err, &unsaved) {
				// Ruling T3-f: the documents are written and the 0.10.0
				// files stay aside, so the lift is on disk and the next
				// open will neither duplicate it nor repeat it. Only the
				// dotdir state was lost, which this session can rewrite.
				s.warnf("the working memory was migrated but its state could not be saved (%v); the task documents are written and the 0.10.0 files are set aside", unsaved.err)
				return s, nil
			}
			var retry retryableError
			if errors.As(err, &retry) {
				// The store was put back exactly as it was, so there is
				// nothing half-converted to rename aside and the next open
				// simply tries again.
				s.warnf("could not migrate the working memory (%v); the 0.10.0 store is untouched and will be tried again", retry.err)
				return s, nil
			}
			aside := dir + ".broken-" + stamp()
			if rerr := os.Rename(dir, aside); rerr != nil {
				return nil, err
			}
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return nil, err
			}
			fresh := newStore(dir, root, sessionID, lim)
			fresh.warnf("could not migrate the working memory (%v); starting fresh, the old store is at %s", err, aside)
			fresh.checkWorkspace()
			fresh.loadDocs()
			return fresh, nil
		}
	}
	return s, nil
}

// startFresh is what a session that continues nothing does to the tree it
// inherits: the step the last session left doing goes back to todo, and the
// first request opens a task of its own. Without it a fresh session filed
// its evidence under, and retitled, whatever task the previous one happened
// to leave open — a different piece of work entirely. The old task is not
// closed: it is unfinished, and saying otherwise would be a guess.
//
// Only the engine's *own* mark is taken back: active is the doing node the
// last flush recorded in state.json. A [>] the user has since moved to
// another step by hand is their intent (spec §5.2) — it stays, and the
// session carries on there instead of opening a task of its own.
func (s *Store) startFresh(active string) {
	if n := s.tree.Find(active); active != "" && n != nil && n.Status == StatusDoing {
		n.Status = StatusTodo
	}
	s.freshRoot = s.tree.Doing() == nil
	s.markDirtyLocked()
}

// checkWorkspace refuses a symlinked .be-code or .be-code/tasks. Every path
// the engine writes, rewrites or renames is built under those two names, so a
// link there sends unapproved writes wherever it points — outside the
// workspace the user confined the session to. Lstat, not Stat: the question
// is what the name is, not what it leads to.
func (s *Store) checkWorkspace() {
	for _, p := range []string{filepath.Join(s.root, workspaceDir), s.tasksDir()} {
		if info, err := os.Lstat(p); err == nil && info.Mode()&os.ModeSymlink != 0 {
			s.noWorkspace = true
			s.warnf("%s is a symbolic link; the task record will not be read from or written to it, and working memory lasts only for this session", p)
			return
		}
	}
}

// warnf records something the human has to be told.
func (s *Store) warnf(format string, args ...any) {
	s.warnMu.Lock()
	defer s.warnMu.Unlock()
	if len(s.warnings) < 64 {
		s.warnings = append(s.warnings, fmt.Sprintf(format, args...))
	}
}

// TakeWarnings returns what the engine needs the human to hear and forgets
// it. The engine cannot say any of it itself: in a hosted session its stderr
// is a log file, and under a TUI it is wiped by the alternate screen — which
// is where a quarantined document's warning used to go. The agent drains
// this into its notice path; cmd drains it at startup.
func (s *Store) TakeWarnings() []string {
	s.warnMu.Lock()
	defer s.warnMu.Unlock()
	out := s.warnings
	s.warnings = nil
	return out
}

func newStore(dir, root, sessionID string, lim Limits) *Store {
	s := &Store{
		dir:      dir,
		root:     root,
		lim:      lim.withDefaults(),
		docs:     map[string]string{},
		base:     map[string]*Node{},
		reserved: map[string]bool{},
		session:  sessionID,
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
	if s.noWorkspace {
		return
	}
	dir := s.tasksDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var names []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !isTaskDoc(n) {
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
			aside := strings.TrimSuffix(name, ".md") + ".broken-" + stamp() + ".md"
			os.Rename(filepath.Join(dir, name), filepath.Join(dir, aside))
			// Losing a task must never be silent: the model will not
			// mention what it cannot see, so the user has to hear it here.
			s.warnf("%s could not be read (%v); moved aside as %s and its task is not loaded", name, perr, aside)
			delete(s.docs, name)
			s.markDirtyLocked()
			continue
		}
		if len(tr.Roots) == 0 {
			// A document the user has emptied of bullets, keeping their own
			// prose. It contributes no task, but the name is still theirs:
			// reserving it stops the next task with the same slug being
			// written straight over their file.
			s.reserved[name] = true
			continue
		}
		s.docs[name] = hashBytes(b)
		// base is what the disk holds, before any id repair: it is compared
		// by text and by evidence line, never by root id.
		s.base[name] = stripRaw(copyNodes(tr.Roots[:1]))[0]
		for _, r := range tr.Roots {
			s.tree.Roots = append(s.tree.Roots, r)
			s.files = append(s.files, name)
			s.extra = append(s.extra, extra)
			extra = nil // only the first root of a document owns its prose
		}
	}
	s.dropSplitDuplicates()
	s.repairRootIDs()
	s.warnMultipleDoing()
}

// dropSplitDuplicates undoes the one thing a failure inside Flush's split can
// leave behind. A task added by hand to another task's document is written to
// a file of its own *before* its source document is rewritten without it
// (ruling T3-g), because the other order loses the task outright; a process
// that dies between the two leaves the task in both files, and loading both
// would show it twice. The copy still inside the source document is dropped
// here — only when it is the same in every respect, so two tasks someone
// really did give the same title are never folded together — and the store
// is marked dirty so the source document is finally rewritten without it.
func (s *Store) dropSplitDuplicates() {
	firstOf := map[string]int{}
	for i, name := range s.files {
		if _, ok := firstOf[name]; !ok {
			firstOf[name] = i
		}
	}
	for i := len(s.tree.Roots) - 1; i >= 0; i-- {
		if firstOf[s.files[i]] == i {
			continue // the root its document is named for
		}
		for j, other := range s.tree.Roots {
			if j == i || s.files[j] == s.files[i] || firstOf[s.files[j]] != j {
				continue
			}
			if sameSubtree(s.tree.Roots[i], other) {
				s.tree.Roots = append(s.tree.Roots[:i], s.tree.Roots[i+1:]...)
				s.files = append(s.files[:i], s.files[i+1:]...)
				s.extra = append(s.extra[:i], s.extra[i+1:]...)
				s.markDirtyLocked()
				break
			}
		}
	}
}

// sameSubtree reports whether two nodes record the same thing: text, status,
// reason and evidence as a document prints them, all the way down. Ids are
// not compared — they are positional, and the two copies sit in different
// places.
func sameSubtree(a, b *Node) bool {
	if a.Text != b.Text || a.Status != b.Status || a.Reason != b.Reason || len(a.Children) != len(b.Children) {
		return false
	}
	la, lb := evidenceLines(a.Evidence), evidenceLines(b.Evidence)
	if len(la) != len(lb) {
		return false
	}
	for i := range la {
		if la[i] != lb[i] {
			return false
		}
	}
	for i := range a.Children {
		if !sameSubtree(a.Children[i], b.Children[i]) {
			return false
		}
	}
	return true
}

// warnMultipleDoing tells the human, once per open, when more than one step
// across the loaded documents is marked doing. Spec 5.2: a hand-written
// status is the user's own intent, so unlike a repaired id or a quarantined
// document, the engine changes neither document over this — it only says
// so. Id repair gets a note in the file and quarantine gets a stderr line;
// this has no single node to leave a note on (nothing is wrong with either
// node in isolation, only with the pair), so stderr is the only place left
// to say it.
func (s *Store) warnMultipleDoing() {
	var docs []string
	seen := map[string]bool{}
	total := 0
	var walk func(n *Node, doc string)
	walk = func(n *Node, doc string) {
		if n.Status == StatusDoing {
			total++
			if !seen[doc] {
				seen[doc] = true
				docs = append(docs, doc)
			}
		}
		for _, c := range n.Children {
			walk(c, doc)
		}
	}
	for i, r := range s.tree.Roots {
		doc := ""
		if i < len(s.files) {
			doc = s.files[i]
		}
		walk(r, doc)
	}
	if total < 2 {
		return
	}
	s.warnf("%d steps are marked doing in %s; keep exactly one — a hand-edited status is left exactly as written, so the engine will not fix this, it simply uses whichever it reads last", total, strings.Join(docs, ", "))
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

// restoreFileMemos puts back what the documents do not carry: each file's
// content hash, outline and turn. Only fields the document left empty are
// filled, so a note or a range the user edited by hand still wins.
//
// The hash goes back only to the node that earned it. Every other node's
// reference to that file keeps an empty hash and therefore never answers a
// read — which is right, because its ranges were measured against content
// that has since moved on. The outline and the turn are about the file
// rather than about what any one node saw, so they are shared.
func (s *Store) restoreFileMemos(memos map[string]fileMemo) {
	if len(memos) == 0 {
		return
	}
	s.tree.Walk(func(n *Node, _ int) {
		for i := range n.Evidence.Files {
			f := &n.Evidence.Files[i]
			m, ok := memos[f.Path]
			if !ok {
				continue
			}
			if f.Hash == "" && m.Node == n.ID && sameRanges(f.Ranges, m.Ranges) {
				f.Hash = m.Hash
			}
			if len(f.Outline) == 0 {
				f.Outline = append([]string(nil), m.Outline...)
			}
			if f.Turn == 0 {
				f.Turn = m.Turn
			}
		}
	})
}

// fileMemosLocked is what stateLocked persists: one entry per file the
// record names, newest first and capped, since an outline per file across
// a long-lived workspace is the one part of state.json that grows.
func (s *Store) fileMemosLocked() map[string]fileMemo {
	type entry struct {
		path string
		memo fileMemo
	}
	var all []entry
	seen := map[string]int{}
	s.tree.Walk(func(n *Node, _ int) {
		for _, f := range n.Evidence.Files {
			if f.Path == "" || (f.Hash == "" && len(f.Outline) == 0) {
				continue
			}
			m := fileMemo{Hash: f.Hash, Outline: f.Outline, Turn: f.Turn, Node: n.ID, Ranges: f.Ranges}
			if at, ok := seen[f.Path]; ok {
				if f.Turn >= all[at].memo.Turn {
					all[at].memo = m
				}
				continue
			}
			seen[f.Path] = len(all)
			all = append(all, entry{f.Path, m})
		}
	})
	sort.SliceStable(all, func(i, j int) bool { return all[i].memo.Turn > all[j].memo.Turn })
	if len(all) > maxFileMemos {
		all = all[:maxFileMemos]
	}
	out := make(map[string]fileMemo, len(all))
	for _, e := range all {
		out[e.path] = e.memo
	}
	return out
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

// flushError is what a failed Flush returns, carrying the one fact a caller
// cannot recover afterwards: which task documents had already reached the
// workspace. The migration needs it (ruling T3-f) — once *its own* document
// is written the lift has happened and the legacy files must stay aside — and
// it has to be the set of names, not a count. A count only identifies a
// document while the documents are written in root order, and they no longer
// are: a root split out of a hand-edited document is written before the
// document it came from (ruling T3-g, corrected), and roots that get no
// document at all are skipped. Every other caller sees an ordinary error,
// since Error and Unwrap defer to the cause.
type flushError struct {
	err   error
	wrote map[string]bool
}

func (e flushError) Error() string { return e.err.Error() }
func (e flushError) Unwrap() error { return e.err }

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), atomic.AddUint64(&tmpSeq, 1))
	if err := os.WriteFile(tmp, data, mode); err != nil {
		// A partial file may already be there, and nothing will ever come
		// back for it: the name carries a sequence number no later write
		// reuses. Leaving it turns every failed write into litter beside
		// the file it was meant to replace.
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
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

// TasksDir is where the task documents live: inside the user's project,
// never the dotdir Dir returns. s.root is set once at construction and
// never changes, so this needs no lock.
func (s *Store) TasksDir() string { return s.tasksDir() }

// HasTaskDocuments reports whether at least one task document exists on
// disk — the true test for "/task open": the tasks directory can exist
// with nothing but README.md, or a reserved document emptied of every
// bullet, and neither is a task record a person would find anything in.
func (s *Store) HasTaskDocuments() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.docs) > 0
}

// Flush writes every task document that changed, the dotdir state and the
// durable notes. The whole snapshot is taken under the lock; the file writes
// happen with it released, so a slow disk never blocks a tool call. dirty is
// cleared afterwards only if every write succeeded and nothing changed in the
// meantime.
//
// It never replaces content it has not seen. Before anything is rendered the
// documents are read back and whatever changed on disk is merged into the
// tree (reconcile, merge.go); and each write looks once more at the file it
// is about to replace (writeDoc), copying it aside if it is still not what
// the engine last saw there.
func (s *Store) Flush() error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	s.mu.Lock()
	dirty := s.dirty
	s.mu.Unlock()
	if !dirty {
		return nil
	}
	s.reconcile()

	s.mu.Lock()
	seq := s.mutSeq
	dir, root := s.dir, s.root

	type docWrite struct {
		name, body, known string
		base              *Node
		split             bool
	}
	names := s.docNamesLocked()
	var writes []docWrite
	docs := map[string]string{}
	for i, r := range s.tree.Roots {
		if s.noWorkspace {
			break
		}
		if spentUnfiled(r) {
			continue
		}
		body := RenderDocWithExtra(docNumber(names[i], i), r.Text, r, s.extraAt(i))
		docs[names[i]] = hashBytes([]byte(body))
		writes = append(writes, docWrite{
			name: names[i], body: body, known: s.docs[names[i]],
			base: stripRaw(copyNodes([]*Node{r}))[0],
			// Split out of a document the user added a second task to: it
			// has a source file, and that file is not the one it is going to.
			split: i < len(s.files) && s.files[i] != "" && s.files[i] != names[i],
		})
	}
	stateBytes, err := json.Marshal(s.stateLocked(docs))
	if err != nil {
		s.mu.Unlock()
		return flushError{err: err}
	}
	notes := []byte(s.notes)
	s.mu.Unlock()

	// Ruling T3-g, corrected. A root split out of a document is written
	// before the document it came from is rewritten without it: the other way
	// round, a failure between the two erases the hand-added task from the
	// only file that held it.
	sort.SliceStable(writes, func(i, j int) bool { return writes[i].split && !writes[j].split })

	wrote := map[string]bool{}
	written := map[string]docWrite{}
	// Whatever reached the disk is remembered as seen, on the failure paths
	// too: a retry must recognise this flush's own documents, or it would
	// find "content it has not seen" under its own names and set copies of
	// its own work aside.
	remember := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for name, w := range written {
			s.docs[name] = hashBytes([]byte(w.body))
			s.base[name] = w.base
		}
	}
	fail := func(err error) error {
		remember()
		return flushError{err: err, wrote: wrote}
	}
	if len(writes) > 0 {
		tasks := filepath.Join(root, workspaceDir, "tasks")
		if err := os.MkdirAll(tasks, 0o755); err != nil {
			return flushError{err: err}
		}
		ensureReadme(tasks)
		for _, w := range writes {
			if w.known == hashBytes([]byte(w.body)) {
				// What is there is already this. Not writing it is what keeps
				// a request that changed one task from touching every
				// document in the project.
				wrote[w.name] = true
				continue
			}
			if err := s.writeDoc(tasks, w.name, []byte(w.body), w.known); err != nil {
				return fail(err)
			}
			wrote[w.name] = true
			written[w.name] = w
		}
		ensureGitignore(root)
	}
	if err := writeAtomic(filepath.Join(dir, "state.json"), stateBytes, 0o600); err != nil {
		return fail(err)
	}
	if err := writeAtomic(filepath.Join(dir, "notes.md"), notes, 0o600); err != nil {
		return fail(err)
	}

	remember()
	s.mu.Lock()
	if !s.noWorkspace {
		for i, name := range names {
			if i < len(s.tree.Roots) {
				for len(s.files) <= i {
					s.files = append(s.files, "")
				}
				s.files[i] = name
			}
		}
	}
	if s.mutSeq == seq {
		s.dirty = false
	}
	s.mu.Unlock()
	return nil
}

// writeDoc replaces one task document, but only over content the engine has
// seen. known is the hash it last saw there ("" for a name it has never
// written). reconcile has already merged every document that parsed, so what
// reaches here as a mismatch is what no merge could take: a document the user
// broke or emptied, a file that appeared under a name about to be used, or an
// edit that landed in the instant since. The on-disk version is copied aside
// as NNN-<slug>.edited-<stamp>.md before the record is written, so neither
// side is lost, and the human is told.
func (s *Store) writeDoc(tasks, name string, body []byte, known string) error {
	path := filepath.Join(tasks, name)
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link; not writing through it", path)
	}
	if cur, err := os.ReadFile(path); err == nil && hashBytes(cur) != known && hashBytes(cur) != hashBytes(body) {
		aside, err := freeName(filepath.Join(tasks, strings.TrimSuffix(name, ".md")+editedSuffix+stamp()))
		if err != nil {
			return err
		}
		aside += ".md"
		if err := writeAtomic(aside, cur, 0o644); err != nil {
			return err
		}
		s.warnf("%s changed on disk in a way that could not be merged; that version is kept as %s and the task record was written over the original", name, filepath.Base(aside))
	}
	return writeAtomic(path, body, 0o644)
}

// docNamesLocked decides which file each root is written to. Two rules, and
// between them the engine never renames or removes a document:
//
//   - a root that already has a file keeps it for life, even when its text
//     changes. The name goes stale against the title; the heading inside is
//     regenerated and is what anyone actually reads. Renaming would mean
//     leaving the old file to reload as a duplicate task, or deleting a file
//     in the user's project, and neither is worth a tidier name.
//   - a name is claimed by exactly one root. A document the user has added a
//     second top-level task to arrives as two roots pointing at one file;
//     the first keeps it and the second gets a new one, so writing the
//     second cannot destroy the first.
//
// Callers hold s.mu.
func (s *Store) docNamesLocked() []string {
	names := make([]string, len(s.tree.Roots))
	used := map[string]bool{}
	// A number identifies a document as surely as its name does, so both are
	// claimed: two files sharing an NNN read as one task split in half.
	usedNum := map[int]bool{}
	claim := func(name string) {
		used[name] = true
		if n := leadingNumber(name); n > 0 {
			usedNum[n] = true
		}
	}
	for name := range s.reserved {
		claim(name)
	}
	for i := range s.tree.Roots {
		if i < len(s.files) && s.files[i] != "" && !used[s.files[i]] {
			names[i] = s.files[i]
			claim(names[i])
		}
	}
	for i, r := range s.tree.Roots {
		if names[i] != "" {
			continue
		}
		for n := i + 1; ; n++ {
			cand := fmt.Sprintf("%03d-%s.md", n, slug(r.Text))
			if usedNum[n] || used[cand] {
				continue
			}
			names[i] = cand
			claim(cand)
			break
		}
	}
	return names
}

// leadingNumber is the NNN a document name starts with, or 0.
func leadingNumber(name string) int {
	digits := 0
	for digits < len(name) && name[digits] >= '0' && name[digits] <= '9' {
		digits++
	}
	n, err := strconv.Atoi(name[:digits])
	if err != nil {
		return 0
	}
	return n
}

// docNumber is the NNN a document's heading carries: the one in its file
// name, so the two never disagree, falling back to the root's position.
func docNumber(name string, i int) string {
	if n := leadingNumber(name); n > 0 {
		return fmt.Sprintf("%03d", n)
	}
	return fmt.Sprintf("%03d", i+1)
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
		Turn:     s.turn,
		Docs:     docs,
		Session:  s.session,
		Baseline: s.baseline,
		Files:    s.fileMemosLocked(),
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
	times := map[string]nodeTimes{}
	s.tree.Walk(func(n *Node, _ int) {
		if !n.Started.IsZero() {
			times[n.ID] = nodeTimes{Text: n.Text, Started: n.Started, Closed: n.Closed, Calls: n.Calls}
		}
	})
	if len(times) > 0 {
		st.Times = times
	}
	return st
}

// restoreTimes puts the recorded times back on the nodes they describe. The
// documents are the truth about the tree, so a record whose node is gone, or
// whose text no longer matches (the user renumbered or rewrote it), is
// dropped rather than attached to the wrong step. A node the document shows
// as still open keeps no closing time.
func (s *Store) restoreTimes(times map[string]nodeTimes) {
	for id, nt := range times {
		n := s.tree.Find(id)
		if n == nil || n.Text != nt.Text {
			continue
		}
		n.Started, n.Calls = nt.Started, nt.Calls
		if n.Status.terminal() && !nt.Closed.IsZero() {
			n.Closed = nt.Closed
		}
	}
}

// DoingClock is the current step and when work on it began, for the agent's
// time footer. ok is false when nothing is doing.
func (s *Store) DoingClock() (id string, started time.Time, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.tree.Doing()
	if d == nil || d.Started.IsZero() {
		return "", time.Time{}, false
	}
	return d.ID, d.Started, true
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
		// ".be-code/", "/.be-code/", ".be-code" and "/.be-code" all already
		// ignore the folder; appending another spelling would just be noise
		// in someone's file.
		if strings.TrimRight(strings.TrimPrefix(strings.TrimSpace(line), "/"), "/") == workspaceDir {
			return
		}
	}
	out := string(b)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	writeAtomic(path, []byte(out+workspaceDir+"/\n"), mode)
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

// ClearSession is the user's "/task clear": it closes the work, it does not
// erase it. Every node still open is dropped with the reason "cleared", the
// buffers are distilled into the record on the way, and the lookup cache and
// the baseline are reset. The tree stays and so do the documents — a reset
// that deletes files in someone's project on one keystroke is not a reset
// (ruling T3-a). Nothing here touches the durable notes.
func (s *Store) ClearSession() {
	snap := s.rawSnaps()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.tree.Walk(func(n *Node, _ int) {
		if len(n.Evidence.Raw) > 0 {
			s.rec.distillWith(n, snap)
		}
		if n.Status.terminal() {
			return
		}
		n.Status, n.Reason, n.Closed = StatusDropped, "cleared", now
	})
	s.lookups = nil
	s.cached = nil
	s.baseline = Baseline{}
	s.markDirtyLocked()
}

// ---------------------------------------------------------------- the tree

// Plan records a task and its steps and returns the new root's id. A node
// still doing is distilled first, so its evidence is never left raw.
func (s *Store) Plan(text string, steps []string) string {
	snap := s.rawSnaps()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeDoingLocked(snap)
	s.freshRoot = false // a plan is a task of this session's own
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
	snap := s.rawSnaps()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.tree.Find(id)
	if n == nil {
		return fmt.Errorf("no node %s", id)
	}
	if status == StatusDoing {
		// Spec 4.2: what the model did before it named a step belongs to
		// the step it then named. Adoption runs before the distill below
		// so the evidence arrives verbatim — the unfiled node is not
		// "previous work", it is this node's own first minutes.
		s.adoptUnfiledLocked(n)
		if prev := s.tree.Doing(); prev != nil && prev != n {
			s.rec.distillWith(prev, snap)
		}
	}
	s.tree.SetStatus(id, status, reason)
	if status.terminal() {
		s.rec.distillWith(n, snap)
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
	if r := s.activeRootLocked(); r != nil && !s.freshRoot {
		return r.ID
	}
	s.freshRoot = false
	n := s.tree.Add("", firstLine(text, 200))
	s.markDirtyLocked()
	return n.ID
}

// ActiveRootID is the id of the task in flight — the newest root that is
// not wholly finished — or "" when none is open. It exists for the task
// tool's optional taskRoots capability: resolving a 0.10.0 step number
// against the right task rather than against a root of the same number.
func (s *Store) ActiveRootID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.activeRootLocked(); r != nil {
		return r.ID
	}
	return ""
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
	// A named node also prints its verbatim buffer, whole. The prompt block
	// is budgeted and shows only the newest of it (ruling F-1); this is the
	// way back to the rest, which is why the block names this command.
	sub := Tree{Roots: []*Node{n}}
	sub.Walk(func(m *Node, _ int) {
		if raw := rawBlock(m.Evidence.Raw, m.Evidence.Dropped); raw != "" {
			fmt.Fprintf(&b, "\n%s, verbatim — %s\n", m.ID, raw)
		}
	})
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
	if host == nil || s.freshRoot {
		// No task open — or only one a previous session left, which is not
		// this session's to file evidence under.
		s.freshRoot = false
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

// adoptUnfiledLocked moves everything an unfiled node collected while
// nothing was doing onto n, and takes the unfiled node out of the tree.
// Spec 4.2 creates that node so evidence always has a home; this is the
// other half of it, the moment the model finally says what it was doing.
//
// Only a node still open is adopted: one that has already closed has been
// distilled into a Task Report, and a report that silently loses its
// contents is worse than an "unfiled" line in it.
func (s *Store) adoptUnfiledLocked(n *Node) {
	if n == nil || n.Text == unfiledText {
		return
	}
	var unfiled []*Node
	s.tree.Walk(func(m *Node, _ int) {
		if m != n && m.Text == unfiledText && !m.Status.terminal() {
			unfiled = append(unfiled, m)
		}
	})
	for _, u := range unfiled {
		mergeEvidence(&n.Evidence, u.Evidence)
		u.Evidence = Evidence{}
		// Two nodes must be emptied and dropped rather than removed:
		//
		//   a root, because s.files and s.extra are index-parallel to
		//   Roots — taking one out shifts every later task onto the
		//   previous one's document and orphans its own, which on the
		//   next load reads as a duplicated task;
		//
		//   one with children, which nothing creates but a model naming
		//   its id as a parent could, because removing it would take
		//   them with it.
		if len(u.Children) == 0 && !s.isRootLocked(u) {
			s.tree.Remove(u)
		} else {
			u.Status = StatusDropped
			u.Reason = adoptedBy + n.ID
		}
		s.markDirtyLocked()
	}
	if len(unfiled) > 0 {
		s.rec.capNode(n)
	}
}

// spentUnfiled reports whether n is a root-level "unfiled" node that
// adoption has already emptied. Such a node has to stay in the tree —
// removing a root shifts every later task onto the previous one's document
// — but it is an artefact of the harness, not a task anyone asked for, so
// it gets no document of its own in the user's repository and no Task
// Report in the prompt. Ruling T5-c: by ruling T3-a nothing would ever
// delete either, so they must not be written in the first place.
//
// Only a childless one: anything filed under it is real work, and a node
// that still has children is a task like any other.
//
// And only one adoption really emptied (ruling T5-d): dropped, with the
// reason adoption writes. Text and childlessness alone would also match a
// real task someone titled "unfiled", which would then silently get no
// document and no report, and a root-level unfiled node still doing.
func spentUnfiled(n *Node) bool {
	return n != nil && n.Text == unfiledText && len(n.Children) == 0 &&
		n.Status == StatusDropped && strings.HasPrefix(n.Reason, adoptedBy)
}

// adoptedBy starts the reason adoption gives the node it emptied.
const adoptedBy = "adopted by "

// isRootLocked reports whether n is a top-level task. A root owns a
// document, and its position owns that document's place in s.files.
func (s *Store) isRootLocked(n *Node) bool {
	for _, r := range s.tree.Roots {
		if r == n {
			return true
		}
	}
	return false
}

// mergeEvidence folds src into dst. The raw buffer goes in front, because
// what the unfiled node saw happened before anything dst has seen, and the
// verbatim block is read in order.
func mergeEvidence(dst *Evidence, src Evidence) {
	for _, f := range src.Files {
		var ref *FileRef
		for i := range dst.Files {
			if dst.Files[i].Path == f.Path {
				ref = &dst.Files[i]
				break
			}
		}
		if ref == nil {
			dst.Files = append(dst.Files, f)
			continue
		}
		for _, r := range f.Ranges {
			ref.Ranges = mergeRange(ref.Ranges, r)
		}
		ref.Edited = ref.Edited || f.Edited
		if f.Turn > ref.Turn {
			ref.Turn = f.Turn
			if f.Hash != "" {
				ref.Hash = f.Hash
			}
			if len(f.Outline) > 0 {
				ref.Outline = append([]string(nil), f.Outline...)
			}
			if f.Note != "" {
				ref.Note = f.Note
			}
		}
	}
	dst.Cmds = append(append([]CmdRef(nil), src.Cmds...), dst.Cmds...)
	dst.Lookups = append(append([]LookupRef(nil), src.Lookups...), dst.Lookups...)
	dst.Notes = append(append([]NoteRef(nil), src.Notes...), dst.Notes...)
	dst.Errors = append(append([]string(nil), src.Errors...), dst.Errors...)
	dst.Raw = append(append([]RawItem(nil), src.Raw...), dst.Raw...)
	dst.Dropped += src.Dropped
}

func (s *Store) closeDoingLocked(snap snapFunc) {
	if prev := s.tree.Doing(); prev != nil {
		s.rec.distillWith(prev, snap)
		prev.Status = StatusTodo
	}
}

// rawSnaps reads every workspace file a verbatim buffer names, before the
// caller takes the lock, and returns them as the lookup distillation uses.
// Distilling under the mutex used to read each of those files from disk with
// every other engine call — the prompt's Render above all — parked behind it.
// A file that is not in the map (a call recorded between this snapshot and
// the lock) reads as unknown, which merges its ranges and leaves its hash
// alone: recordWith already merged that call from its whole result.
func (s *Store) rawSnaps() snapFunc {
	s.mu.Lock()
	paths := map[string]bool{}
	s.tree.Walk(func(n *Node, _ int) {
		for _, it := range n.Evidence.Raw {
			if it.Path != "" {
				paths[it.Path] = true
			}
		}
	})
	root := s.root
	s.mu.Unlock()
	snaps := make(map[string]fileSnap, len(paths))
	for p := range paths {
		snaps[p] = snapFile(root, p)
	}
	return func(rel string) fileSnap { return snaps[rel] }
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

// --------------------------------------------------- the request-level seam

// StartTask refreshes the task line for a new request: always when no task
// is open, and otherwise only when no step is in progress and none is still
// to do. A plan the model is part-way through keeps its own task line, so
// the block does not start describing a side question as the task.
func (s *Store) StartTask(text string) {
	line := firstLine(text, 200)
	if line == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r := s.activeRootLocked(); r != nil && !s.freshRoot {
		for _, c := range r.Children {
			if c.Text == unfiledText {
				continue
			}
			if c.Status == StatusDoing || c.Status == StatusTodo {
				return
			}
		}
		if r.Text != line {
			r.Text = line
			s.markDirtyLocked()
		}
		return
	}
	s.freshRoot = false
	s.tree.Add("", line)
	s.markDirtyLocked()
}

// StoppedAt is the text of the node in progress, or "".
func (s *Store) StoppedAt() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.tree.Doing()
	if d == nil || d.Text == unfiledText {
		return ""
	}
	return d.Text
}

// ApplyFileNotes stores "path — note" lines from a compaction summary's
// files: block against files the record already knows.
func (s *Store) ApplyFileNotes(block string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range fileNoteLine.FindAllStringSubmatch(block, -1) {
		path := relPath(m[1])
		note := strings.TrimSpace(m[2])
		s.tree.Walk(func(n *Node, _ int) {
			for i := range n.Evidence.Files {
				if n.Evidence.Files[i].Path == path {
					n.Evidence.Files[i].Note = note
					s.markDirtyLocked()
				}
			}
		})
	}
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
