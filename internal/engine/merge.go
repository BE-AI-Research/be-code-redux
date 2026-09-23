package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// This file is what makes the README's promise true while a session is
// running. A task document is the user's file: they may retitle a task, move
// a status mark, delete a note or write prose under it at any moment, and a
// second session on the same workspace writes to the same documents. Flush
// used to render its own tree over whatever was there. It now looks first
// (reconcile), merges what changed on disk into the tree, and only then
// writes — and the write itself refuses to replace content it has not seen
// (writeDoc), copying it aside instead.
//
// The merge is three-way. theirs is the document as it is on disk, ours is
// the tree in memory, and base is the document as the engine last saw it on
// disk — loaded, written or merged. base is what tells an edit from an
// absence: a note in ours and not in base is new evidence to append; a note
// in base and not in theirs is one the user deleted, and it stays deleted.
// The user's edits win for text, status, reason and their own prose, and the
// engine's new evidence is appended.

// editedSuffix marks the copy the engine sets aside when it finds content on
// disk it cannot merge and would otherwise replace.
const editedSuffix = ".edited-"

// isTaskDoc reports whether a name in the tasks directory is a task document
// the engine loads: not the README, not a quarantined document, not a copy
// set aside by a conflicting write.
func isTaskDoc(name string) bool {
	return strings.HasSuffix(name, ".md") && name != "README.md" &&
		!strings.Contains(name, ".broken-") && !strings.Contains(name, editedSuffix)
}

// Reload reads back whatever changed in the task documents since the engine
// last looked: a hand edit, or another session's flush. The agent calls it at
// the top of every request, so the request works from the user's edit rather
// than over it. It never writes; what it found is merged into the tree and
// reaches the disk with the next Flush.
func (s *Store) Reload() {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.reconcile()
}

// diskDoc is one document as reconcile found it, read and parsed with the
// lock released.
type diskDoc struct {
	name  string
	data  []byte
	tree  *Tree
	extra []string
	err   error
}

// reconcile merges every document that differs from what the engine last saw
// into the tree. Callers hold flushMu and not mu.
func (s *Store) reconcile() {
	s.mu.Lock()
	if s.noWorkspace {
		s.mu.Unlock()
		return
	}
	known := make(map[string]string, len(s.docs))
	for name, h := range s.docs {
		known[name] = h
	}
	reserved := make(map[string]bool, len(s.reserved))
	for name := range s.reserved {
		reserved[name] = true
	}
	dir := s.tasksDir()
	s.mu.Unlock()

	ents, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	var changed []diskDoc
	seen := map[string]bool{}
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !isTaskDoc(name) {
			continue
		}
		seen[name] = true
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || known[name] == hashBytes(b) {
			continue
		}
		d := diskDoc{name: name, data: b}
		d.tree, d.extra, d.err = ParseDoc(string(b))
		if reserved[name] && d.err == nil && len(d.tree.Roots) == 0 {
			continue // still the user's prose and nothing else
		}
		changed = append(changed, d)
	}
	sort.Slice(changed, func(i, j int) bool { return changed[i].name < changed[j].name })

	s.mu.Lock()
	defer s.mu.Unlock()
	for name := range s.docs {
		if !seen[name] {
			// The user deleted it. Forgetting the hash is all that is needed:
			// the next write finds nothing there and puts the record back,
			// which loses nothing of theirs.
			delete(s.docs, name)
			delete(s.base, name)
		}
	}
	for _, d := range changed {
		s.mergeDocLocked(d)
	}
}

// mergeDocLocked folds one changed document into the tree. Callers hold mu.
func (s *Store) mergeDocLocked(d diskDoc) {
	var mine []int
	for i := range s.tree.Roots {
		if i < len(s.files) && s.files[i] == d.name {
			mine = append(mine, i)
		}
	}
	if d.err != nil {
		// Not parseable, so not mergeable. The hash is left stale on purpose:
		// if this store then has to write that name, writeDoc sees content it
		// does not know and copies it aside first. A document nothing here
		// writes to is left for the next open, which quarantines it.
		return
	}
	if len(mine) == 0 {
		// A document this store has never had: another session's task, or one
		// made by hand. It joins the tree the way it would on the next open.
		if len(d.tree.Roots) == 0 {
			s.reserved[d.name] = true
			return
		}
		extra := d.extra
		for _, r := range d.tree.Roots {
			id := fmt.Sprint(nextIndex(s.tree.Roots))
			renumber(r, id)
			s.tree.Roots = append(s.tree.Roots, r)
			for len(s.files) < len(s.tree.Roots)-1 {
				s.files = append(s.files, "")
				s.extra = append(s.extra, nil)
			}
			s.files = append(s.files, d.name)
			s.extra = append(s.extra, extra)
			extra = nil
		}
		s.docs[d.name] = hashBytes(d.data)
		s.base[d.name] = stripRaw(copyNodes(d.tree.Roots[:1]))[0]
		s.mutSeq++ // the tree changed; whether it needs writing is decided below
		s.dirtyIfDiffers(d, len(s.tree.Roots)-len(d.tree.Roots))
		return
	}
	if len(d.tree.Roots) == 0 {
		// The user took the task out of its own document while it was being
		// worked on. There is no merge that keeps both: their prose is kept
		// with the task, and writeDoc copies their version aside before the
		// record goes back, because the hash is left stale.
		s.setExtraLocked(mine[0], d.extra)
		s.markDirtyLocked()
		return
	}

	ours := s.tree.Roots[mine[0]]
	var userDoing *Node
	mergeNode(ours, d.tree.Roots[0], s.base[d.name], &userDoing)
	s.setExtraLocked(mine[0], d.extra)
	// A second top-level task written into the document by hand: pair it with
	// a root this store already split out of the same file, or take it on.
	for _, t := range d.tree.Roots[1:] {
		paired := false
		for _, i := range mine[1:] {
			if s.tree.Roots[i].Text == t.Text {
				mergeNode(s.tree.Roots[i], t, nil, &userDoing)
				paired = true
				break
			}
		}
		if paired {
			continue
		}
		renumber(t, fmt.Sprint(nextIndex(s.tree.Roots)))
		s.tree.Roots = append(s.tree.Roots, t)
		for len(s.files) < len(s.tree.Roots)-1 {
			s.files = append(s.files, "")
			s.extra = append(s.extra, nil)
		}
		s.files = append(s.files, d.name)
		s.extra = append(s.extra, nil)
	}
	if userDoing != nil {
		// Exactly one node is doing. A mark the user moved by hand is their
		// intent (spec §5.2), so the engine's own gives way — distilled first,
		// as any node leaving doing is. No file is read for it: the record
		// merged every file as it was seen.
		s.tree.Walk(func(n *Node, _ int) {
			if n != userDoing && n.Status == StatusDoing {
				s.rec.distillWith(n, nil)
				n.Status = StatusTodo
			}
		})
	}
	s.docs[d.name] = hashBytes(d.data)
	s.base[d.name] = stripRaw(copyNodes(d.tree.Roots[:1]))[0]
	s.mutSeq++
	s.dirtyIfDiffers(d, mine[0])
}

// dirtyIfDiffers marks the store dirty when the root at i no longer renders
// as the document on disk does — when the merge kept something of ours that
// the disk has not got yet. A merge that only took the user's edit in leaves
// nothing to write.
func (s *Store) dirtyIfDiffers(d diskDoc, i int) {
	if i < 0 || i >= len(s.tree.Roots) {
		return
	}
	r := s.tree.Roots[i]
	body := RenderDocWithExtra(docNumber(d.name, i), r.Text, r, s.extraAt(i))
	if body != string(d.data) || len(d.tree.Roots) > 1 {
		s.markDirtyLocked()
	}
}

func (s *Store) setExtraLocked(i int, extra []string) {
	for len(s.extra) <= i {
		s.extra = append(s.extra, nil)
	}
	s.extra[i] = extra
}

// stripRaw empties the verbatim buffers of a copied subtree: base mirrors
// what a document holds, and a document never holds those.
func stripRaw(ns []*Node) []*Node {
	for _, n := range ns {
		n.Evidence.Raw = nil
		stripRaw(n.Children)
	}
	return ns
}

// mergeNode folds theirs into ours, in place, with base as the common
// ancestor (nil when there is none, and then theirs wins wherever the two
// differ, since nothing says the difference is the engine's). userDoing is
// set to the node whose doing mark came from the document.
func mergeNode(ours, theirs, base *Node, userDoing **Node) {
	if base == nil || theirs.Text != base.Text {
		ours.Text = theirs.Text
	}
	if base == nil || theirs.Status != base.Status || theirs.Reason != base.Reason {
		if ours.Status != theirs.Status {
			if theirs.Status == StatusDoing {
				*userDoing = ours
			}
			if theirs.Status.terminal() && ours.Closed.IsZero() {
				ours.Closed = time.Now()
			}
			if !theirs.Status.terminal() {
				ours.Closed = time.Time{}
			}
		}
		ours.Status, ours.Reason = theirs.Status, theirs.Reason
	}
	// The assignment is the operator's exactly as the status mark is (spec
	// §3.1, §4.3): an `@owner`, a `scope:` or an `after:` edited by hand
	// wins over the tree, and one *removed* by hand is removed here too.
	// Without this the document's assignment never reaches the tree and the
	// next Flush writes the engine's render back over the operator's file.
	if base == nil || theirs.Owner != base.Owner || theirs.OwnerPinned != base.OwnerPinned {
		ours.Owner, ours.OwnerPinned = theirs.Owner, theirs.OwnerPinned
	}
	if base == nil || !sameStrings(theirs.Scope, base.Scope) {
		ours.Scope = append([]string(nil), theirs.Scope...)
	}
	if base == nil || !sameStrings(theirs.After, base.After) {
		ours.After = append([]string(nil), theirs.After...)
	}
	var baseEv Evidence
	if base != nil {
		baseEv = base.Evidence
	}
	ours.Evidence = mergeDocEvidence(ours.Evidence, theirs.Evidence, baseEv)

	var baseKids []*Node
	if base != nil {
		baseKids = base.Children
	}
	toTheirs := pairNodes(ours.Children, theirs.Children)
	toBase := pairNodes(ours.Children, baseKids)
	taken := map[*Node]bool{}
	var kids []*Node
	byTheirs := map[*Node]*Node{}
	for o, t := range toTheirs {
		byTheirs[t] = o
	}
	// The document's order is the user's, so it leads; a step only this store
	// has follows it.
	var added []*Node
	for _, t := range theirs.Children {
		if o := byTheirs[t]; o != nil {
			mergeNode(o, t, toBase[o], userDoing)
			kids = append(kids, o)
			taken[o] = true
			continue
		}
		added = append(added, t)
		kids = append(kids, t)
	}
	for _, o := range ours.Children {
		if taken[o] {
			continue
		}
		// In base and gone from the document: the user deleted it, and it
		// stays deleted — unless the engine has put something on it since
		// that the document never held, which must not be lost with it.
		if b := toBase[o]; b != nil && unchangedSince(o, b) {
			continue
		}
		kids = append(kids, o)
	}
	ours.Children = kids
	// A step the user added gets an id no live sibling has. Ids the model
	// already knows do not move while the session runs. The document's own
	// numbers on the added steps do not count towards "taken": they are about
	// to be replaced.
	isAdded := map[*Node]bool{}
	for _, t := range added {
		isAdded[t] = true
	}
	var settled []*Node
	for _, k := range kids {
		if !isAdded[k] {
			settled = append(settled, k)
		}
	}
	next := nextIndex(settled)
	for _, t := range added {
		renumber(t, ours.ID+"."+fmt.Sprint(next))
		next++
		if d := (&Tree{Roots: []*Node{t}}).Doing(); d != nil {
			*userDoing = d
		}
	}
}

// unchangedSince reports whether o carries nothing base does not: same text
// and status, no verbatim buffer, no evidence line base lacks, and children
// that are all the same way.
func unchangedSince(o, b *Node) bool {
	if o.Text != b.Text || o.Status != b.Status || o.Reason != b.Reason || len(o.Evidence.Raw) > 0 {
		return false
	}
	if o.Owner != b.Owner || o.OwnerPinned != b.OwnerPinned ||
		!sameStrings(o.Scope, b.Scope) || !sameStrings(o.After, b.After) {
		return false
	}
	have := map[string]int{}
	for _, l := range evidenceLines(b.Evidence) {
		have[l]++
	}
	for _, l := range evidenceLines(o.Evidence) {
		if have[l] == 0 {
			return false
		}
		have[l]--
	}
	pairs := pairNodes(o.Children, b.Children)
	for _, c := range o.Children {
		bc := pairs[c]
		if bc == nil || !unchangedSince(c, bc) {
			return false
		}
	}
	return true
}

// sameStrings compares two path lists element by element; order is part of
// the value, since the document prints them in the order it was given.
func sameStrings(a, b []string) bool {
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

// pairNodes matches two sibling lists: identical text first, in order, then
// identical id among what is left. Text first because ids are positional — a
// step inserted by hand shifts every id after it, and pairing by id would
// then read the insertion as a run of retitles.
func pairNodes(a, b []*Node) map[*Node]*Node {
	out := map[*Node]*Node{}
	used := map[*Node]bool{}
	for _, x := range a {
		for _, y := range b {
			if !used[y] && x.Text == y.Text {
				out[x], used[y] = y, true
				break
			}
		}
	}
	for _, x := range a {
		if out[x] != nil {
			continue
		}
		for _, y := range b {
			if !used[y] && lastIndex(x.ID) == lastIndex(y.ID) {
				out[x], used[y] = y, true
				break
			}
		}
	}
	return out
}

// lastIndex is the final component of a dotted id.
func lastIndex(id string) string {
	if i := strings.LastIndexByte(id, '.'); i >= 0 {
		return id[i+1:]
	}
	return id
}

// evidenceLines is a node's durable evidence exactly as a document prints
// it, one string per bullet. It is the identity the merge compares by, so
// "the same note" means the same thing to the merge as to a reader.
func evidenceLines(e Evidence) []string {
	var out []string
	for _, f := range e.Files {
		out = append(out, "files: "+renderFile(f))
	}
	for _, c := range e.Cmds {
		out = append(out, "cmds: "+c.Cmd+" — "+okWord(c.OK))
	}
	for _, l := range e.Lookups {
		out = append(out, "lookups: "+renderLookup(l))
	}
	for _, n := range e.Notes {
		out = append(out, noteLine(n))
	}
	for _, x := range e.Errors {
		out = append(out, "error: "+x)
	}
	return out
}

func noteLine(n NoteRef) string {
	key := "note: "
	if n.Decision {
		key = "decision: "
	}
	if n.File != "" {
		return key + n.Text + " [file: " + n.File + "]"
	}
	return key + n.Text
}

// mergeDocEvidence is theirs plus whatever ours has gained since base.
// Entries the two share keep ours' value, which carries what a document
// cannot: a file's hash and outline, a command's excerpt, a lookup's text.
func mergeDocEvidence(ours, theirs, base Evidence) Evidence {
	out := Evidence{Raw: ours.Raw, Dropped: ours.Dropped}

	// Files are keyed by path: one row per file per node.
	baseRow := map[string]string{}
	for _, f := range base.Files {
		baseRow[f.Path] = renderFile(f)
	}
	ourFile := map[string]FileRef{}
	for _, f := range ours.Files {
		ourFile[f.Path] = f
	}
	inTheirs := map[string]bool{}
	for _, t := range theirs.Files {
		inTheirs[t.Path] = true
		o, have := ourFile[t.Path]
		switch {
		case !have:
			out.Files = append(out.Files, t)
		case renderFile(t) == baseRow[t.Path]:
			out.Files = append(out.Files, o) // untouched on disk: ours stands
		default:
			// The row was edited. Its note and flag are the user's; ranges
			// the engine did not measure itself are kept but no longer vouch
			// for unseen lines, so the hash that pins them goes.
			o.Note, o.Edited = t.Note, t.Edited || o.Edited
			for _, r := range t.Ranges {
				if !covered(o.Ranges, r) {
					o.Ranges = mergeRange(o.Ranges, r)
					o.Hash = ""
				}
			}
			out.Files = append(out.Files, o)
		}
	}
	for _, o := range ours.Files {
		if inTheirs[o.Path] {
			continue
		}
		if row, was := baseRow[o.Path]; was && row == renderFile(o) {
			continue // the user deleted the row and nothing new is on it
		}
		out.Files = append(out.Files, o)
	}

	out.Cmds = mergeList(ours.Cmds, theirs.Cmds, base.Cmds, func(c CmdRef) string { return c.Cmd + " — " + okWord(c.OK) })
	out.Lookups = mergeList(ours.Lookups, theirs.Lookups, base.Lookups, renderLookup)
	out.Notes = mergeList(ours.Notes, theirs.Notes, base.Notes, noteLine)
	out.Errors = mergeList(ours.Errors, theirs.Errors, base.Errors, func(s string) string { return s })
	return out
}

// mergeList is theirs, in the document's order, followed by what ours has
// that base has not — a multiset difference, so a command run twice since the
// last write is two new entries and not one.
func mergeList[T any](ours, theirs, base []T, key func(T) string) []T {
	mine := map[string][]T{}
	for _, o := range ours {
		mine[key(o)] = append(mine[key(o)], o)
	}
	var out []T
	for _, t := range theirs {
		k := key(t)
		if q := mine[k]; len(q) > 0 {
			// Same line on both sides: ours is the richer value. It is not
			// consumed from the "new" count below — that is base's job.
			out = append(out, q[0])
			continue
		}
		out = append(out, t)
	}
	inBase := map[string]int{}
	for _, b := range base {
		inBase[key(b)]++
	}
	onDisk := map[string]int{}
	for _, t := range theirs {
		onDisk[key(t)]++
	}
	seen := map[string]int{}
	for _, o := range ours {
		k := key(o)
		seen[k]++
		// The n-th occurrence is new when base held fewer than n of it; it is
		// already in the output when the document holds n of it anyway.
		if seen[k] > inBase[k] && seen[k] > onDisk[k] {
			out = append(out, o)
		}
	}
	return out
}
