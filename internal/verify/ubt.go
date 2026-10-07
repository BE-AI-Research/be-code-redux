// UBT, UHT, MSVC, Clang and linker output, parsed into one root cause.
//
// Ported from the be-code-packs handoff bundle (reference/ubtparse) on
// 2026-10-07. The spec is BE-SPEC-CODE-PACKS-2026-001 §8.2. Every pattern
// here earned its place against real output; the Windows forms in particular
// are easy to break, so ubt_test.go pins the ones that were once dropped
// silently: absolute paths with a drive letter, and MSBuild's "N>" prefix.
package verify

import (
	"regexp"
	"strconv"
	"strings"
)

var (
	// MSVC compiler: C:\Proj\Source\A.cpp(12,5): error C2065: 'x': undeclared identifier
	reMSVC = regexp.MustCompile(`^\s*(?P<file>.+?)\((?P<line>\d+)(?:,(?P<col>\d+))?\)\s*:\s*(?P<sev>fatal error|error|warning)\s+(?P<code>[A-Z]{1,3}\d{3,5})\s*:\s*(?P<msg>.*)$`)

	// UHT: /Proj/Source/A.h(12): Error: Expected a GENERATED_BODY() at the start of class
	reUHT = regexp.MustCompile(`^\s*(?P<file>.+?\.(?:h|hpp|inl))\((?P<line>\d+)\)\s*:\s*(?P<sev>Error|Warning)\s*:\s*(?P<msg>.*)$`)

	// Clang/GCC: /Proj/Source/A.cpp:12:5: error: use of undeclared identifier 'x'
	// The optional drive letter keeps Windows paths from clang-cl intact.
	reGCC = regexp.MustCompile(`^\s*(?P<file>(?:[A-Za-z]:)?[^:]+?):(?P<line>\d+):(?:(?P<col>\d+):)?\s*(?P<sev>fatal error|error|warning):\s*(?P<msg>.*)$`)

	// link.exe: A.cpp.obj : error LNK2019: unresolved external symbol ...
	// The optional drive letter matters as much here as in reGCC: link.exe
	// names its inputs by absolute path, and without it every Windows link
	// failure parsed as nothing at all.
	reLNK = regexp.MustCompile(`^\s*(?P<file>(?:[A-Za-z]:)?[^:]*?)\s*:\s*(?P<sev>fatal error|error|warning)\s+(?P<code>LNK\d{4})\s*:\s*(?P<msg>.*)$`)

	// lld / ld
	reLLD      = regexp.MustCompile(`^\s*(?:ld\.lld|ld|lld-link)(?:\.exe)?:\s*error:\s*(?P<msg>.*)$`)
	reUndefRef = regexp.MustCompile(`^\s*(?P<file>(?:[A-Za-z]:)?[^:]+?)(?::(?P<line>\d+))?(?::\(.*?\))?:\s*(?P<msg>undefined reference to .*)$`)

	// UBT-level failures worth surfacing on their own.
	reUBT = regexp.MustCompile(`^\s*(?:ERROR:|UBT ERROR:|Error:)\s*(?P<msg>.+)$`)

	reWS     = regexp.MustCompile(`\s+`)
	reDigits = regexp.MustCompile(`\d+`)
)

// Fixed UBT messages that have no file/line but are the whole story.
var toolPatterns = []struct {
	contains string
	code     string
}{
	{"Unable to build while Live Coding is active", "UBT-LIVECODING"},
	{"Could not find definition for module", "UBT-MODULE"},
	{"Couldn't find target rules file", "UBT-TARGET"},
	{"Unable to find plugin", "UBT-PLUGIN"},
	{"is not a valid configuration", "UBT-CONFIG"},
}

// kindRank orders root-cause candidates. UHT errors stop the build first and
// cause everything after them, so they win; then compile, then link; UBT-level
// lines like "Failed to produce item" are usually consequences, so they rank
// last unless nothing else was found.
var kindRank = map[string]int{"uht": 0, "compile": 1, "link": 2, "tool": 3}

// betterRootCause reports whether d is a more useful root cause than best.
// Kind decides first. Within compile errors a fatal one wins: a fatal
// "cannot open include file 'X.generated.h'" is the cause of every plain
// "undeclared identifier" that follows it in other translation units.
//
// The same does not hold for the linker, which is why this is restricted to
// compile: link.exe's fatal line is LNK1120 "N unresolved externals", a count
// of the LNK2019s above it rather than a cause of them. Ties keep the earlier
// line.
func betterRootCause(d, best Diagnostic) bool {
	if rank(d.Kind) != rank(best.Kind) {
		return rank(d.Kind) < rank(best.Kind)
	}
	return d.Kind == "compile" && d.Fatal && !best.Fatal
}

// rank orders kinds as root causes. An unrecognised kind ranks last rather
// than taking map-zero's place at the very top, which is where "uht" sits.
func rank(kind string) int {
	if r, ok := kindRank[kind]; ok {
		return r
	}
	return len(kindRank) + 1
}

// lineMatcher recognises one family of diagnostic line. A named parser is a
// list of these: "gcc" is the Clang/GCC form alone, "msvc" adds the linker,
// and "ubt" is everything a UnrealBuildTool run can emit.
type lineMatcher func(raw string) (Diagnostic, bool)

var (
	mUHT lineMatcher = func(raw string) (Diagnostic, bool) {
		m := match(reUHT, raw)
		if m == nil {
			return Diagnostic{}, false
		}
		return Diagnostic{Kind: "uht", Severity: strings.ToLower(m.get("sev")),
			File: m.get("file"), Line: atoi(m.get("line")), Message: m.get("msg"), Raw: raw}, true
	}
	mLNK lineMatcher = func(raw string) (Diagnostic, bool) {
		m := match(reLNK, raw)
		if m == nil {
			return Diagnostic{}, false
		}
		return Diagnostic{Kind: "link", Severity: sev(m.get("sev")), File: m.get("file"),
			Code: m.get("code"), Message: m.get("msg"), Raw: raw, Fatal: isFatal(m.get("sev"))}, true
	}
	mMSVC lineMatcher = func(raw string) (Diagnostic, bool) {
		m := match(reMSVC, raw)
		if m == nil {
			return Diagnostic{}, false
		}
		kind := "compile"
		if strings.HasPrefix(m.get("code"), "LNK") {
			kind = "link"
		}
		return Diagnostic{Kind: kind, Severity: sev(m.get("sev")), File: m.get("file"),
			Line: atoi(m.get("line")), Column: atoi(m.get("col")),
			Code: m.get("code"), Message: m.get("msg"), Raw: raw, Fatal: isFatal(m.get("sev"))}, true
	}
	mLLD lineMatcher = func(raw string) (Diagnostic, bool) {
		m := match(reLLD, raw)
		if m == nil {
			return Diagnostic{}, false
		}
		return Diagnostic{Kind: "link", Severity: "error", Message: m.get("msg"), Raw: raw}, true
	}
	mUndefRef lineMatcher = func(raw string) (Diagnostic, bool) {
		m := match(reUndefRef, raw)
		if m == nil {
			return Diagnostic{}, false
		}
		return Diagnostic{Kind: "link", Severity: "error", File: m.get("file"),
			Line: atoi(m.get("line")), Message: m.get("msg"), Raw: raw}, true
	}
	mGCC lineMatcher = func(raw string) (Diagnostic, bool) {
		m := match(reGCC, raw)
		if m == nil {
			return Diagnostic{}, false
		}
		return Diagnostic{Kind: "compile", Severity: sev(m.get("sev")), File: m.get("file"),
			Line: atoi(m.get("line")), Column: atoi(m.get("col")), Message: m.get("msg"),
			Raw: raw, Fatal: isFatal(m.get("sev"))}, true
	}
	mUBT lineMatcher = func(raw string) (Diagnostic, bool) {
		m := match(reUBT, raw)
		if m == nil {
			return Diagnostic{}, false
		}
		return Diagnostic{Kind: "tool", Severity: "error", Message: m.get("msg"), Raw: raw}, true
	}
	// mToolMessage is the UBT failures that have no file or line but are the
	// whole story. It runs first, and ahead of looksLikeDiagnostic, because
	// none of these lines contain the word "error".
	mToolMessage lineMatcher = func(raw string) (Diagnostic, bool) {
		for _, p := range toolPatterns {
			if strings.Contains(raw, p.contains) {
				return Diagnostic{Kind: "tool", Severity: "error", Code: p.code, Message: raw, Raw: raw}, true
			}
		}
		return Diagnostic{}, false
	}
)

var (
	gccLines  = []lineMatcher{mGCC}
	msvcLines = []lineMatcher{mMSVC, mLNK}
	ubtLines  = []lineMatcher{mToolMessage, mUHT, mLNK, mMSVC, mLLD, mUndefRef, mGCC, mUBT}
)

// parseLine tries each matcher in order. Only the first to recognise the line
// is used, so the order of a parser's list is its precedence.
func parseLine(ms []lineMatcher, line string) (Diagnostic, bool) {
	raw := strings.TrimSpace(stripMSBuildPrefix(line))
	if raw == "" || !looksLikeDiagnostic(raw) {
		return Diagnostic{}, false
	}
	for _, m := range ms {
		if d, ok := m(raw); ok {
			return d, true
		}
	}
	return Diagnostic{}, false
}

// stripMSBuildPrefix removes a leading "1>" / "12>" — the project number
// MSBuild and Visual Studio put on every line of a build. Left in place it
// lands inside the file name, which then never resolves against the
// workspace and so cannot be opened. Hand-rolled rather than a regexp
// because it runs on every line of a log that can reach tens of megabytes,
// and this way it allocates nothing.
func stripMSBuildPrefix(line string) string {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	j := i
	for j < len(line) && line[j] >= '0' && line[j] <= '9' {
		j++
	}
	if j > i && j < len(line) && line[j] == '>' {
		return line[j+1:]
	}
	return line
}

// looksLikeDiagnostic is a cheap gate in front of the patterns. Most lines of
// a build log are progress ("[412/1872] Compile Foo.cpp"), and running every
// regexp over each of them is the difference between parsing a large log once
// and parsing it expensively.
//
// Every pattern requires one of these words or one of the fixed tool
// messages, so the gate cannot hide a diagnostic. A pattern added later that
// needs neither must be added here as well, or its lines will never be seen.
func looksLikeDiagnostic(s string) bool {
	if strings.Contains(s, "rror") || // error, Error, ERROR
		strings.Contains(s, "arning") ||
		strings.Contains(s, "undefined reference") ||
		strings.Contains(s, "undefined symbol") {
		return true
	}
	for _, p := range toolPatterns {
		if strings.Contains(s, p.contains) {
			return true
		}
	}
	return false
}

// groups gives a pattern's named submatches without building a map per
// line. A failing unity build emits error lines by the thousand, and a map
// of six strings for each of them was the bulk of what this package
// allocated; the values here are slices of the line itself, so reading a
// group copies nothing.
type groups struct {
	re  *regexp.Regexp
	s   string
	idx []int
}

func match(re *regexp.Regexp, s string) *groups {
	idx := re.FindStringSubmatchIndex(s)
	if idx == nil {
		return nil
	}
	return &groups{re: re, s: s, idx: idx}
}

func (g *groups) get(name string) string {
	i := g.re.SubexpIndex(name)
	if i < 0 || 2*i+1 >= len(g.idx) || g.idx[2*i] < 0 {
		return ""
	}
	return strings.TrimSpace(g.s[g.idx[2*i]:g.idx[2*i+1]])
}

func isFatal(s string) bool { return strings.Contains(strings.ToLower(s), "fatal") }

func sev(s string) string {
	if strings.Contains(strings.ToLower(s), "error") {
		return "error"
	}
	return "warning"
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
