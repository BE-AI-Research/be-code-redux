package engine

import (
	"fmt"
	"strings"
)

// This file is the deterministic half of the prompt block: a finished
// branch rolled up into fixed prose, built only from the record so it can
// never disagree with what actually happened. Nothing here calls a model.
//
// Report renders full detail. Render's condensation ladder (render.go)
// steps a report down through three shorter rungs — reportHeadline,
// reportOneLine, reportPointer — before giving up on it, oldest report
// first, so the work in flight is never what gets cut.

// Report is a finished branch rolled up: the task line and its status, one
// line per child with its status and reason, the files touched (with
// ranges and the edited flag), the commands that succeeded, the decisions
// made, the errors hit, and what was left blocked or dropped. Sections are
// omitted when empty. It is built from the tree alone — never a model — so
// the same store renders the same bytes every time.
func (s *Store) Report(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.tree.Find(id)
	if n == nil {
		return "no node " + id
	}
	return buildReport(n)
}

// buildReport is Report's full-detail rung (rung 0).
func buildReport(n *Node) string {
	var b strings.Builder
	b.WriteString(statusLine(n) + "\n")
	for _, c := range n.Children {
		fmt.Fprintf(&b, "  %s\n", statusLine(c))
	}
	ev := collectEvidence(n)
	if len(ev.files) > 0 {
		b.WriteString("files:\n")
		for _, f := range ev.files {
			fmt.Fprintf(&b, "  %s\n", renderFile(f))
		}
	}
	if len(ev.cmdsOK) > 0 {
		b.WriteString("cmds:\n")
		for _, c := range ev.cmdsOK {
			fmt.Fprintf(&b, "  %s — ok\n", c.Cmd)
		}
	}
	if len(ev.decisions) > 0 {
		b.WriteString("decisions:\n")
		for _, d := range ev.decisions {
			fmt.Fprintf(&b, "  - %s\n", d)
		}
	}
	if len(ev.errors) > 0 {
		b.WriteString("errors:\n")
		for _, e := range ev.errors {
			fmt.Fprintf(&b, "  - %s\n", e)
		}
	}
	if left := leftLines(n); left != "" {
		b.WriteString("left:\n" + left + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// reportHeadline is rung 1: the headline plus each child's outcome and the
// decisions made — everything a reader needs to trust the summary, none of
// the raw evidence (files, cmds, errors) that earned it.
func reportHeadline(n *Node) string {
	var b strings.Builder
	b.WriteString(statusLine(n) + "\n")
	for _, c := range n.Children {
		fmt.Fprintf(&b, "  %s\n", statusLine(c))
	}
	ev := collectEvidence(n)
	if len(ev.decisions) > 0 {
		b.WriteString("decisions:\n")
		for _, d := range ev.decisions {
			fmt.Fprintf(&b, "  - %s\n", d)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// reportOneLine is rung 2: the headline alone, with child outcomes folded
// into a count so the line still says how the branch came out.
func reportOneLine(n *Node) string {
	counts := map[Status]int{}
	for _, c := range n.Children {
		counts[c.Status]++
	}
	line := statusLine(n)
	var bits []string
	for _, st := range []Status{StatusDone, StatusDropped, StatusBlocked, StatusDoing, StatusTodo} {
		if c := counts[st]; c > 0 {
			bits = append(bits, fmt.Sprintf("%d %s", c, st))
		}
	}
	if len(bits) > 0 {
		line += " (" + strings.Join(bits, ", ") + ")"
	}
	return line
}

// reportPointer is rung 3, the ladder's floor: a pointer the model opens
// with the task tool's show action rather than any more of the report.
func reportPointer(n *Node) string {
	word := string(n.Status)
	if word != "" {
		word = strings.ToUpper(word[:1]) + word[1:]
	}
	return fmt.Sprintf("%s: %s — task show %s for the report", word, n.Text, n.ID)
}

// maxRung is the ladder's floor: reportPointer, past which a report cannot
// condense any further.
const maxRung = 3

// rungText renders n at one rung of the condensation ladder.
func rungText(n *Node, rung int) string {
	switch {
	case rung <= 0:
		return buildReport(n)
	case rung == 1:
		return reportHeadline(n)
	case rung == 2:
		return reportOneLine(n)
	default:
		return reportPointer(n)
	}
}

// statusLine is "<id>. <text> — <status>", with ": <reason>" appended for a
// blocked or dropped node that gave one — the same phrasing the task
// document itself uses, so a report never disagrees with the file it came
// from.
func statusLine(n *Node) string {
	if n.Reason != "" && (n.Status == StatusBlocked || n.Status == StatusDropped) {
		return fmt.Sprintf("%s. %s — %s: %s", n.ID, n.Text, n.Status, n.Reason)
	}
	return fmt.Sprintf("%s. %s — %s", n.ID, n.Text, n.Status)
}

// leftLines lists every blocked or dropped descendant of n with why,
// depth-first, so "what was left undone" survives even once the per-child
// line above it has condensed away.
func leftLines(n *Node) string {
	var lines []string
	var walk func(*Node)
	walk = func(m *Node) {
		for _, c := range m.Children {
			if c.Status == StatusBlocked || c.Status == StatusDropped {
				lines = append(lines, "  - "+statusLine(c))
			}
			walk(c)
		}
	}
	walk(n)
	return strings.Join(lines, "\n")
}

// subtreeEvidence is n and everything under it, folded into the shapes a
// report prints. Files are merged by path exactly the way the old
// store-wide digest was (latest turn wins for hash/note, ranges merge);
// cmds keep only the successes, because a failed one is already named by
// its errors entry as "<args>: <first line>" and printing it again under
// cmds would print the same command twice (ruling T2-a).
type subtreeEvidence struct {
	files     []FileRef
	cmdsOK    []CmdRef
	decisions []string
	errors    []string
}

func collectEvidence(n *Node) subtreeEvidence {
	var out subtreeEvidence
	byPath := map[string]int{}
	var walk func(*Node)
	walk = func(m *Node) {
		for _, f := range m.Evidence.Files {
			if i, ok := byPath[f.Path]; ok {
				cur := &out.files[i]
				for _, r := range f.Ranges {
					cur.Ranges = mergeRange(cur.Ranges, r)
				}
				cur.Edited = cur.Edited || f.Edited
				if f.Turn >= cur.Turn {
					cur.Turn = f.Turn
					if f.Hash != "" {
						cur.Hash = f.Hash
					}
					if f.Note != "" {
						cur.Note = f.Note
					}
				}
				continue
			}
			cp := f
			cp.Ranges = append([]Range(nil), f.Ranges...)
			byPath[f.Path] = len(out.files)
			out.files = append(out.files, cp)
		}
		for _, c := range m.Evidence.Cmds {
			if c.OK {
				out.cmdsOK = append(out.cmdsOK, c)
			}
		}
		for _, nt := range m.Evidence.Notes {
			if nt.Decision {
				out.decisions = append(out.decisions, nt.Text)
			}
		}
		out.errors = append(out.errors, m.Evidence.Errors...)
		for _, c := range m.Children {
			walk(c)
		}
	}
	walk(n)
	return out
}
