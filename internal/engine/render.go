package engine

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Render builds the Working memory: block the system prompt carries. In
// order: a Task Report (report.go) for every terminal root, oldest first;
// the active branch — the path from the top-level task to the doing node,
// with sibling statuses so what remains is visible; the doing node's raw
// buffer, verbatim, because that is the lossless part; then the durable
// notes. Ruling T4-b: when nothing is doing (a task planned but not yet
// started), the active section falls back to the newest root that is not
// wholly finished, showing its own status and its children — that is
// exactly when the model most needs to see the plan it just made. inMap
// reports whether the repository map already lists a file's symbols, so the
// active node's own file listing does not repeat an outline the model has
// already been shown.
//
// The budget is a ladder, not a cliff: composed at full detail, measured,
// and while it is over budget the oldest report condenses one rung —
// buildReport (full) -> reportHeadline -> reportOneLine -> reportPointer —
// before the next-oldest report starts condensing. The active branch and
// its verbatim step are never a rung; they are the work in flight. If
// every report has condensed all the way to its pointer and the block
// still does not fit, whole reports are dropped oldest-first, and if even
// the single newest one still does not fit beside the active branch it is
// trimmed to what remains rather than dropped outright.
func (s *Store) Render(budget int, inMap func(string) bool) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	var roots []*Node
	for _, r := range s.tree.Roots {
		// A spent unfiled root is a harness artefact, not work: it would
		// otherwise report itself as "unfiled — dropped: adopted by 3.1",
		// a phantom task in the model's own picture of what it has done.
		if s.tree.Terminal(r) && !spentUnfiled(r) {
			roots = append(roots, r)
		}
	}
	tail := joinBlock(activeBranchText(&s.tree, inMap), strings.TrimRight(s.notes, "\n"))
	if len(roots) == 0 {
		return tail
	}

	rung := make([]int, len(roots))
	reports := make([]string, len(roots))
	for i, r := range roots {
		reports[i] = rungText(r, rung[i])
	}
	compose := func(reports []string) string {
		return joinBlock(strings.Join(reports, "\n\n"), tail)
	}

	out := compose(reports)
	for len(out) > budget {
		i := -1
		for j := range rung {
			if rung[j] < maxRung {
				i = j
				break
			}
		}
		if i < 0 {
			break
		}
		rung[i]++
		// Only the report that just condensed needs re-rendering; every
		// other report's text is unchanged by this step.
		reports[i] = rungText(roots[i], rung[i])
		out = compose(reports)
	}
	if len(out) <= budget {
		return out
	}

	// Every report is at its floor and the block still does not fit. The
	// active branch stays whole regardless — it is never what gets cut —
	// so what gives is the reports, oldest first, dropped entirely down to
	// the single newest one; if even that alone does not fit beside the
	// active branch, it is trimmed rather than dropped, so something of the
	// most recent finished work survives over nothing at all.
	for len(reports) > 1 && len(out) > budget {
		reports = reports[1:]
		out = compose(reports)
	}
	if len(out) > budget && len(reports) == 1 {
		avail := budget - len(tail)
		if tail != "" {
			avail -= 2 // the blank line joinBlock puts between sections
		}
		// trimLines treats budget<=0 as "no limit", so a non-positive avail
		// has to drop the report outright rather than call it.
		if avail <= 0 {
			reports = nil
		} else if trimmed := trimLines(reports[0], avail); trimmed != "" {
			reports[0] = trimmed
		} else {
			reports = nil
		}
		out = compose(reports)
	}
	return strings.TrimRight(out, "\n") + "\n(reports condensed)"
}

// TreeText is the /task listing: the whole tree, exactly as ShowText("").
func (s *Store) TreeText() string {
	return s.ShowText("")
}

// activeBranchText renders the path from a root down to the doing node,
// with sibling statuses at each level, closed by the doing node's raw
// buffer verbatim (the lossless part) and, when it has read a file, that
// file's current listing. When nothing is doing at all, ruling T4-b's
// fallback applies: the newest root that is not wholly finished renders its
// own status and its children, with no doing node to descend into and
// nothing verbatim to show — a plan the model made but has not started yet
// is exactly when it most needs to see it.
func activeBranchText(t *Tree, inMap func(string) bool) string {
	path := t.ActiveBranch()
	if len(path) == 0 {
		r := newestOpenRoot(t)
		if r == nil {
			return ""
		}
		var b strings.Builder
		b.WriteString("Active task:\n")
		fmt.Fprintf(&b, "%s\n", statusLine(r))
		for _, c := range r.Children {
			fmt.Fprintf(&b, "  %s\n", statusLine(c))
		}
		return strings.TrimRight(b.String(), "\n")
	}
	var b strings.Builder
	b.WriteString("Active task:\n")
	fmt.Fprintf(&b, "%s\n", statusLine(path[0]))
	for depth := 1; depth < len(path); depth++ {
		for _, c := range path[depth-1].Children {
			fmt.Fprintf(&b, "%s%s\n", strings.Repeat("  ", depth), statusLine(c))
		}
	}
	doing := path[len(path)-1]
	if raw := rawBlock(doing.Evidence.Raw, doing.Evidence.Dropped); raw != "" {
		b.WriteString("\n" + raw + "\n")
	}
	if files := activeFilesBlock(doing.Evidence.Files, inMap); files != "" {
		b.WriteString("\n" + files + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// newestOpenRoot is the last root that is not wholly finished — the same
// definition of "the active task" store.go's own activeRootLocked uses.
// Ruling T4-b: it is what the active section falls back to when nothing is
// doing, so a task the model has planned but not started still renders.
func newestOpenRoot(t *Tree) *Node {
	for i := len(t.Roots) - 1; i >= 0; i-- {
		if !t.Terminal(t.Roots[i]) {
			return t.Roots[i]
		}
	}
	return nil
}

// rawBlock renders a node's verbatim buffer: every tool call still in
// flight, exactly as the model saw it. This is what "the doing node
// verbatim" means — it is never condensed and never summarised. dropped is
// Evidence.Dropped, the count of raw items the node cap already discarded;
// a node whose buffer was capped is never allowed to read as complete.
func rawBlock(items []RawItem, dropped int) string {
	if len(items) == 0 && dropped == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("raw:\n")
	for _, it := range items {
		fmt.Fprintf(&b, "  %s %s\n", it.Tool, it.Args)
		out := strings.TrimRight(it.Out, "\n")
		if out == "" {
			continue
		}
		for _, line := range strings.Split(out, "\n") {
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}
	if dropped > 0 {
		fmt.Fprintf(&b, "  (%d item(s) dropped)\n", dropped)
	}
	return strings.TrimRight(b.String(), "\n")
}

// activeFilesBlock lists the doing node's own file references — the
// analogue of the 0.10.0 digest row, and the one place an outline still
// appears, suppressed by inMap exactly as it was there. A finished report
// never carries an outline: it is a fixed rollup of what happened, not a
// live symbol map, and repeating it there would only fight the repository
// map for the same bytes.
func activeFilesBlock(files []FileRef, inMap func(string) bool) string {
	if len(files) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("files:\n")
	for _, f := range files {
		b.WriteString("  " + renderFile(f))
		if len(f.Outline) > 0 && (inMap == nil || !inMap(f.Path)) {
			b.WriteString("\n    outline: " + strings.Join(f.Outline, ", "))
		}
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}

// joinBlock joins non-empty parts with a blank line, the way every section
// of the working-memory block is stitched together.
func joinBlock(parts ...string) string {
	var out []string
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, strings.TrimRight(p, "\n"))
		}
	}
	return strings.Join(out, "\n\n")
}

// trimLines cuts s to at most budget bytes at a line boundary, or (when no
// newline lies inside the budget) at the last full UTF-8 rune.
func trimLines(s string, budget int) string {
	if budget <= 0 || len(s) <= budget {
		return s
	}
	cut := s[:budget]
	if nl := strings.LastIndexByte(cut, '\n'); nl > 0 {
		cut = cut[:nl]
	} else {
		for len(cut) > 0 {
			if r, size := utf8.DecodeLastRuneInString(cut); r != utf8.RuneError || size != 1 {
				break
			}
			cut = cut[:len(cut)-1]
		}
	}
	return strings.TrimRight(cut, "\n")
}

// ledgerLocked and legacyStatus are the last of the 0.10.0 flat
// projection: LedgerText below is the only thing that still reads them,
// and Task 6 retires that call site along with these.
func (s *Store) ledgerLocked() Ledger {
	l := Ledger{Baseline: s.baseline, Session: s.session}
	// No open task means no task line: with every root finished — or
	// dropped by "/task clear" — the old block has nothing to describe.
	r := s.activeRootLocked()
	if r == nil {
		return l
	}
	l.Task = r.Text
	for _, c := range r.Children {
		if c.Text == unfiledText {
			continue
		}
		l.Steps = append(l.Steps, Step{Text: c.Text, Status: legacyStatus(c.Status)})
	}
	var walk func(n *Node)
	walk = func(n *Node) {
		for _, nt := range n.Evidence.Notes {
			if nt.Decision {
				l.Decisions = append(l.Decisions, nt.Text)
			} else {
				l.Facts = append(l.Facts, nt.Text)
			}
		}
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(r)
	return l
}

// legacyStatus maps a node status onto the four words the old ledger knew.
// blocked has no old equivalent; it reads as skip, which is at least
// terminal, and Task 4's report is where the distinction comes back.
func legacyStatus(st Status) string {
	switch st {
	case StatusDoing:
		return "doing"
	case StatusDone:
		return "done"
	case StatusDropped, StatusBlocked:
		return "skip"
	}
	return "todo"
}

// LedgerText is the 0.10.0 /task listing, kept only because the TUI and the
// plain REPL still call it (internal/tui/view.go, internal/ui/repl.go);
// Task 6 replaces both call sites with TreeText/ShowText via ui.TaskLines,
// at which point this and the projection above can go.
func (s *Store) LedgerText() string {
	s.mu.Lock()
	l := s.ledgerLocked()
	s.mu.Unlock()
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

var fileNoteLine = regexp.MustCompile(`(?m)^\s*(?:-\s*)?([^\s—]+)\s+—\s+(.+)$`)

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

// summaryKeyPriority is the order of preference for the "query" a lookup
// row shows: LookupKey sorts arguments alphabetically, so an unqualified
// pick (the first quoted value) would show "glob" ahead of "pattern" for a
// search called with both.
var summaryKeyPriority = []string{"query", "pattern", "regex", "symbol", "text", "term"}

// summariseKey turns the canonical key back into a readable "query" — used
// by the migration (migrate.go) to translate a 0.10.0 lookup's key into the
// tree's LookupRef.Query.
func summariseKey(k string) string {
	values := map[string]string{}
	first := ""
	for _, part := range strings.Split(k, ";") {
		i := strings.Index(part, "=\"")
		if i < 0 || !strings.HasSuffix(part, "\"") {
			continue
		}
		name, val := part[:i], part[i+1:] // val keeps its surrounding quotes
		values[name] = val
		if first == "" {
			first = val
		}
	}
	for _, name := range summaryKeyPriority {
		if v, ok := values[name]; ok {
			return v
		}
	}
	if first != "" {
		return first
	}
	return `""`
}
