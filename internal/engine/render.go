package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Render builds the Working memory block body: notes, task, files read,
// recent lookups — in that order, so the budget drops the least valuable
// part first. inMap reports whether the repository map already lists a
// file's symbols (then the outline is not repeated). Empty when nothing
// has been learned yet.
func (s *Store) Render(budget int, inMap func(path string) bool) string {
	s.mu.Lock()
	notes := s.notes
	l := s.ledger
	digests := s.digestsLocked()
	lookups := make([]Lookup, len(s.lookups))
	copy(lookups, s.lookups)
	s.mu.Unlock()

	var parts []string
	if strings.TrimSpace(notes) != "" {
		parts = append(parts, strings.TrimRight(notes, "\n"))
	}
	if l.Task != "" || len(l.Steps) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "Task: %s\n", l.Task)
		var done []string
		for i, st := range l.Steps {
			switch st.Status {
			case "doing":
				fmt.Fprintf(&b, "doing: %d. %s\n", i+1, st.Text)
			case "done", "skip":
				done = append(done, fmt.Sprintf("%d. %s", i+1, st.Text))
			}
		}
		if len(done) > 0 {
			fmt.Fprintf(&b, "done: %s\n", strings.Join(done, "; "))
		}
		if todo := countStatus(l.Steps, "todo"); todo > 0 {
			fmt.Fprintf(&b, "todo: %d step(s)\n", todo)
		}
		if len(l.Decisions) > 0 {
			b.WriteString("decisions:\n- " + strings.Join(l.Decisions, "\n- ") + "\n")
		}
		if len(l.Facts) > 0 {
			b.WriteString("facts:\n- " + strings.Join(l.Facts, "\n- ") + "\n")
		}
		parts = append(parts, strings.TrimRight(b.String(), "\n"))
	}
	if len(digests) > 0 {
		var b strings.Builder
		b.WriteString("Files read:\n")
		for _, d := range digests {
			b.WriteString(s.digestRow(d, inMap))
			b.WriteByte('\n')
		}
		parts = append(parts, strings.TrimRight(b.String(), "\n"))
	}
	if len(lookups) > 0 {
		var b strings.Builder
		b.WriteString("Recent lookups:\n")
		for i := len(lookups) - 1; i >= 0; i-- {
			b.WriteString(lookupRow(lookups[i]))
			b.WriteByte('\n')
		}
		parts = append(parts, strings.TrimRight(b.String(), "\n"))
	}
	if len(parts) == 0 {
		return ""
	}
	return trimLines(strings.Join(parts, "\n\n"), budget)
}

func countStatus(steps []Step, status string) int {
	n := 0
	for _, st := range steps {
		if st.Status == status {
			n++
		}
	}
	return n
}

func (s *Store) digestRow(d Digest, inMap func(string) bool) string {
	var b strings.Builder
	b.WriteString(d.Path)
	if len(d.Ranges) > 0 {
		var rs []string
		for _, r := range d.Ranges {
			rs = append(rs, fmt.Sprintf("%d–%d", r.From, r.To))
		}
		fmt.Fprintf(&b, " (lines %s)", strings.Join(rs, ", "))
	}
	if d.Note != "" {
		b.WriteString(" — " + d.Note)
	}
	if d.Hash != "" && s.stale(d) {
		b.WriteString(" [changed since read]")
	}
	if d.Edited {
		b.WriteString(" [you edited this]")
	}
	if len(d.Outline) > 0 && (inMap == nil || !inMap(d.Path)) {
		b.WriteString("\n  outline: " + strings.Join(d.Outline, ", "))
	}
	return b.String()
}

// stale checks size and mtime first and hashes only when they moved.
func (s *Store) stale(d Digest) bool {
	info, err := os.Stat(filepath.Join(s.root, filepath.FromSlash(d.Path)))
	if err != nil || info.IsDir() {
		return true
	}
	if info.Size() == d.Size && info.ModTime().UnixNano() == d.ModTime {
		return false
	}
	_, hash, _, _, ok := s.fileState(d.Path)
	return !ok || hash != d.Hash
}

func lookupRow(l Lookup) string {
	q := l.Query
	if i := strings.Index(q, " "); i >= 0 {
		q = q[i+1:]
	}
	q = summariseKey(q)
	var refs []string
	seen := map[string]bool{}
	for _, h := range l.Hits {
		ref := fmt.Sprintf("%s:%d", h.File, h.Line)
		if seen[ref] {
			continue
		}
		seen[ref] = true
		refs = append(refs, ref)
		if len(refs) >= 8 {
			break
		}
	}
	if len(refs) == 0 {
		refs = []string{"no hits"}
	}
	return fmt.Sprintf("%s %s: %s", l.Tool, q, strings.Join(refs, ", "))
}

// summariseKey turns the canonical key back into a readable "query" for
// the block: the first quoted string value it finds.
func summariseKey(k string) string {
	for _, part := range strings.Split(k, ";") {
		if i := strings.Index(part, "=\""); i >= 0 && strings.HasSuffix(part, "\"") {
			return part[i+1:]
		}
	}
	return `""`
}

// trimLines cuts s to at most budget bytes at a line boundary.
func trimLines(s string, budget int) string {
	if budget <= 0 || len(s) <= budget {
		return s
	}
	cut := s[:budget]
	if nl := strings.LastIndexByte(cut, '\n'); nl > 0 {
		cut = cut[:nl]
	}
	return strings.TrimRight(cut, "\n")
}

// LedgerText is the /task listing.
func (s *Store) LedgerText() string {
	l := s.Ledger()
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

// StoppedAt is the text of the step in progress, or "".
func (s *Store) StoppedAt() string {
	for _, st := range s.Ledger().Steps {
		if st.Status == "doing" {
			return st.Text
		}
	}
	return ""
}

var fileNoteLine = regexp.MustCompile(`(?m)^\s*(?:-\s*)?([^\s—]+)\s+—\s+(.+)$`)

// ApplyFileNotes stores "path — note" lines from a compaction summary's
// files: block for paths that already have a digest.
func (s *Store) ApplyFileNotes(block string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range fileNoteLine.FindAllStringSubmatch(block, -1) {
		if d, ok := s.digests[relPath(m[1])]; ok {
			d.Note = strings.TrimSpace(m[2])
			s.dirty = true
		}
	}
}

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
