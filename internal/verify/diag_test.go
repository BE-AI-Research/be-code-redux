package verify

import (
	"strconv"
	"strings"
	"testing"
)

// A pack or a config entry names a parser; an unknown name must be a
// reportable error rather than a quiet downgrade to exit codes, which would
// look like it was working while telling the model nothing.
func TestParserForRejectsUnknownNames(t *testing.T) {
	for _, name := range ParserNames() {
		if _, ok := ParserFor(name); !ok {
			t.Errorf("ParserNames lists %q but ParserFor does not have it", name)
		}
	}
	if _, ok := ParserFor("ubtparse"); ok {
		t.Error("an unknown parser name must not resolve")
	}
	if got := ParserNames(); len(got) != 4 || got[0] != "gcc" || got[3] != "ubt" {
		t.Errorf("ParserNames() = %v, want the four built-ins sorted", got)
	}
}

func TestGenericParserUsesTheExitCode(t *testing.T) {
	p, _ := ParserFor("generic")
	if d := p("all fine\n", 0, ""); !d.OK {
		t.Fatal("exit 0 must pass")
	}
	d := p("make: *** [all] Error 1\n", 2, "")
	if d.OK || d.First == nil || d.First.Raw == "" {
		t.Fatalf("got %+v", d.First)
	}
	// Two different failures of the same command must not look like a repeat.
	if d.Signature == p("a totally different problem\n", 2, "").Signature {
		t.Error("generic signatures do not distinguish failures")
	}
}

// The named parsers are deliberately narrow: "gcc" is the Clang/GCC form and
// "msvc" is the Visual C++ and linker forms. A parser that quietly accepted
// the other toolchain's lines would report a diagnostic from a build it does
// not understand, and the file paths in it would not resolve.
func TestNamedParsersStayInTheirOwnFormat(t *testing.T) {
	gcc, _ := ParserFor("gcc")
	msvc, _ := ParserFor("msvc")
	msvcLine := `C:\P\Source\A.cpp(42,10): error C2065: 'x': undeclared identifier`
	gccLine := `/p/Source/A.cpp:57:3: error: no member named 'Fier' in 'AWeapon'`

	if d := gcc(msvcLine, 1, ""); d.First.Code == "C2065" {
		t.Error("the gcc parser claimed an MSVC diagnostic")
	}
	if d := msvc(gccLine, 1, ""); d.First.Code != "EXIT" {
		t.Errorf("the msvc parser claimed a Clang diagnostic: %+v", d.First)
	}
	// Each still reads its own.
	if d := gcc(gccLine, 1, ""); d.First.Line != 57 || d.First.Column != 3 {
		t.Errorf("gcc parser missed its own format: %+v", d.First)
	}
	if d := msvc(msvcLine, 1, `C:\P`); d.First.Code != "C2065" || d.First.File != "Source/A.cpp" {
		t.Errorf("msvc parser missed its own format: %+v", d.First)
	}
	// And a failure it cannot read still fails, with the exit-code fallback.
	if d := gcc(msvcLine, 1, ""); d.OK || d.First.Code != "EXIT" {
		t.Errorf("an unreadable failure must still fail: %+v", d.First)
	}
}

// A check that names a parser must reach the model as one root cause, not as
// a head-and-tail slice of the whole log. This is the point of the parsers:
// a failing unity build is thousands of lines, and all but the first are
// consequences.
func TestModelSummaryShowsTheRootCauseWhenAParserIsNamed(t *testing.T) {
	out := `Building 4 actions with 16 processes...
C:\P\Source\A.cpp(42,10): error C2065: 'Healthh': undeclared identifier
C:\P\Source\A.cpp(43,5): error C2228: left of '.Num' must have class/struct/union
Result: Failed`
	c := Check{Name: "build", Command: "ue-build", Parser: "msvc"}
	d := diagnose(c, out, exitErr{6}, `C:\P`)
	if d == nil || d.First == nil {
		t.Fatal("no diagnosis")
	}
	rep := &Report{Results: []CheckResult{{Check: c, Output: out, Diagnosis: d}}}
	got := rep.ModelSummary()
	if !strings.Contains(got, "Source/A.cpp:42:10") || !strings.Contains(got, "C2065") {
		t.Errorf("summary does not lead with the root cause:\n%s", got)
	}
	if !strings.Contains(got, "2 errors") {
		t.Errorf("summary does not say how many errors there were:\n%s", got)
	}
	if strings.Contains(got, "Building 4 actions") {
		t.Errorf("summary still carries the whole log:\n%s", got)
	}
}

// With no parser named, the summary is what it has always been.
func TestModelSummaryUnchangedWithoutAParser(t *testing.T) {
	out := "some build output\nthat nobody parses"
	rep := &Report{Results: []CheckResult{{Check: Check{Name: "test", Command: "go test ./..."}, Output: out}}}
	got := rep.ModelSummary()
	if !strings.Contains(got, out) || !strings.Contains(got, "FAIL: test") {
		t.Errorf("plain summary changed:\n%s", got)
	}
}

func TestValidateParser(t *testing.T) {
	if err := ValidateParser("ubt"); err != nil {
		t.Errorf("ubt rejected: %v", err)
	}
	if err := ValidateParser(""); err != nil {
		t.Errorf("an empty name means no parser, which is allowed: %v", err)
	}
	err := ValidateParser("ubtparse")
	if err == nil {
		t.Fatal("an unknown parser name must be rejected at load time")
	}
	// The message has to name the alternatives, since this reaches a pack author.
	if !strings.Contains(err.Error(), "ubt") || !strings.Contains(err.Error(), "generic") {
		t.Errorf("message does not list the built-ins: %v", err)
	}
}

// exitErr stands in for an *exec.ExitError without running a command.
type exitErr struct{ code int }

func (e exitErr) Error() string { return "exit status " + strconv.Itoa(e.code) }
func (e exitErr) ExitCode() int { return e.code }
