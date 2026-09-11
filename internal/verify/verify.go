// Package verify implements BE-Code's generate→lint→test→repair pipeline.
//
// Local models produce more broken code than frontier models; the fix is
// not a better prompt, it is a tight feedback loop with real toolchains.
// verify detects the project type, runs its build/lint/test commands, and
// condenses failures into a compact, model-consumable summary.
package verify

import (
	"context"
	"fmt"
	"os"
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
}

// Project describes a detected project type and its checks.
type Project struct {
	Kind   string // go | node | python | rust | make | none
	Checks []Check
}

// CheckResult is the outcome of one check.
type CheckResult struct {
	Check  Check
	Passed bool
	Output string
	Err    error
}

// Report aggregates a verification run.
type Report struct {
	Project Project
	Results []CheckResult
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
		checks := []Check{
			{Name: "py compile", Command: `python3 -m compileall -q .`, Timeout: 3 * time.Minute},
		}
		if exists(root, "pyproject.toml") || exists(root, "pytest.ini") || exists(root, "tests") || exists(root, "test") {
			checks = append(checks, Check{Name: "pytest", Command: "python3 -m pytest -x -q", Timeout: 10 * time.Minute})
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

// RunChecks executes the project's checks in order, stopping at the first
// failure (later checks usually cascade from the same root cause, and the
// model repairs best with one failure at a time).
func RunChecks(ctx context.Context, root string, proj Project) *Report {
	rep := &Report{Project: proj}
	for _, c := range proj.Checks {
		out, err := tools.RunShell(ctx, root, c.Command, c.Timeout)
		if err != nil && c.Fallback != "" {
			out, err = tools.RunShell(ctx, root, c.Fallback, c.Timeout)
		}
		res := CheckResult{Check: c, Passed: err == nil, Output: out, Err: err}
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
		fmt.Fprintf(&b, "FAIL: %s (command: %s)\n", res.Check.Name, res.Check.Command)
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

// Human renders the report for terminal display.
func (r *Report) Human() string {
	if len(r.Results) == 0 {
		return "no verification checks apply (unrecognized project type)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "verification (%s project):\n", r.Project.Kind)
	for _, res := range r.Results {
		mark := "PASS"
		if !res.Passed {
			mark = "FAIL"
		}
		fmt.Fprintf(&b, "  [%s] %s\n", mark, res.Check.Name)
	}
	return strings.TrimRight(b.String(), "\n")
}
