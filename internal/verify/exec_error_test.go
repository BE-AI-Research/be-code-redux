package verify

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A pack installs by copying a folder, and a copy does not always keep the
// executable bit — docs/packs-integration-notes.md predicted exactly this.
// When the program cannot be run there is no output to parse, so the exec
// error is the only information in existence; ModelSummary discarded it as
// soon as a parser had synthesised any Diagnosis at all, leaving the model
// with "the command failed with exit code 1 and no recognisable error line"
// and nothing to act on. The repair loop then burned every round, plus an
// auto:verify consultation, on a message with no content.
func TestModelSummaryKeepsTheExecErrorWhenThereIsNoOutput(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	c := Check{Name: "build", Argv: []string{missing, "--project", "/ws"}, Parser: "gcc"}

	// What runCheck produces for a program that cannot be started: no output,
	// and an error from exec.
	execErr := errors.New("fork/exec " + missing + ": no such file or directory")
	d := diagnose(c, "", execErr, "/ws")
	rep := &Report{Results: []CheckResult{{Check: c, Output: "", Err: execErr, Diagnosis: d}}}

	got := rep.ModelSummary()
	if !strings.Contains(got, "no such file or directory") {
		t.Errorf("the exec error is the only information there is, and it was dropped:\n%s", got)
	}
}

// Same for a program that exists but is not executable, which is the commoner
// half of the same install problem.
func TestModelSummaryKeepsThePermissionError(t *testing.T) {
	dir := t.TempDir()
	prog := filepath.Join(dir, "ue-build")
	if err := os.WriteFile(prog, []byte("#!/bin/sh\necho hi\n"), 0o644); err != nil { // no +x
		t.Fatal(err)
	}
	c := Check{Name: "build", Argv: []string{prog}, Parser: "gcc"}
	execErr := errors.New("fork/exec " + prog + ": permission denied")
	d := diagnose(c, "", execErr, dir)
	rep := &Report{Results: []CheckResult{{Check: c, Output: "", Err: execErr, Diagnosis: d}}}

	got := rep.ModelSummary()
	if !strings.Contains(got, "permission denied") {
		t.Errorf("a missing executable bit must say so:\n%s", got)
	}
}

// The reason the exec error was dropped must not come back: a real parsed
// diagnostic still leads, and the summary must not regress into pasting the
// whole log beside it.
func TestARealDiagnosticStillLeadsAndTheLogStaysOut(t *testing.T) {
	out := `Building...
/p/src/a.cpp:42:10: error: use of undeclared identifier 'Healthh'
/p/src/a.cpp:43:5: error: no member named 'Num'
Result: Failed`
	c := Check{Name: "build", Command: "ue-build", Parser: "gcc"}
	d := diagnose(c, out, exitErr{6}, "/p")
	if d == nil || d.First == nil {
		t.Fatal("no diagnosis")
	}
	rep := &Report{Results: []CheckResult{{Check: c, Output: out, Err: exitErr{6}, Diagnosis: d}}}

	got := rep.ModelSummary()
	if !strings.Contains(got, "a.cpp:42:10") {
		t.Errorf("summary does not lead with the root cause:\n%s", got)
	}
	if strings.Contains(got, "Building...") {
		t.Errorf("summary carries the whole log again:\n%s", got)
	}
}

// A check built without a timeout used to expire the instant it started,
// reporting "[timed out after 0s]" for a command that never ran — which reads
// as a broken build rather than a missing field. ValidateCheck deliberately
// does not reject it (a timeout is a runtime concern, not a shape error), so
// the default belongs where the check runs.
func TestCheckWithNoTimeoutDoesNotFailInstantly(t *testing.T) {
	c := Check{Name: "echo", Argv: []string{"echo", "hello"}} // no Timeout
	out, err := c.run(context.Background(), os.TempDir())
	if err != nil {
		t.Fatalf("a check with no timeout failed: %v (output %q)", err, out)
	}
	if !strings.Contains(out, "hello") {
		t.Errorf("the command did not run: %q", out)
	}
}

// And an explicit timeout is still honoured, so the default cannot mask one
// somebody chose deliberately.
func TestAnExplicitTimeoutIsStillHonoured(t *testing.T) {
	c := Check{Name: "sleep", Argv: []string{"sleep", "5"}, Timeout: 50 * time.Millisecond}
	start := time.Now()
	_, err := c.run(context.Background(), os.TempDir())
	if err == nil {
		t.Fatal("a check that outlasts its timeout must fail")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("the explicit timeout was ignored: took %s", elapsed)
	}
}
