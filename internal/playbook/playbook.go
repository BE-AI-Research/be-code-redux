// Package playbook parses a pack's error playbook (packs spec §11.1) and
// matches a diagnostic against it.
//
// A playbook is how a pack turns a failure the harness can already name into
// the next move: a cause in one sentence, a fix in numbered steps, and a
// "next" to try when the same failure comes back rather than repeating the
// change that did not work. That last field is the one that matters most for
// a small model, which will otherwise make the same edit again.
//
// A leaf package: it takes the pieces of a diagnostic as strings and knows
// nothing about where they came from.
package playbook

import (
	"bufio"
	"fmt"
	"regexp"
	"strings"
)

// Entry is one playbook entry.
type Entry struct {
	Title string
	Match *regexp.Regexp
	Kind  string // "", "any", or a diagnostic kind
	Cause string
	Fix   string
	Next  string
}

// maxLine bounds one line of a playbook. bufio's own 64 KiB default used to
// stop the scan part-way through a document, and because flush() ran before
// the scanner's error was checked, the truncation was reported as a missing
// match line while every entry after it vanished.
const maxLine = 1 << 20

// Parse reads a playbook document. It is deliberately lenient: a playbook is
// a file a person edits, and one mistake in it must not take away every other
// entry's routing. An entry that cannot be used is skipped and named in
// warnings; err is returned only when the document itself could not be read.
// Anything before the first "## " heading is preamble.
func Parse(doc string) (entries []Entry, warnings []string, err error) {
	var cur *Entry
	var field string         // which multi-line field we are appending to
	var seen map[string]bool // keys already set on cur
	curBad := false          // cur was already reported; do not report it twice
	inFence := false
	orphan := false

	warn := func(format string, args ...any) {
		warnings = append(warnings, fmt.Sprintf(format, args...))
	}

	flush := func() {
		if cur == nil {
			return
		}
		if cur.Match == nil {
			if !curBad {
				warn("playbook entry %q has no match line; entry skipped", cur.Title)
			}
			cur = nil
			return
		}
		cur.Cause = strings.TrimSpace(cur.Cause)
		cur.Fix = strings.TrimSpace(cur.Fix)
		cur.Next = strings.TrimSpace(cur.Next)
		entries = append(entries, *cur)
		cur = nil
	}

	body := func(line string) {
		if cur == nil {
			return
		}
		switch field {
		case "cause":
			cur.Cause += "\n" + line
		case "fix":
			cur.Fix += "\n" + line
		case "next":
			cur.Next += "\n" + line
		}
	}

	sc := bufio.NewScanner(strings.NewReader(doc))
	sc.Buffer(make([]byte, 0, 64*1024), maxLine)
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimLeft(line, " \t")

		// A fenced block is prose. Inside one, a line that happens to begin
		// with a field name is part of the body — the entries here legitimately
		// show code, and treating such a line as a field silently rewrote the
		// entry's routing regex and discarded the rest of the body.
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			body(line)
			continue
		}

		// A heading ends an unclosed fence. Without this, inFence stayed set
		// for the rest of the file and every later entry became body text of
		// this one — silently, which is the one failure mode the format doc
		// and the loader both single out as worse than a bad entry. A fence
		// that swallows a heading is always a mistake, so recovering here
		// costs one entry's body instead of the whole table.
		if inFence && strings.HasPrefix(line, "## ") {
			title := "(before the first entry)"
			if cur != nil {
				title = cur.Title
			}
			warn("playbook entry %q: unclosed code fence; it was closed at the next heading", title)
			inFence = false
		}

		if !inFence && strings.HasPrefix(trimmed, "## ") {
			flush()
			cur = &Entry{Title: strings.TrimSpace(strings.TrimPrefix(trimmed, "## "))}
			field, seen, curBad = "", map[string]bool{}, false
			continue
		}
		if cur == nil {
			if !inFence {
				if _, _, ok := splitField(trimmed); ok {
					orphan = true
				}
			}
			continue
		}
		if !inFence {
			if key, val, ok := splitField(trimmed); ok {
				if seen[key] {
					warn("playbook entry %q: repeated %q line; keeping the first", cur.Title, key)
					continue
				}
				seen[key] = true
				switch key {
				case "match":
					if strings.TrimSpace(val) == "" {
						// regexp.Compile("") succeeds and matches everything,
						// so leaving the value off produced a silent
						// catch-all — the one thing the format doc says in
						// bold not to write, and the loader cannot tell it
						// from a deliberate one.
						warn("playbook entry %q: empty match, which would match every failure; entry skipped", cur.Title)
						curBad = true
						field = ""
						continue
					}
					re, cerr := regexp.Compile(val)
					if cerr != nil {
						warn("playbook entry %q: bad match regex %q: %v; entry skipped", cur.Title, val, cerr)
						curBad = true
					}
					cur.Match, field = re, ""
				case "kind":
					cur.Kind, field = val, ""
				case "cause":
					cur.Cause, field = val, "cause"
				case "fix":
					cur.Fix, field = val, "fix"
				case "next":
					cur.Next, field = val, "next"
				}
				continue
			}
		}
		body(line)
	}
	flush()
	if orphan {
		warn("playbook has field lines before the first %q heading; they were ignored", "## ")
	}
	// Before anything else is reported: a scanner that stopped early makes
	// every later entry look absent rather than unread.
	if err := sc.Err(); err != nil {
		return entries, warnings, fmt.Errorf("playbook could not be read (a line may be longer than %d bytes): %w", maxLine, err)
	}
	return entries, warnings, nil
}

// Match returns the first entry whose kind fits and whose regex matches the
// raw line or the message, or nil. An entry that declares a kind takes only
// that kind: an unknown kind ("") must not be routed to a typed entry, since
// a link entry's advice is wrong for a compile failure. Entries with no kind,
// or "any", take anything.
func Match(entries []Entry, kind, raw, message string) *Entry {
	for i := range entries {
		e := &entries[i]
		// Entry and its Match are exported, so a caller that merges playbooks
		// or appends a fallback of its own can hand us an entry with no regex.
		if e.Match == nil {
			continue
		}
		if e.Kind != "" && e.Kind != "any" && e.Kind != kind {
			continue
		}
		if e.Match.MatchString(raw) || e.Match.MatchString(message) {
			return e
		}
	}
	return nil
}

func splitField(line string) (key, val string, ok bool) {
	for _, k := range []string{"match", "kind", "cause", "fix", "next"} {
		if strings.HasPrefix(line, k+":") {
			return k, strings.TrimSpace(line[len(k)+1:]), true
		}
	}
	return "", "", false
}
