// Package verify implements BE-Code's generate→lint→test→repair pipeline.
//
// Local models produce more broken code than frontier models; the fix is
// not a better prompt, it is a tight feedback loop with real toolchains.
// verify detects the project type, runs its build/lint/test commands, and
// condenses failures into a compact, model-consumable summary.
package verify

import (
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/tools"
)

// Check is one runnable verification step.
type Check struct {
	Name    string // e.g. "go build"
	Command string
	Timeout time.Duration
	// Optional: only run if this file exists relative to root (beyond the
	// Fallback runs only when Command fails (e.g. a test runner flag some
	// projects reject); it keeps shell-specific "a || b" out of check strings.
	Fallback string
	// Argv is an alternative to Command: the program and its arguments,
	// executed with no shell between us and them. Nothing in it is ever
	// word-split, globbed, substituted or quoted away, on any platform —
	// which is why a pack's verify commands are written this way. A path
	// with spaces in it ("C:\\Unreal Projects\\My Game") needs no quoting
	// and no escaping, and a value that happens to contain shell syntax is
	// just a value. Exactly one of Command and Argv is set.
	Argv []string
	// Parser names a built-in output parser (see ParserNames). Empty means
	// the failure is reported as output, the way it always has been; a name
	// means the model is shown one root cause instead. Validate the name with
	// ValidateParser wherever a check is configured, so a typo is caught where
	// it was written rather than silently producing no diagnosis.
	Parser string
}

// Display is the check's command as a person should see it: in an approval
// prompt, in the repair turn, in the report. An argv check is joined with
// quotes around any argument that needs them — for reading only, never for
// execution, which never goes near a shell.
func (c Check) Display() string {
	if len(c.Argv) == 0 {
		return c.Command
	}
	parts := make([]string, len(c.Argv))
	for i, a := range c.Argv {
		if a == "" || strings.ContainsAny(a, " \t\"'") {
			// Plain quotes, and backslashes left alone: this is read by a
			// person, and strconv.Quote would turn a Windows path into
			// "C:\\Unreal Projects\\My Game". Only an embedded quote needs
			// escaping to stay readable.
			parts[i] = `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
			continue
		}
		parts[i] = a
	}
	return strings.Join(parts, " ")
}

// ValidateCheck reports whether a check is written correctly. Call it wherever
// a check comes from outside this package — config, a pack manifest — so a
// mistake is reported against the file it was written in rather than becoming
// a check that runs the wrong thing or nothing at all.
func ValidateCheck(c Check) error {
	if strings.TrimSpace(c.Name) == "" {
		return fmt.Errorf("check has no name")
	}
	switch {
	case c.Command == "" && len(c.Argv) == 0:
		return fmt.Errorf("check %q has no command: set either command or argv", c.Name)
	case c.Command != "" && len(c.Argv) > 0:
		return fmt.Errorf("check %q sets both command and argv; set one, not both", c.Name)
	}
	for i, a := range c.Argv {
		if a == "" {
			return fmt.Errorf("check %q: argv[%d] is empty", c.Name, i)
		}
	}
	// Fallback is a shell string, so pairing it with argv would quietly put a
	// shell behind a check that asked for none.
	if len(c.Argv) > 0 && c.Fallback != "" {
		return fmt.Errorf("check %q cannot have a fallback: fallback is a shell command and argv runs no shell", c.Name)
	}
	return ValidateParser(c.Parser)
}

// program is the executable the check runs, for messages about it. An argv
// check has no command string to take the first word of.
func (c Check) program() string {
	if len(c.Argv) > 0 {
		return c.Argv[0]
	}
	if f := strings.Fields(c.Command); len(f) > 0 {
		return f[0]
	}
	return c.Name
}

// run executes the check, with or without a shell according to how it was
// written.
func (c Check) run(ctx context.Context, root string) (string, error) {
	timeout := c.Timeout
	if timeout <= 0 {
		// A zero timeout expires the instant it starts, so a check built
		// without one failed with "[timed out after 0s]" — which reads as a
		// broken command rather than a missing field, and sends the repair
		// loop after a command that never ran. Every built-in check sets one
		// and packs get a default from their manifest; this is for anything
		// else built against the exported Check, where forgetting the field
		// should not be indistinguishable from a hanging build.
		timeout = defaultRunTimeout
	}
	if len(c.Argv) > 0 {
		return tools.RunArgv(ctx, root, timeout, c.Argv[0], c.Argv[1:]...)
	}
	return tools.RunShell(ctx, root, c.Command, timeout)
}

// defaultRunTimeout matches the longest the built-in checks allow themselves
// (go test, npm test, pytest), so a check with no timeout behaves like the
// most generous one rather than like a failure.
const defaultRunTimeout = 10 * time.Minute

// Project describes a detected project type and its checks.
type Project struct {
	Kind   string // go | node | python | rust | make | none
	Checks []Check
}

// CheckResult is the outcome of one check.
type CheckResult struct {
	Check   Check
	Passed  bool
	Skipped bool   // the check could not run meaningfully (tool missing, nothing to test); counts as passed
	Note    string // why it was skipped
	Output  string
	Err     error
	// Diagnosis is set when the check named a parser and failed: the single
	// root cause the repair turn should show, with a count and a signature.
	Diagnosis *Diagnosis
}

// Report aggregates a verification run.
type Report struct {
	Project Project
	Results []CheckResult
	// Declined is the command RunChecksGated's gate refused; the run
	// stopped before it. Empty when every command was allowed.
	Declined string
}

func exists(root, rel string) bool {
	_, err := os.Stat(filepath.Join(root, rel))
	return err == nil
}

// Detect inspects the workspace and returns the applicable checks.
// Detection is ordered: an explicit Makefile "verify"/"test" target wins,
// then language markers.
func Detect(root string) Project {
	switch {
	case exists(root, "go.mod"):
		return Project{Kind: "go", Checks: []Check{
			{Name: "go vet", Command: "go vet ./...", Timeout: 3 * time.Minute},
			{Name: "go build", Command: "go build ./...", Timeout: 5 * time.Minute},
			{Name: "go test", Command: "go test ./...", Timeout: 10 * time.Minute},
		}}
	case exists(root, "package.json"):
		checks := []Check{}
		if exists(root, "tsconfig.json") {
			checks = append(checks, Check{Name: "tsc", Command: "npx --no-install tsc --noEmit", Timeout: 5 * time.Minute})
		}
		checks = append(checks, Check{Name: "npm test", Command: "npm test --silent -- --watch=false", Fallback: "npm test --silent", Timeout: 10 * time.Minute})
		return Project{Kind: "node", Checks: checks}
	case exists(root, "pyproject.toml") || exists(root, "setup.py") || exists(root, "requirements.txt"):
		py := pythonCmd(root)
		checks := []Check{
			{Name: "py compile", Command: py + " -m compileall -q .", Timeout: 3 * time.Minute},
		}
		if exists(root, "pyproject.toml") || exists(root, "pytest.ini") || exists(root, "tests") || exists(root, "test") {
			checks = append(checks, Check{Name: "pytest", Command: py + " -m pytest -x -q", Timeout: 10 * time.Minute})
		}
		return Project{Kind: "python", Checks: checks}
	case exists(root, "Cargo.toml"):
		return Project{Kind: "rust", Checks: []Check{
			{Name: "cargo check", Command: "cargo check --quiet", Timeout: 10 * time.Minute},
			{Name: "cargo test", Command: "cargo test --quiet", Timeout: 15 * time.Minute},
		}}
	case exists(root, "Makefile"):
		return Project{Kind: "make", Checks: []Check{
			{Name: "make test", Command: "make test", Timeout: 15 * time.Minute},
		}}
	}
	return Project{Kind: "none"}
}

// pythonCmd prefers a workspace virtualenv's interpreter (where the
// project's pytest and dependencies live) over the system python.
func pythonCmd(root string) string {
	for _, cand := range []string{".venv/bin/python", "venv/bin/python", ".venv/Scripts/python.exe", "venv/Scripts/python.exe"} {
		if exists(root, cand) {
			return cand
		}
	}
	return "python3"
}

// skipReason recognizes failures that are not the code's fault and that
// the model cannot repair: a missing test runner, or a runner that found
// nothing to run. Such checks are reported as skipped.
func skipReason(c Check, out string, err error) (string, bool) {
	if strings.Contains(out, "No module named pytest") {
		return "pytest is not installed for the interpreter used (" + c.program() + "); install it or add a .venv", true
	}
	var exitErr *osexec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 5 && (c.Name == "pytest" || strings.Contains(c.Display(), "pytest")) {
		return "no tests collected", true
	}
	return "", false
}

// RunChecks executes the project's checks in order, stopping at the first
// failure (later checks usually cascade from the same root cause, and the
// model repairs best with one failure at a time).
func RunChecks(ctx context.Context, root string, proj Project) *Report {
	return RunChecksGated(ctx, root, proj, nil)
}

// RunChecksGated is RunChecks with every check put to allow first (nil
// allows everything). The first check allow refuses is not run: the report
// stops there with Declined naming it, and the checks before it are kept.
//
// allow receives the whole Check, not just its command text, because the
// decision can depend on how the check was written: an argv check carries a
// program to match deny rules against, which a command string does not have.
func RunChecksGated(ctx context.Context, root string, proj Project, allow func(c Check) bool) *Report {
	rep := &Report{Project: proj}
	for _, c := range proj.Checks {
		if allow != nil && !allow(c) {
			rep.Declined = c.Display()
			return rep
		}
		out, err := c.run(ctx, root)
		if err != nil && c.Fallback != "" {
			// The fallback is a command of its own and is put to allow in its
			// own right. Only a shell check has one (ValidateCheck refuses the
			// pairing), so it is asked about as a shell command.
			if allow != nil && !allow(Check{Name: c.Name, Command: c.Fallback, Timeout: c.Timeout, Parser: c.Parser}) {
				rep.Declined = c.Fallback
				return rep
			}
			out, err = tools.RunShell(ctx, root, c.Fallback, c.Timeout)
		}
		res := CheckResult{Check: c, Passed: err == nil, Output: out, Err: err}
		if err != nil {
			if note, skip := skipReason(c, out, err); skip {
				res.Passed, res.Skipped, res.Note, res.Err = true, true, note, nil
			}
		}
		// After skipReason: a check that was skipped did not really fail, so
		// there is nothing to diagnose.
		if !res.Passed {
			res.Diagnosis = diagnose(c, out, err, root)
		}
		rep.Results = append(rep.Results, res)
		if !res.Passed {
			break
		}
	}
	return rep
}

// Passed reports whether every executed check succeeded.
func (r *Report) Passed() bool {
	for _, res := range r.Results {
		if !res.Passed {
			return false
		}
	}
	return len(r.Results) > 0
}

// FailSummary is a one-line human summary.
func (r *Report) FailSummary() string {
	for _, res := range r.Results {
		if !res.Passed {
			return res.Check.Name
		}
	}
	return "all checks passed"
}

// maxErrorFeedback bounds how much failure output goes back to the model —
// enough to fix the problem, not enough to blow the context budget.
const maxErrorFeedback = 6 * 1024

// ModelSummary renders failures for the repair prompt: the failing command
// and a head+tail slice of its output (compiler errors lead; test failures
// often trail).
func (r *Report) ModelSummary() string {
	var b strings.Builder
	for _, res := range r.Results {
		if res.Passed {
			fmt.Fprintf(&b, "PASS: %s\n", res.Check.Name)
			continue
		}
		fmt.Fprintf(&b, "FAIL: %s (command: %s)\n", res.Check.Name, res.Check.Display())
		// One failure at a time: when a parser read the output, the model is
		// shown the root cause alone. The rest of the log is almost always a
		// consequence of it, and on a large build it is thousands of lines.
		if d := res.Diagnosis; d != nil && d.First != nil {
			fmt.Fprintf(&b, "%s\n", Format(*d.First))
			if d.Count > 1 {
				fmt.Fprintf(&b, "(%d errors; fix this one, then run the check again)\n", d.Count)
			}
			// A parser that recognised nothing still produces a diagnostic
			// ("tool error EXIT: … no recognisable error line"), and a command
			// that could not start produces no output at all — so the exec
			// error is the only information that exists. Dropping it left the
			// model with a sentence it could not act on, and the repair loop
			// spent every round plus a consultation on it. A pack installs by
			// copying a folder and a copy does not always keep the executable
			// bit, so this is the likeliest way a pack check ever fails.
			//
			// Only when the output is empty: beside a real parsed diagnostic
			// an "exit status 2" adds nothing and the point of the parsers is
			// one clean root cause.
			if res.Err != nil && strings.TrimSpace(res.Output) == "" &&
				!strings.Contains(Format(*d.First), res.Err.Error()) {
				fmt.Fprintf(&b, "[%s]\n", res.Err.Error())
			}
			continue
		}
		out := strings.TrimSpace(res.Output)
		if res.Err != nil && !strings.Contains(out, res.Err.Error()) {
			out += "\n[" + res.Err.Error() + "]"
		}
		b.WriteString(headTail(out, maxErrorFeedback))
		b.WriteString("\n")
	}
	return b.String()
}

// headTail keeps the first 2/3 and last 1/3 of oversized output.
func headTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	head := max * 2 / 3
	tail := max - head
	return s[:head] + "\n... [output elided] ...\n" + s[len(s)-tail:]
}

// FirstFailure is the check that failed and the diagnosis its parser made of
// the output, or nils. Checks stop at the first failure, so there is at most
// one — and it is the one a playbook should be matched against.
func (r *Report) FirstFailure() (*CheckResult, *Diagnosis) {
	for i := range r.Results {
		if !r.Results[i].Passed {
			return &r.Results[i], r.Results[i].Diagnosis
		}
	}
	return nil, nil
}

// Human renders the report for terminal display.
func (r *Report) Human() string {
	if len(r.Results) == 0 {
		return "no verification checks apply (unrecognized project type)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "verification (%s project):\n", r.Project.Kind)
	for _, res := range r.Results {
		mark := "PASS"
		switch {
		case res.Skipped:
			mark = "SKIP"
		case !res.Passed:
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "  [%s] %s", mark, res.Check.Name)
		if res.Skipped && res.Note != "" {
			fmt.Fprintf(&b, " (%s)", res.Note)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}
