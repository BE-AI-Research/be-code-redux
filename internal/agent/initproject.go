package agent

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/brown-enterprises/be-code/internal/discover"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// harnessWords flags mentions of the harness itself rather than the
// project. "compat" and "read_file" are deliberately excluded: they
// false-positive on ordinary prose ("backward compatibility", a project's
// own read_file helper).
var harnessWords = []string{"be-code", "tool_call", "write_file", "edit_file", "verification loop", "agent loop", "system prompt"}

// projectVocabulary returns the harness words a document may use because the
// *measured facts* already contain them: a repository whose README, file
// names or symbols talk about `tool_call` (BE-Code's own checkout being the
// extreme case — its module path is the blocklist's first entry) cannot be
// described accurately without them. Writing what the facts show is
// grounded; the rule exists to catch a document that drifted into describing
// the harness instead, and every word the facts do NOT mention stays
// forbidden.
func projectVocabulary(facts discover.Facts) map[string]bool {
	sheet := strings.ToLower(facts.Markdown())
	out := map[string]bool{}
	for _, w := range harnessWords {
		if strings.Contains(sheet, w) {
			out[w] = true
		}
	}
	return out
}

// commandSpan finds backtick-quoted spans anywhere in a line (not just at
// line start, since generated prose usually leads with prose before the
// code span, e.g. "Build with `go build ./...`").
var commandSpan = regexp.MustCompile("`([^`\n]+)`")

// fencedBlock finds fenced code blocks (```...```), which get their own
// scan since a single-backtick span never sees inside them.
var fencedBlock = regexp.MustCompile("(?s)```[a-zA-Z0-9]*\n(.*?)```")

// commandVerb recognizes the runners whose invocations must match a
// measured command exactly.
var commandVerb = regexp.MustCompile(`^(go|npm|npx|python3?|pytest|cargo|make)\b`)

// extractCommands collects every runner invocation the doc claims, from the
// idiomatic Markdown for a command: lines inside fenced code blocks, and
// inline backtick spans. Bare prose lines are deliberately NOT scanned —
// ordinary sentences ("make sure the daemon is running", "go to the
// settings page") legitimately start with a runner verb without being a
// command.
func extractCommands(doc string) []string {
	var cmds []string
	for _, m := range fencedBlock.FindAllStringSubmatch(doc, -1) {
		for _, line := range strings.Split(m[1], "\n") {
			line = strings.TrimSpace(line)
			if commandVerb.MatchString(line) {
				cmds = append(cmds, line)
			}
		}
	}
	rest := fencedBlock.ReplaceAllString(doc, "")
	for _, m := range commandSpan.FindAllStringSubmatch(rest, -1) {
		cmd := strings.TrimSpace(m[1])
		if commandVerb.MatchString(cmd) {
			cmds = append(cmds, cmd)
		}
	}
	return cmds
}

// commandMeasured reports whether one shown command is one of the measured
// names. The match is per *command*, not a substring of the joined names: a
// command is accepted when it is exactly a measured name, or a measured name
// followed by flags (`go test ./... -race` against a measured
// `go test ./...`), so `go generate ./...` is still rejected even though
// "go" and "./..." both occur among the names.
//
// A compound line is split first and each runner segment checked on its own,
// because `go vet ./... && go test ./...` documents two measured commands,
// not an unmeasured third one.
func commandMeasured(cmd string, names []string) bool {
	for _, seg := range splitCompound(cmd) {
		seg = strings.TrimSpace(seg)
		if seg == "" || !commandVerb.MatchString(seg) {
			continue // a non-runner segment (grep, tee) is not ours to police
		}
		if !segmentMeasured(seg, names) {
			return false
		}
	}
	return true
}

// compoundSep splits a shell line on the operators that separate whole
// commands.
var compoundSep = regexp.MustCompile(`&&|\|\||;|\|`)

func splitCompound(cmd string) []string { return compoundSep.Split(cmd, -1) }

func segmentMeasured(seg string, names []string) bool {
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || !strings.HasPrefix(seg, n) {
			continue
		}
		if len(seg) == len(n) {
			return true
		}
		if seg[len(n)] != ' ' {
			continue
		}
		// Only flags may follow a measured command: another path or
		// subcommand would make it a different invocation.
		if rest := strings.TrimSpace(seg[len(n):]); strings.HasPrefix(rest, "-") {
			return true
		}
	}
	return false
}

// ValidateProjectNotes returns the reasons a generated BECODE.md must be
// rejected; empty means acceptable.
func ValidateProjectNotes(doc string, facts discover.Facts) []string {
	var v []string
	low := strings.ToLower(doc)
	ownVocabulary := projectVocabulary(facts)
	for _, w := range harnessWords {
		if ownVocabulary[w] {
			continue // the project's own vocabulary, measured in its facts
		}
		if strings.Contains(low, w) {
			v = append(v, fmt.Sprintf("it describes the tool (%q)", w))
		}
	}
	cited := 0
	for _, n := range facts.Names() {
		if strings.Contains(doc, n) {
			cited++
		}
	}
	if cited < 2 {
		v = append(v, "it cites fewer than two measured files, directories or commands")
	}
	names := facts.Names()
	seen := map[string]bool{}
	for _, cmd := range extractCommands(doc) {
		if seen[cmd] {
			continue
		}
		seen[cmd] = true
		if !commandMeasured(cmd, names) {
			v = append(v, fmt.Sprintf("command %q is not in the measured facts", cmd))
		}
	}
	if strings.Count(strings.TrimSpace(doc), "\n")+1 > 150 {
		v = append(v, "it is longer than 150 lines")
	}
	return v
}

// InitProject asks the model for BECODE.md prose from the facts, validates
// it, retries once with the violations, and falls back to the fact sheet.
func (a *Agent) InitProject(ctx context.Context, facts discover.Facts) (string, bool, error) {
	user := "Facts:\n\n" + facts.Markdown()
	var last []string
	var lastDoc string
	for attempt := 0; attempt < 2; attempt++ {
		msgs := []provider.Message{{Role: provider.RoleSystem, Content: InitFrame}, {Role: provider.RoleUser, Content: user}}
		if attempt == 1 {
			msgs = append(msgs,
				provider.Message{Role: provider.RoleAssistant, Content: lastDoc},
				provider.Message{Role: provider.RoleUser, Content: "The previous attempt was rejected because: " + strings.Join(last, "; ") + ". Write BECODE.md again, correcting these."})
		}
		a.awaitWindow(ctx) // never send with no window on the wire
		// In the lane: /init runs on its own goroutine, outside any turn.
		resp, err := a.inLane(ctx, func() (*provider.ChatResponse, error) {
			return a.Provider.Chat(ctx, provider.ChatRequest{Model: a.Model, Messages: msgs, Temperature: 0.2, NoThink: true}, nil)
		})
		if err != nil {
			return "", false, err
		}
		doc := resp.Content
		if a.Profile.StripThink {
			doc = StripThink(doc)
		}
		doc = strings.TrimSpace(doc) + "\n"
		last = ValidateProjectNotes(doc, facts)
		if len(last) == 0 {
			return doc, false, nil
		}
		lastDoc = doc
	}
	// The fallback is written to BECODE.md *and* loaded as the project notes,
	// so it has to fit MaxProjectNotes by itself: NotesFallback drops the
	// repo-map "## Symbols" section (which was only ever for the model's own
	// request) and trims at a line boundary, rather than letting the caller's
	// cap cut the document off mid-sentence.
	reasons := strings.Join(last, "; ")
	if len(reasons) > 600 { // a doc full of unmeasured commands must not crowd out the facts
		reasons = reasons[:600] + "…"
	}
	head := fmt.Sprintf("<!-- generated by be-code init from measured facts; the model's overview was rejected: %s -->\n\n", reasons)
	return head + facts.NotesFallback(MaxProjectNotes-len(head)), true, nil
}
