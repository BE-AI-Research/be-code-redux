package engine

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

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
var hitLine = regexp.MustCompile(`^([^\s:][^:]*):(\d+):(.*)$`)

// Limits bounds the recorder's raw buffer: ItemCap per tool result, NodeCap
// per node in total, NotesCap the durable notes. All in bytes.
type Limits struct{ NotesCap, ItemCap, NodeCap int }

// recorder is the continuous half of a node's evidence: it keeps every tool
// result verbatim (capped) while a node is doing, refreshes the durable
// file record as it goes, and distills the buffer into the rest of the
// durable record when the node closes.
//
// tree is the Store's own tree when the recorder is embedded in one, and
// nil when it stands alone (its unit tests). It is what makes the
// redundant-read check store-wide rather than per-node.
type recorder struct {
	lim  Limits
	root string // workspace root, for path resolution
	tree *Tree
}

// record files one tool result against the node that is doing. It returns
// the footer to append to the tool result, or "".
//
// The raw item is what makes the recent work lossless: it is exactly what
// the tool returned, capped so one large read cannot swallow the buffer.
func (r *recorder) record(n *Node, ev Event, turn int) string {
	return r.recordWith(n, ev, turn, snapFor(r.root, ev))
}

// recordWith is record over a snapshot of the file the event names, taken
// by the caller. The Store takes it before it locks, so the one piece of
// I/O the record needs never happens under the mutex.
func (r *recorder) recordWith(n *Node, ev Event, turn int, snap fileSnap) string {
	if n == nil {
		return ""
	}
	// The path is resolved from the event, not from the raw item's Args:
	// Args is excerpted to the item cap, so a long enough path would
	// silently stop matching itself.
	rel := r.rel(argStr(ev.Args, "path", "file", "filename"))
	item := RawItem{
		Tool: ev.Tool,
		Path: rel,
		Args: excerpt(argsLine(ev.Args), r.lim.ItemCap),
		Out:  excerpt(ev.Content, r.lim.ItemCap),
		OK:   !ev.IsError,
		Turn: turn,
	}
	n.Evidence.Raw = append(n.Evidence.Raw, item)
	r.capNode(n)

	if ev.IsError {
		return ""
	}
	switch ev.Tool {
	case "read_file":
		// The footer is decided before the read is merged in, or every read
		// would look like a repeat of itself.
		footer := r.readFooter(n, rel, seenRange(ev, countLines(ev.Content)), snap)
		r.mergeFile(n, item, snap)
		return footer
	case "write_file", "edit_file":
		r.mergeFile(n, item, snap)
	}
	return ""
}

// capNode drops the oldest raw items until the node is under its byte cap,
// counting what it dropped. A record that quietly loses half its evidence
// while still reading as complete is worse than one that admits the gap.
func (r *recorder) capNode(n *Node) {
	total := 0
	for _, it := range n.Evidence.Raw {
		total += len(it.Out) + len(it.Args)
	}
	// The len(...)>1 guard never drops the last remaining item, even if it
	// alone is over NodeCap (only possible when ItemCap > NodeCap): there
	// has to be something left to distill, and evidence that reads as
	// over-budget is safer than a node with none at all.
	for total > r.lim.NodeCap && len(n.Evidence.Raw) > 1 {
		drop := n.Evidence.Raw[0]
		total -= len(drop.Out) + len(drop.Args)
		n.Evidence.Raw = n.Evidence.Raw[1:]
		n.Evidence.Dropped++
	}
}

// readFooter answers a redundant read the way 0.10.0 did: when the range
// just returned is already covered by an earlier read of the same,
// unchanged file, it names the turn that covered it.
//
// Ruling T2-b: with a tree attached the check is store-wide and outlives
// the session, because the footer exists to stop the model re-reading what
// it has already been shown, and a node closing is no reason to forget
// that. It is also hash-aware: a file that changed on disk is never
// redundant. Standing alone, the recorder falls back to this node's own
// verbatim buffer, which is all it has.
func (r *recorder) readFooter(n *Node, rel string, want Range, snap fileSnap) string {
	if rel == "" {
		return ""
	}
	if r.tree != nil {
		turn, found := 0, false
		r.tree.Walk(func(m *Node, _ int) {
			for _, f := range m.Evidence.Files {
				if f.Path != rel || f.Hash == "" || f.Hash != snap.hash {
					continue
				}
				if covered(f.Ranges, want) {
					turn, found = f.Turn, true
				}
			}
		})
		if found {
			return fmt.Sprintf(footerAlreadyRead, turn)
		}
		return ""
	}
	raw := n.Evidence.Raw
	var have []Range
	found := false
	prevTurn := 0
	for i := 0; i < len(raw)-1; i++ { // exclude the item recordWith just appended
		it := raw[i]
		if it.Tool != "read_file" || it.Path != rel {
			continue
		}
		have = mergeRange(have, seenRange(Event{Content: it.Out}, countLines(it.Out)))
		prevTurn = it.Turn
		found = true
	}
	if found && covered(have, want) {
		return fmt.Sprintf(footerAlreadyRead, prevTurn)
	}
	return ""
}

// distill turns the raw buffer into the durable record and empties it. It
// reads only the buffer, never the transcript, so it does not depend on the
// transcript still existing — which after a compaction it does not.
func (r *recorder) distill(n *Node) {
	if n == nil {
		return
	}
	for _, it := range n.Evidence.Raw {
		switch it.Tool {
		case "read_file", "write_file", "edit_file":
			// Idempotent with the merge recordWith already did, so a node
			// distilled twice — or one whose store merged as it went — is
			// the same record either way.
			r.mergeFile(n, it, snapFile(r.root, it.Path))
		case "shell", "process":
			n.Evidence.Cmds = append(n.Evidence.Cmds, CmdRef{
				Cmd: it.Args, OK: it.OK, Excerpt: excerpt(it.Out, 240),
			})
		case "search", "lookup", "history":
			n.Evidence.Lookups = append(n.Evidence.Lookups, LookupRef{
				Tool: it.Tool, Query: it.Args, Hits: parseHits(it.Out),
			})
		}
		if !it.OK {
			line := firstOutputLine(it.Out)
			if line == "" {
				line = "(no output)"
			}
			// Spec 4.2: an error names the command that produced it.
			// Errors stays []string (later tasks consume that shape), so
			// the command and the line share one entry.
			n.Evidence.Errors = append(n.Evidence.Errors, it.Args+": "+line)
		}
	}
	n.Evidence.Raw = nil
}

// mergeFile folds one read/write/edit raw item into the node's FileRef
// list: either replace the outline (the content changed, or was unreadable
// last time) or merge the seen range. It is the durable half of what the
// old observeRead/observeWrite pair did, aimed at a node's FileRef instead
// of a store-wide digest.
func (r *recorder) mergeFile(n *Node, it RawItem, snap fileSnap) {
	rel := it.Path
	if rel == "" {
		return
	}
	// A failed read/write/edit told the model nothing about the file's
	// actual content: merging it in would mark a failed write "edited" and
	// stretch Ranges over the error text seenRange mistakes for output.
	if !it.OK {
		return
	}
	ref := fileRefFor(n, rel)
	if it.Tool == "write_file" || it.Tool == "edit_file" {
		ref.Edited = true
	}
	if it.Turn > ref.Turn {
		ref.Turn = it.Turn
	}
	// What each tool tells us about what the model has actually seen:
	//   read_file  — the lines it echoed back.
	//   write_file — the whole file; the model authored every line of it.
	//   edit_file  — nothing. It replaced a fragment of a file it may never
	//                have read, so the echo is not content and claiming the
	//                whole file would answer a later read with "already read
	//                at turn N" for lines nobody has seen.
	rng, known := seenRange(Event{Content: it.Out}, countLines(it.Out)), true
	switch it.Tool {
	case "write_file":
		if snap.ok {
			rng = Range{From: 1, To: countLines(string(snap.data))}
		}
	case "edit_file":
		known = false
	}
	if snap.ok && ref.Hash != snap.hash {
		ref.Hash = snap.hash
		ref.Outline = repomap.Outline(rel, snap.data)
		// The content moved, so the ranges seen before it moved are stale.
		ref.Ranges = nil
		if known {
			ref.Ranges = []Range{rng}
		}
		return
	}
	if known {
		ref.Ranges = mergeRange(ref.Ranges, rng)
	}
}

// rel folds a model-supplied path onto the root-relative key evidence is
// filed under. One resolver, shared with the Store.
func (r *recorder) rel(p string) string { return foldPath(r.root, p) }

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

// parseHits reads file:line:text lines out of a search or lookup result;
// context lines (file-line-text, git grep's context form) are deliberately
// not parsed, since a bare "-" separator is indistinguishable from one
// inside a filename.
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

// argsLine renders the argument that identifies a call, for the raw item
// and for the distilled CmdRef/LookupRef: command for shell and process,
// path or file for the file tools, query or pattern for the lookups, else
// the compact JSON of the whole map.
func argsLine(args map[string]any) string {
	if v := argStr(args, "command"); v != "" {
		return v
	}
	if v := argStr(args, "path", "file", "filename"); v != "" {
		return v
	}
	if v := argStr(args, "query", "pattern"); v != "" {
		return v
	}
	b, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	return string(b)
}

// countLines counts the lines in s, counting a trailing partial line.
func countLines(s string) int {
	n := strings.Count(s, "\n")
	if len(s) > 0 && s[len(s)-1] != '\n' {
		n++
	}
	return n
}

func firstOutputLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// excerpt caps a string at n bytes on a line boundary, never mid-rune, and
// says so when it cut.
func excerpt(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	cut := s[:n]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	} else {
		for len(cut) > 0 && !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
	}
	return cut + "\n… (truncated)"
}
