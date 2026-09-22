package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"
)

// DefaultBudget is the block's byte cap when the caller names none.
const DefaultBudget = 6144

// condensedMarker closes a block whose finished reports were dropped or cut.
const condensedMarker = "(reports condensed)"

// minNewestRaw is the least the newest raw item's output is ever cut to: the
// last thing the model did is the one piece of verbatim evidence the block
// always carries.
const minNewestRaw = 256

// maxChangedCheck bounds the file the [changed since read] check will hash.
const maxChangedCheck = 4 << 20

// Render builds the Working memory: block the system prompt carries. In
// order: a Task Report (report.go) for every terminal root, oldest first;
// the active branch — the path from the top-level task to the doing node,
// with sibling statuses so what remains is visible; what the doing node has
// done, the newest of it verbatim; its files; then the durable notes. Ruling
// T4-b: when nothing is doing (a task planned but not yet started), the
// active section falls back to the newest root that is not wholly finished,
// showing its own status and its children — that is exactly when the model
// most needs to see the plan it just made. inMap reports whether the
// repository map already lists a file's symbols, so the active node's own
// file listing does not repeat an outline the model has already been shown.
//
// **The budget wins (ruling F-1).** The whole block is at most budget bytes,
// whatever the tree holds. The spec once promised both a 32 KiB verbatim
// node and a 6144-byte block, and kept the first: a model that never calls
// task leaves everything on one unfiled node that stays doing all session,
// so the block reached 36 KB, three quarters of a 16k window, and the engine
// built to survive compaction was what forced it. The budget is a ladder,
// not a cliff, and each rung gives up the least valuable thing left:
//
//  1. finished reports condense oldest first — full, headline, one line,
//     pointer (report.go);
//  2. the doing node's raw items turn into distilled one-liners, oldest
//     first, until only the newest is verbatim;
//  3. file outlines go;
//  4. reports are dropped oldest first, the last one cut rather than
//     dropped when the rest already fits;
//  5. durable notes lose their oldest lines;
//  6. the newest raw item's output is cut, down to a third of the budget;
//  7. the distilled one-liners lose their oldest lines;
//  8. file rows lose their oldest, then the last notes go;
//  9. the newest raw item is cut again, never below minNewestRaw.
//
// Nothing is lost by any of it: the full verbatim buffer stays in the state
// file, and `task show <id>` prints it.
//
// The tree is copied under the lock and rendered outside it, because the
// [changed since read] marker has to look at the disk.
func (s *Store) Render(budget int, inMap func(string) bool) string {
	if budget <= 0 {
		budget = DefaultBudget
	}
	s.mu.Lock()
	t := Tree{Roots: copyNodes(s.tree.Roots), nudge: s.lim.StepNudge}
	// A copy of the dispatched set, not the live map: Render runs outside
	// the lock, and every other part of this copy — the nodes above — is
	// already its own, never shared with the live tree.
	if len(s.tree.dispatched) > 0 {
		t.dispatched = make(map[string]bool, len(s.tree.dispatched))
		for k, v := range s.tree.dispatched {
			t.dispatched[k] = v
		}
	}
	for _, r := range s.tree.Dispatched() {
		if n := t.Find(r); n != nil {
			n.Dispatched = true
			if d := t.DoingUnder(r); d != nil {
				n.DispatchedAt = d.ID
			}
			n.Calls = subtreeCalls(n)
		}
	}
	notes := strings.TrimRight(s.notes, "\n")
	root := s.root
	s.mu.Unlock()
	return renderBlock(&t, notes, budget, inMap, func(f FileRef) bool { return changedSinceRead(root, f) })
}

// subtreeCalls sums Calls over a subtree, for the running line.
func subtreeCalls(n *Node) int {
	total := n.Calls
	for _, c := range n.Children {
		total += subtreeCalls(c)
	}
	return total
}

// changedSinceRead reports whether a file the record describes no longer has
// the content it described. A reference with no hash describes nothing in
// particular, so it is never stale; a file that has gone is.
func changedSinceRead(root string, f FileRef) bool {
	if f.Hash == "" || f.Path == "" {
		return false
	}
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(f.Path)))
	if err != nil || info.IsDir() {
		return true
	}
	if info.Size() > maxChangedCheck {
		return false
	}
	_, hash, _, _, ok := fileState(root, f.Path)
	return !ok || hash != f.Hash
}

func renderBlock(t *Tree, notes string, budget int, inMap func(string) bool, changed func(FileRef) bool) string {
	var roots []*Node
	for _, r := range t.Roots {
		// A spent unfiled root is a harness artefact, not work: it would
		// otherwise report itself as "unfiled — dropped: adopted by 3.1",
		// a phantom task in the model's own picture of what it has done.
		if t.Terminal(r) && !spentUnfiled(r) {
			roots = append(roots, r)
		}
	}
	ap := newActiveParts(t, inMap, changed)
	opts := ap.full()
	rung := make([]int, len(roots))
	reports := make([]string, len(roots))
	for i, r := range roots {
		reports[i] = rungText(r, rung[i])
	}
	noteLines := []string(nil)
	if strings.TrimSpace(notes) != "" {
		noteLines = strings.Split(notes, "\n")
	}
	condensed := false

	compose := func() string {
		out := joinBlock(strings.Join(reports, "\n\n"), ap.text(opts), strings.Join(noteLines, "\n"))
		if condensed {
			out = strings.TrimRight(out, "\n") + "\n" + condensedMarker
		}
		return out
	}
	over := func() bool { return len(compose()) > budget }

	// 1. The ladder: the oldest report condenses one rung at a time before
	// the next-oldest starts.
	for over() {
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
		reports[i] = rungText(roots[i], rung[i])
	}
	// 2. Raw items become one-liners, oldest first; the newest stays whole.
	for over() && opts.verbatim > 1 {
		opts.verbatim--
	}
	// 3. Outlines.
	if over() {
		opts.outlines = false
	}
	// 4. Reports go, oldest first. The last is cut to fit rather than
	// dropped, so something of the most recent finished work survives — but
	// only when everything else already fits, or it would be squeezing the
	// work in flight to keep a sliver of history.
	for over() && len(reports) > 1 {
		reports = reports[1:]
		condensed = true
	}
	if over() && len(reports) == 1 {
		condensed = true
		last := reports[0]
		reports = nil
		if avail := budget - len(compose()) - 2; avail > 0 {
			// trimLines treats budget<=0 as "no limit", hence the guard.
			if cut := trimLines(last, avail); cut != "" {
				reports = []string{cut}
			}
		}
	}
	// 5. Durable notes, oldest lines first, down to the newest few.
	for over() && len(noteLines) > 3 {
		noteLines = noteLines[1:]
	}
	// 6. The newest raw item gives up its tail, down to a third of the
	// budget: one large read is worth less than every line that says what
	// else was done and every file row that says what has been seen.
	capNewest := func(floor int) {
		if !over() || len(ap.rawItems()) == 0 {
			return
		}
		newest := ap.raw[len(ap.raw)-1]
		size := len(newest.Out)
		if opts.newestCap > 0 && opts.newestCap < size {
			size = opts.newestCap
		}
		want := size - (len(compose()) - budget) - len(truncatedNote) - 8
		if want < floor {
			want = floor
		}
		if want < size {
			opts.newestCap = want
		}
	}
	capNewest(budget / 3)
	// 7. The distilled lines, oldest first.
	for over() && opts.hidden < ap.distilledLen(opts) {
		opts.hidden++
	}
	// 8. File rows, oldest first, then what is left of the notes.
	for over() && opts.hideFiles < len(ap.fileRows()) {
		opts.hideFiles++
	}
	for over() && len(noteLines) > 0 {
		noteLines = noteLines[1:]
	}
	// 9. The newest raw item again, to its floor.
	capNewest(minNewestRaw)
	out := compose()
	if len(out) > budget {
		// Only a branch path longer than the whole budget gets here. The cap
		// still holds.
		out = trimLines(out, budget)
	}
	return out
}

// TreeText is the /task listing: the whole tree, exactly as ShowText("").
func (s *Store) TreeText() string {
	return s.ShowText("")
}

// activeParts is the active section taken apart, so Render can give up one
// piece of it at a time.
type activeParts struct {
	id       string   // the doing node, "" when nothing is doing
	header   string   // "Active task:" and the path with sibling statuses
	recorded []string // the doing node's durable record, one line each
	raw      []RawItem
	dropped  int
	files    []FileRef
	stale    map[string]bool
	inMap    func(string) bool
}

// activeOpts is how much of the active section one render shows.
type activeOpts struct {
	verbatim  int  // how many of the newest raw items render in full
	hidden    int  // how many of the oldest distilled lines are left out
	outlines  bool // file outlines
	hideFiles int  // how many of the oldest file rows are left out
	newestCap int  // byte cap on the newest raw item's output; 0 for none
}

// rawItems and fileRows are nil-safe reads for Render's ladder.
func (p *activeParts) rawItems() []RawItem {
	if p == nil {
		return nil
	}
	return p.raw
}

func (p *activeParts) fileRows() []FileRef {
	if p == nil {
		return nil
	}
	return p.files
}

func (p *activeParts) full() activeOpts {
	if p == nil {
		return activeOpts{}
	}
	return activeOpts{verbatim: len(p.raw), outlines: true}
}

// distilledLen is how many distilled lines a render at o would show in full:
// the durable record plus every raw item that is not verbatim.
func (p *activeParts) distilledLen(o activeOpts) int {
	if p == nil {
		return 0
	}
	return len(p.recorded) + len(p.raw) - o.verbatim
}

// newActiveParts reads the active section off the tree: the path from a root
// down to the doing node, with sibling statuses at each level, and the doing
// node's evidence. When nothing is doing at all, ruling T4-b's fallback
// applies: the newest root that is not wholly finished renders its own
// status and its children, with no doing node to descend into — a plan the
// model made but has not started yet is exactly when it most needs to see
// it.
func newActiveParts(t *Tree, inMap func(string) bool, changed func(FileRef) bool) *activeParts {
	path := t.ActiveBranch()
	var b strings.Builder
	if len(path) == 0 {
		r := newestOpenRoot(t)
		if r == nil {
			return nil
		}
		b.WriteString("Active task:\n")
		fmt.Fprintf(&b, "%s\n", statusLine(r))
		for _, c := range r.Children {
			fmt.Fprintf(&b, "  %s\n", statusLine(c))
		}
		return &activeParts{header: strings.TrimRight(b.String(), "\n")}
	}
	b.WriteString("Active task:\n")
	fmt.Fprintf(&b, "%s\n", statusLine(path[0]))
	for depth := 1; depth < len(path); depth++ {
		for _, c := range path[depth-1].Children {
			fmt.Fprintf(&b, "%s%s\n", strings.Repeat("  ", depth), statusLine(c))
		}
	}
	doing := path[len(path)-1]
	// Part of the header, so it is never a rung of the budget ladder: the
	// step it is about is the one thing the block always keeps.
	if calls := len(doing.Evidence.Raw) + doing.Evidence.Dropped; t.nudge > 0 && calls > t.nudge {
		b.WriteString(nudgeLine(doing.ID, calls) + "\n")
	}
	p := &activeParts{
		id:      doing.ID,
		header:  strings.TrimRight(b.String(), "\n"),
		raw:     doing.Evidence.Raw,
		dropped: doing.Evidence.Dropped,
		files:   doing.Evidence.Files,
		stale:   map[string]bool{},
		inMap:   inMap,
	}
	for _, nt := range doing.Evidence.Notes {
		key := "note"
		if nt.Decision {
			key = "decision"
		}
		p.recorded = append(p.recorded, key+": "+oneLine(nt.Text, distilledWidth))
	}
	for _, c := range doing.Evidence.Cmds {
		p.recorded = append(p.recorded, oneLine(c.Cmd, distilledWidth)+" — "+okWord(c.OK))
	}
	for _, l := range doing.Evidence.Lookups {
		p.recorded = append(p.recorded, fmt.Sprintf("%s %s → %d hit(s)", l.Tool, oneLine(l.Query, distilledWidth), len(l.Hits)))
	}
	for _, e := range doing.Evidence.Errors {
		p.recorded = append(p.recorded, "error: "+oneLine(e, 2*distilledWidth))
	}
	if changed != nil {
		for _, f := range p.files {
			if changed(f) {
				p.stale[f.Path] = true
			}
		}
	}
	return p
}

// nudgeEvery is how often the mid-run nudge repeats once a step is past its
// threshold: often enough to be seen, not on every call.
const nudgeEvery = 10

// StepNudge is the long-open-step line for the end of a tool result, or "".
// The Working memory block carries the same line in its header, but the block
// is only attached when a request begins or the history is rewritten; a step
// that runs long does so in between, and this is how the model hears of it.
// It fires on the first call past the threshold and every nudgeEvery after.
func (s *Store) StepNudge() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.tree.Doing()
	if d == nil || s.lim.StepNudge <= 0 {
		return ""
	}
	calls := len(d.Evidence.Raw) + d.Evidence.Dropped
	if over := calls - s.lim.StepNudge; over < 1 || (over-1)%nudgeEvery != 0 {
		return ""
	}
	return nudgeLine(d.ID, calls)
}

func nudgeLine(id string, calls int) string {
	return fmt.Sprintf("! step %s has been open for %d tool calls: finish it, split it into smaller steps (task add, parent %s), or note why it is taking this long", id, calls, id)
}

// distilledWidth bounds the argument half of a distilled line.
const distilledWidth = 120

// oneLine is a text's first line, bounded, for a distilled row.
func oneLine(s string, max int) string {
	line := firstLine(s, max)
	if len(line) < len(strings.TrimSpace(s)) {
		line += " …"
	}
	return line
}

// distilledLine is one raw item the way distillation will eventually record
// it: what was called and how it came out, without the output.
func distilledLine(it RawItem) string {
	line := strings.TrimSpace(it.Tool + " " + oneLine(it.Args, distilledWidth))
	switch it.Tool {
	case "search", "lookup", "history":
		if it.OK {
			return fmt.Sprintf("%s → %d hit(s)", line, len(parseHits(it.Out)))
		}
	}
	if it.OK {
		return line + " — ok"
	}
	first := firstOutputLine(it.Out)
	if first == "" {
		first = "(no output)"
	}
	return line + " — failed: " + oneLine(first, distilledWidth)
}

// text renders the active section at o.
func (p *activeParts) text(o activeOpts) string {
	if p == nil {
		return ""
	}
	var b strings.Builder
	b.WriteString(p.header + "\n")
	if p.id == "" {
		return strings.TrimRight(b.String(), "\n")
	}

	verbatim := o.verbatim
	if verbatim > len(p.raw) {
		verbatim = len(p.raw)
	}
	if verbatim < 0 {
		verbatim = 0
	}
	distilled := append([]string(nil), p.recorded...)
	for _, it := range p.raw[:len(p.raw)-verbatim] {
		distilled = append(distilled, distilledLine(it))
	}
	if len(distilled) > 0 {
		// The pointer is only worth its bytes when a verbatim item was
		// actually turned into a line here.
		if verbatim < len(p.raw) {
			fmt.Fprintf(&b, "\nearlier (distilled; task show %s prints the buffer in full):\n", p.id)
		} else {
			b.WriteString("\nearlier:\n")
		}
		hidden := o.hidden
		if hidden > len(distilled) {
			hidden = len(distilled)
		}
		if hidden > 0 {
			fmt.Fprintf(&b, "  (%d earlier line(s) not shown)\n", hidden)
		}
		for _, line := range distilled[hidden:] {
			fmt.Fprintf(&b, "  - %s\n", line)
		}
	}

	shown := p.raw[len(p.raw)-verbatim:]
	if len(shown) > 0 || p.dropped > 0 {
		b.WriteString("\nraw:\n")
		for i, it := range shown {
			out := strings.TrimRight(it.Out, "\n")
			if o.newestCap > 0 && i == len(shown)-1 {
				out = excerpt(out, o.newestCap)
			}
			fmt.Fprintf(&b, "  %s %s\n", it.Tool, it.Args)
			if out == "" {
				continue
			}
			for _, line := range strings.Split(out, "\n") {
				fmt.Fprintf(&b, "    %s\n", line)
			}
		}
		if p.dropped > 0 {
			fmt.Fprintf(&b, "  (%d item(s) dropped)\n", p.dropped)
		}
	}

	if len(p.files) > 0 {
		b.WriteString("\nfiles:\n")
		hide := o.hideFiles
		if hide > len(p.files) {
			hide = len(p.files)
		}
		if hide > 0 {
			fmt.Fprintf(&b, "  (%d earlier file(s) not shown)\n", hide)
		}
		for _, f := range p.files[hide:] {
			b.WriteString("  " + renderFile(f))
			if p.stale[f.Path] {
				b.WriteString(" " + changedMarker)
			}
			if o.outlines && len(f.Outline) > 0 && (p.inMap == nil || !p.inMap(f.Path)) {
				b.WriteString("\n    outline: " + strings.Join(f.Outline, ", "))
			}
			b.WriteByte('\n')
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// changedMarker is what the prompt's guidance points at: "do not read those
// files again unless they are marked changed".
const changedMarker = "[changed since read]"

// truncatedNote is what excerpt appends when it cuts.
const truncatedNote = "\n… (truncated)"

// activeBranchText is the active section at full detail.
func activeBranchText(t *Tree, inMap func(string) bool) string {
	p := newActiveParts(t, inMap, nil)
	return p.text(p.full())
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

// rawBlock renders a node's whole verbatim buffer, exactly as the model saw
// it. The prompt block no longer prints it this way — it is budgeted there
// (ruling F-1) — but `task show <id>` does, which is what keeps the lossless
// part reachable. dropped is Evidence.Dropped, the count of raw items the
// node cap already distilled and discarded; a node whose buffer was capped
// is never allowed to read as complete.
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
