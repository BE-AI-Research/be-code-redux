package engine

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The task document is Markdown the user reads and edits: one heading, a
// nested list of nodes, and each node's evidence as sub-bullets under it.
//
//	# 001 — fix the parser
//
//	- [x] 1. fix the parser
//	  - [x] 1.1. find the bug
//	    - files: lexer.go (lines 1–120)
//	    - cmds: go test ./... — failed
//	    - error: FAIL: TestLex
//	  - [>] 1.2. fix and verify
//	  - [-] 1.3. rewrite the scanner — dropped: not needed after all
//
// Two rules govern everything here. The document is the user's file, so a
// line the engine does not understand is preserved verbatim rather than
// dropped; and a document the engine cannot understand at all is an error
// the caller quarantines, never something it half-parses and rewrites.

// statusMark and markStatus are one mapping read both ways, so a status can
// never render as a mark the parser would not read back.
var statusMark = map[Status]string{
	StatusTodo:    " ",
	StatusDoing:   ">",
	StatusDone:    "x",
	StatusBlocked: "!",
	StatusDropped: "-",
}

var markStatus = map[string]Status{
	" ": StatusTodo,
	">": StatusDoing,
	"x": StatusDone,
	"X": StatusDone,
	"!": StatusBlocked,
	"-": StatusDropped,
}

// indentStep is how many spaces one level of nesting costs.
const indentStep = 2

// nodeLine matches "  - [x] 1.2. text". The id is optional so a human can
// add a step without inventing one; the parser then fills it by position.
var nodeLine = regexp.MustCompile(`^([ \t]*)- \[(.)\] (?:((?:\d+\.)+)[ \t]+)?(.*)$`)

// evidenceLine matches "    - files: …" for the keys the engine writes.
var evidenceLine = regexp.MustCompile(`^([ \t]*)- (files|cmds|lookups|note|decision|error): (.*)$`)

// linesRange matches the "(lines 1–120, 200–240)" suffix of a files bullet.
var linesRange = regexp.MustCompile(`^(\d+)[–-](\d+)$`)

// RenderDoc writes a root node as a document.
func RenderDoc(num, title string, root *Node) string {
	return RenderDocWithExtra(num, title, root, nil)
}

// RenderDocWithExtra is RenderDoc plus the unrecognised lines a parse
// preserved, appended after the tree so a human's own prose survives a
// round trip through the engine.
func RenderDocWithExtra(num, title string, root *Node, extra []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — %s\n\n", num, title)
	if root != nil {
		renderNode(&b, root, 0)
	}
	if len(extra) > 0 {
		b.WriteByte('\n')
		b.WriteString(strings.Join(extra, "\n"))
		b.WriteByte('\n')
	}
	return b.String()
}

func renderNode(b *strings.Builder, n *Node, depth int) {
	ind := strings.Repeat(" ", depth*indentStep)
	mark, ok := statusMark[n.Status]
	if !ok {
		mark = " "
	}
	fmt.Fprintf(b, "%s- [%s] %s. %s", ind, mark, n.ID, n.Text)
	if n.Reason != "" && (n.Status == StatusBlocked || n.Status == StatusDropped) {
		fmt.Fprintf(b, " — %s: %s", n.Status, n.Reason)
	}
	b.WriteByte('\n')

	ev := ind + strings.Repeat(" ", indentStep)
	for _, f := range n.Evidence.Files {
		fmt.Fprintf(b, "%s- files: %s\n", ev, renderFile(f))
	}
	for _, c := range n.Evidence.Cmds {
		fmt.Fprintf(b, "%s- cmds: %s — %s\n", ev, c.Cmd, okWord(c.OK))
	}
	for _, l := range n.Evidence.Lookups {
		fmt.Fprintf(b, "%s- lookups: %s\n", ev, renderLookup(l))
	}
	for _, nt := range n.Evidence.Notes {
		key := "note"
		if nt.Decision {
			key = "decision"
		}
		line := nt.Text
		if nt.File != "" {
			line += " [file: " + nt.File + "]"
		}
		fmt.Fprintf(b, "%s- %s: %s\n", ev, key, line)
	}
	for _, e := range n.Evidence.Errors {
		fmt.Fprintf(b, "%s- error: %s\n", ev, e)
	}
	for _, c := range n.Children {
		renderNode(b, c, depth+1)
	}
}

func okWord(ok bool) string {
	if ok {
		return "ok"
	}
	return "failed"
}

// renderFile is "<path> (lines a–b, c–d) [edited] — <note>", every part
// after the path optional.
func renderFile(f FileRef) string {
	var b strings.Builder
	b.WriteString(f.Path)
	if len(f.Ranges) > 0 {
		rs := make([]string, 0, len(f.Ranges))
		for _, r := range f.Ranges {
			rs = append(rs, fmt.Sprintf("%d–%d", r.From, r.To))
		}
		fmt.Fprintf(&b, " (lines %s)", strings.Join(rs, ", "))
	}
	if f.Edited {
		b.WriteString(" [edited]")
	}
	if f.Note != "" {
		b.WriteString(" — " + f.Note)
	}
	return b.String()
}

func parseFile(s string) FileRef {
	var f FileRef
	// The note is separated by " — "; a path never contains that, and the
	// first occurrence wins so a note may contain one of its own.
	if i := strings.Index(s, " — "); i >= 0 {
		f.Note = strings.TrimSpace(s[i+len(" — "):])
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if strings.HasSuffix(s, " [edited]") {
		f.Edited = true
		s = strings.TrimSpace(strings.TrimSuffix(s, " [edited]"))
	}
	if i := strings.LastIndex(s, " (lines "); i >= 0 && strings.HasSuffix(s, ")") {
		body := s[i+len(" (lines ") : len(s)-1]
		s = s[:i]
		for _, part := range strings.Split(body, ",") {
			m := linesRange.FindStringSubmatch(strings.TrimSpace(part))
			if m == nil {
				continue
			}
			from, _ := strconv.Atoi(m[1])
			to, _ := strconv.Atoi(m[2])
			f.Ranges = append(f.Ranges, Range{From: from, To: to})
		}
	}
	f.Path = relPath(strings.TrimSpace(s))
	return f
}

// renderLookup is "<tool> <query> → a.go:12, b.go:3".
func renderLookup(l LookupRef) string {
	s := strings.TrimSpace(l.Tool + " " + l.Query)
	if len(l.Hits) == 0 {
		return s
	}
	refs := make([]string, 0, len(l.Hits))
	for _, h := range l.Hits {
		refs = append(refs, fmt.Sprintf("%s:%d", h.File, h.Line))
	}
	return s + " → " + strings.Join(refs, ", ")
}

func parseLookup(s string) LookupRef {
	var l LookupRef
	if i := strings.Index(s, " → "); i >= 0 {
		for _, ref := range strings.Split(s[i+len(" → "):], ",") {
			ref = strings.TrimSpace(ref)
			j := strings.LastIndexByte(ref, ':')
			if j < 0 {
				continue
			}
			n, err := strconv.Atoi(ref[j+1:])
			if err != nil {
				continue
			}
			l.Hits = append(l.Hits, Hit{File: ref[:j], Line: n})
		}
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, ' '); i >= 0 {
		l.Tool, l.Query = s[:i], strings.TrimSpace(s[i+1:])
	} else {
		l.Tool = s
	}
	return l
}

func parseNote(s string, decision bool) NoteRef {
	n := NoteRef{Decision: decision}
	if i := strings.LastIndex(s, " [file: "); i >= 0 && strings.HasSuffix(s, "]") {
		n.File = relPath(s[i+len(" [file: ") : len(s)-1])
		s = s[:i]
	}
	n.Text = strings.TrimSpace(s)
	return n
}

// ParseDoc reads a task document. It returns the tree, the lines it did not
// recognise (preserved verbatim so a human's own prose survives a round
// trip), and an error only when the document is structurally broken — the
// caller quarantines rather than half-parsing.
func ParseDoc(text string) (*Tree, []string, error) {
	tr := &Tree{}
	var extra []string
	var stack []*Node // stack[d] is the node currently open at depth d
	var last *Node
	lastDepth := -1
	sawHeading := false
	seen := map[string]bool{}
	now := time.Now()

	lines := strings.Split(text, "\n")
	// A file ending in a newline splits into a trailing empty element that
	// is not a line of the document.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if !sawHeading && strings.HasPrefix(trimmed, "# ") {
			sawHeading = true
			continue
		}
		if m := nodeLine.FindStringSubmatch(line); m != nil {
			st, ok := markStatus[m[2]]
			if !ok {
				return nil, nil, fmt.Errorf("line %d: unknown status mark %q", i+1, m[2])
			}
			depth := indentWidth(m[1]) / indentStep
			if depth > lastDepth+1 {
				return nil, nil, fmt.Errorf("line %d: a step is indented under nothing", i+1)
			}
			text, reason := splitReason(strings.TrimSpace(m[4]))
			n := &Node{Text: text, Status: st, Reason: reason, Opened: now}
			if st.terminal() {
				n.Closed = now
			}
			var want string
			if depth == 0 {
				tr.Roots = append(tr.Roots, n)
				want = strconv.Itoa(len(tr.Roots))
			} else {
				p := stack[depth-1]
				p.Children = append(p.Children, n)
				want = p.ID + "." + strconv.Itoa(len(p.Children))
			}
			// Ids are read from the document, but position is what the tree
			// is built on: a missing, wrong or duplicate id is repaired to
			// the node's own position, and the repair is noted so the user
			// can see the engine disagreed with them.
			got := strings.TrimSuffix(m[3], ".")
			if got != "" && (got != want || seen[got]) {
				n.Evidence.Notes = append(n.Evidence.Notes,
					NoteRef{Text: fmt.Sprintf("id repaired from %s to %s", got, want)})
			}
			n.ID = want
			seen[want] = true
			stack = append(stack[:depth], n)
			lastDepth, last = depth, n
			continue
		}
		if m := evidenceLine.FindStringSubmatch(line); m != nil && last != nil {
			addEvidence(last, m[2], strings.TrimSpace(m[3]))
			continue
		}
		extra = append(extra, line)
	}
	return tr, extra, nil
}

// indentWidth counts a tab as one indent step, so a tab-indented document
// nests the way its author meant it to.
func indentWidth(s string) int {
	n := 0
	for _, r := range s {
		if r == '\t' {
			n += indentStep
			continue
		}
		n++
	}
	return n
}

// splitReason peels " — blocked: why" / " — dropped: why" off a node's text.
func splitReason(s string) (text, reason string) {
	for _, word := range []Status{StatusBlocked, StatusDropped} {
		sep := " — " + string(word) + ": "
		if i := strings.Index(s, sep); i >= 0 {
			return strings.TrimSpace(s[:i]), strings.TrimSpace(s[i+len(sep):])
		}
	}
	return s, ""
}

func addEvidence(n *Node, key, val string) {
	switch key {
	case "files":
		n.Evidence.Files = append(n.Evidence.Files, parseFile(val))
	case "cmds":
		c := CmdRef{Cmd: val, OK: true}
		if i := strings.LastIndex(val, " — "); i >= 0 {
			switch strings.TrimSpace(val[i+len(" — "):]) {
			case "ok":
				c = CmdRef{Cmd: strings.TrimSpace(val[:i]), OK: true}
			case "failed":
				c = CmdRef{Cmd: strings.TrimSpace(val[:i]), OK: false}
			}
		}
		n.Evidence.Cmds = append(n.Evidence.Cmds, c)
	case "lookups":
		n.Evidence.Lookups = append(n.Evidence.Lookups, parseLookup(val))
	case "note":
		n.Evidence.Notes = append(n.Evidence.Notes, parseNote(val, false))
	case "decision":
		n.Evidence.Notes = append(n.Evidence.Notes, parseNote(val, true))
	case "error":
		n.Evidence.Errors = append(n.Evidence.Errors, val)
	}
}
