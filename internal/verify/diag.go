// Structured diagnostics for a failing check.
//
// A check's raw output is the wrong thing to hand a small model: a failing
// unity build can be thirty thousand lines, and the first error is the only
// one worth fixing — every line after it is usually a consequence. A parser
// turns that output into one root cause plus a count, which is the same
// "one failure at a time" contract the built-in checks already follow.
//
// Parsers are compiled in and chosen by name (BE-SPEC-CODE-PACKS-2026-001
// §8.2). A pack may pick one; it may never ship one, so a pack can never run
// code of its own inside the verify loop.
//
// Two deliberate departures from the spec, both for this package's sake:
// the spec's Result is Diagnosis here, because verify already has a
// CheckResult and two kinds of "result" in one package reads badly; and
// Parser is a func type rather than an interface, since a parser is one
// operation with no state.
package verify

import (
	"errors"
	"fmt"
	"hash/fnv"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Diagnostic is one parsed error or warning.
type Diagnostic struct {
	Kind     string // "uht", "compile", "link", "tool", "test"
	Severity string // "error", "warning"
	File     string // workspace-relative where possible, forward slashes
	Line     int
	Column   int
	Code     string // C2065, LNK2019, ... (empty when the tool gives none)
	Message  string
	Raw      string // the line as the tool wrote it, trimmed (any MSBuild "N>" prefix removed)
	// Fatal is set for a "fatal error", which outranks a plain error of the
	// same kind when choosing the root cause. sev() reports both as "error",
	// so severity alone cannot carry this.
	Fatal bool
}

// Diagnosis is what a parser makes of one check's output.
type Diagnosis struct {
	OK        bool
	First     *Diagnostic // the root cause to show the model; nil when OK
	Count     int         // total errors seen
	Signature string      // stable fingerprint of First, for anti-loop
}

// Parser turns a check's combined stdout/stderr plus its exit code into a
// Diagnosis. workspace, when non-empty, is trimmed from file paths so the
// model is given paths it can open.
type Parser func(output string, exitCode int, workspace string) Diagnosis

// parsers is the whole set; a pack or a config entry names one of these and
// cannot introduce another.
var parsers = map[string]Parser{
	"generic": parseGeneric,
	"gcc":     func(out string, code int, ws string) Diagnosis { return parseLines(gccLines, out, code, ws) },
	"msvc":    func(out string, code int, ws string) Diagnosis { return parseLines(msvcLines, out, code, ws) },
	"ubt":     func(out string, code int, ws string) Diagnosis { return parseLines(ubtLines, out, code, ws) },
}

// ParserFor looks up a parser by name. An unknown name is a configuration
// error the caller reports; it is never silently treated as "generic",
// because a pack that asked for "ubt" and got exit codes would look like it
// was working while telling the model nothing.
func ParserFor(name string) (Parser, bool) {
	p, ok := parsers[name]
	return p, ok
}

// ValidateParser reports whether a configured parser name is usable. An
// empty name is valid and means "no parser". Call this where a check is
// written down — config, a pack manifest — because a name that only fails at
// run time produces a check that looks fine and explains nothing.
func ValidateParser(name string) error {
	if name == "" {
		return nil
	}
	if _, ok := parsers[name]; !ok {
		return fmt.Errorf("unknown parser %q; the built-in parsers are %s", name, strings.Join(ParserNames(), ", "))
	}
	return nil
}

// diagnose runs a check's parser over its output. It returns nil when the
// check names no parser, when the name is unknown (ValidateParser is where
// that is meant to be caught), or when the parser finds nothing wrong.
func diagnose(c Check, output string, err error, workspace string) *Diagnosis {
	parse, ok := ParserFor(c.Parser)
	if !ok {
		return nil
	}
	code := 0
	if err != nil {
		code = 1 // failed, but the runner could not say how
		var ec interface{ ExitCode() int }
		if errors.As(err, &ec) {
			code = ec.ExitCode()
		}
	}
	d := parse(output, code, workspace)
	if d.OK {
		return nil
	}
	return &d
}

// ParserNames lists the built-in parsers, sorted, for validation messages
// and the config reference.
func ParserNames() []string {
	out := make([]string, 0, len(parsers))
	for n := range parsers {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// parseGeneric is the parser for a toolchain nobody has written patterns for:
// the exit code decides, and the tail of the output is what the model is
// shown. The tail also fingerprints the failure, so two different failures of
// the same command do not read as a repeat.
func parseGeneric(output string, exitCode int, workspace string) Diagnosis {
	if exitCode == 0 {
		return Diagnosis{OK: true}
	}
	d := Diagnostic{
		Kind:     "tool",
		Severity: "error",
		Message:  "command failed with exit code " + strconv.Itoa(exitCode),
		Raw:      lastLines(output, 40),
	}
	return Diagnosis{First: &d, Count: 1, Signature: Signature(d) + "|" + tailKey(d.Raw)}
}

// parseLines is the body every pattern-based parser shares: one pass, keeping
// only the best diagnostic and a count. A unity build failing in a widely
// included header emits thousands of near-identical error lines, and nothing
// but the root cause and the count is ever used, so nothing else is kept.
func parseLines(ms []lineMatcher, output string, exitCode int, workspace string) Diagnosis {
	var best Diagnostic
	count := 0
	for rest := output; rest != ""; {
		line := rest
		if i := strings.IndexByte(rest, '\n'); i >= 0 {
			line, rest = rest[:i], rest[i+1:]
		} else {
			rest = ""
		}
		d, ok := parseLine(ms, strings.TrimSuffix(line, "\r"))
		if !ok || d.Severity != "error" {
			continue
		}
		count++
		if count == 1 || betterRootCause(d, best) {
			d.File = relPath(d.File, workspace)
			best = d
		}
	}
	if count == 0 {
		if exitCode == 0 {
			return Diagnosis{OK: true}
		}
		// Nothing recognisable, but it failed. The diagnostic is identical for
		// every such failure, so its signature alone would make a brand-new
		// failure read as a repeat; the tail distinguishes them.
		d := Diagnostic{
			Kind:     "tool",
			Severity: "error",
			Code:     "EXIT",
			Message:  "the command failed with exit code " + strconv.Itoa(exitCode) + " and no recognisable error line",
			Raw:      lastLines(output, 40),
		}
		return Diagnosis{First: &d, Count: 1, Signature: Signature(d) + "|" + tailKey(d.Raw)}
	}
	return Diagnosis{First: &best, Count: count, Signature: Signature(best)}
}

// Signature is a stable fingerprint: kind|code|file|message, with line and
// column left out so the same failure on a shifted line still matches, and
// whitespace collapsed so formatting differences do not split it.
func Signature(d Diagnostic) string {
	msg := strings.ToLower(reWS.ReplaceAllString(strings.TrimSpace(d.Message), " "))
	return strings.Join([]string{d.Kind, d.Code, strings.ToLower(d.File), msg}, "|")
}

// tailKey fingerprints an unrecognised failure's own output. Digits are
// masked so a differing duration ("Total execution time: 4.21 seconds") does
// not make one failure look like two.
func tailKey(tail string) string {
	norm := strings.ToLower(reWS.ReplaceAllString(strings.TrimSpace(tail), " "))
	h := fnv.New32a()
	h.Write([]byte(reDigits.ReplaceAllString(norm, "#")))
	return strconv.FormatUint(uint64(h.Sum32()), 16)
}

// Format renders a diagnostic the way a repair turn should show it.
func Format(d Diagnostic) string {
	var b strings.Builder
	if d.File != "" {
		b.WriteString(d.File)
		if d.Line > 0 {
			b.WriteString(":" + strconv.Itoa(d.Line))
			if d.Column > 0 {
				b.WriteString(":" + strconv.Itoa(d.Column))
			}
		}
		b.WriteString(": ")
	}
	// Severity defaults to error: Format renders the root cause of a failure,
	// and a diagnostic built by hand (a test, a hand-written fallback) often
	// leaves it unset. Naming a warning correctly still matters, so this
	// reads the field rather than hardcoding the word.
	severity := d.Severity
	if severity == "" {
		severity = "error"
	}
	b.WriteString(strings.TrimSpace(d.Kind + " " + severity))
	if d.Code != "" {
		b.WriteString(" " + d.Code)
	}
	b.WriteString(": " + d.Message)
	return b.String()
}

func relPath(file, workspace string) string {
	f := strings.ReplaceAll(file, `\`, "/")
	if workspace == "" {
		return f
	}
	w := strings.TrimRight(strings.ReplaceAll(workspace, `\`, "/"), "/") + "/"
	if len(f) >= len(w) && strings.EqualFold(f[:len(w)], w) {
		return f[len(w):]
	}
	return filepath.ToSlash(f)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\r\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
