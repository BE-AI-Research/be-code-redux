package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/brown-enterprises/be-code/internal/repomap"
)

// Limits bounds the recorder's raw buffer: ItemCap per tool result, NodeCap
// per node in total. Both in bytes.
type Limits struct{ NotesCap, ItemCap, NodeCap int }

// recorder is the continuous half of a node's evidence: it keeps every tool
// result verbatim (capped) while a node is doing, and distills that buffer
// into the durable record when the node closes. Task 3 embeds it in Store,
// which is where its root and lim fields come from in production; here it
// stands alone so this task compiles and tests without one.
type recorder struct {
	lim  Limits
	root string // workspace root, for path resolution
}

// record files one tool result against the node that is doing. It returns
// the footer to append to the tool result, or "".
//
// The raw item is what makes the recent work lossless: it is exactly what
// the tool returned, capped so one large read cannot swallow the buffer.
func (r *recorder) record(n *Node, ev Event, turn int) string {
	if n == nil {
		return ""
	}
	item := RawItem{Tool: ev.Tool, Args: excerpt(argsLine(ev.Args), r.lim.ItemCap), Out: excerpt(ev.Content, r.lim.ItemCap), OK: !ev.IsError, Turn: turn}
	n.Evidence.Raw = append(n.Evidence.Raw, item)
	r.capNode(n)

	if ev.IsError {
		return ""
	}
	switch ev.Tool {
	case "read_file":
		return r.readFooter(n, ev, turn)
	case "write_file", "edit_file":
		r.markEdited(n, ev)
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

// readFooter answers a redundant read the same way 0.10.0 did: when the
// range just returned is already covered by an earlier read of this node's
// own raw buffer (the doing node has not been distilled yet, so there is no
// FileRef to consult), it names the turn that first covered it. The buffer
// is the source of truth here, not the file on disk: what matters is what
// the model has already been shown in this stretch of work.
func (r *recorder) readFooter(n *Node, ev Event, turn int) string {
	path := argStr(ev.Args, "path", "file", "filename")
	rel := r.rel(path)
	if rel == "" {
		return ""
	}
	want := seenRange(ev, countLines(ev.Content))
	raw := n.Evidence.Raw
	var have []Range
	found := false
	prevTurn := 0
	for i := 0; i < len(raw)-1; i++ { // exclude the item record() just appended
		it := raw[i]
		if it.Tool != "read_file" || r.rel(it.Args) != rel {
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

// markEdited is a placeholder for the doing phase: the durable Edited flag
// is set by mergeFile at distill time, from the raw item's tool name, since
// Files itself is only built then.
func (r *recorder) markEdited(n *Node, ev Event) {}

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
			r.mergeFile(n, it)
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
			line := firstLine(it.Out)
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
// list: resolve the path, hash the current workspace content, and either
// replace the outline (hash changed, or unreadable) or merge the seen
// range (hash unchanged). This is the digest logic the old observeRead
// carried, aimed at a node's FileRef instead of the store's Digest.
func (r *recorder) mergeFile(n *Node, it RawItem) {
	rel := r.rel(it.Args)
	if rel == "" {
		return
	}
	// A failed read/write/edit told the model nothing about the file's
	// actual content: merging it in would mark a failed write "edited" and
	// stretch Ranges over the error text seenRange mistakes for output.
	if !it.OK {
		return
	}
	var ref *FileRef
	for i := range n.Evidence.Files {
		if n.Evidence.Files[i].Path == rel {
			ref = &n.Evidence.Files[i]
			break
		}
	}
	if ref == nil {
		n.Evidence.Files = append(n.Evidence.Files, FileRef{Path: rel})
		ref = &n.Evidence.Files[len(n.Evidence.Files)-1]
	}
	if it.Tool == "write_file" || it.Tool == "edit_file" {
		ref.Edited = true
	}
	rng := seenRange(Event{Content: it.Out}, countLines(it.Out))
	data, hash, ok := r.readWorkspaceFile(rel)
	if ok && ref.Hash != hash {
		ref.Hash = hash
		ref.Outline = repomap.Outline(rel, data)
		ref.Ranges = []Range{rng}
		return
	}
	ref.Ranges = mergeRange(ref.Ranges, rng)
}

// rel maps a model-supplied path to the root-relative slash path evidence
// is keyed by, the same fold a relative read would produce for an absolute
// path inside the workspace. An absolute path outside the root returns "".
func (r *recorder) rel(p string) string {
	if strings.TrimSpace(p) == "" {
		return ""
	}
	if !filepath.IsAbs(p) {
		return relPath(p)
	}
	root, err := filepath.Abs(r.root)
	if err != nil {
		return ""
	}
	out, err := filepath.Rel(root, filepath.Clean(p))
	if err != nil || out == ".." || strings.HasPrefix(out, ".."+string(filepath.Separator)) {
		return ""
	}
	return relPath(out)
}

// readWorkspaceFile reads a root-relative path off disk; ok is false when
// it cannot be read (deleted, outside the sandbox visible to this process,
// or simply not on disk, which distill must tolerate rather than fail).
func (r *recorder) readWorkspaceFile(rel string) (data []byte, hash string, ok bool) {
	abs := filepath.Join(r.root, filepath.FromSlash(rel))
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, "", false
	}
	return data, hashBytes(data), true
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

func firstLine(s string) string {
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
