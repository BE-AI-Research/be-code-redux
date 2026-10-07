package verify

import (
	"context"
	osexec "os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The point of an argument vector: nothing in it is word-split, globbed or
// substituted. A pack's verify command carries a project path, and paths
// contain spaces ("C:\Unreal Projects\My Game") and characters a shell would
// read as syntax.
func TestArgvCheckRunsWithoutAShell(t *testing.T) {
	echo, err := osexec.LookPath("echo")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("needs a real echo binary")
	}
	hostile := `a;b|c $HOME && rm -rf .`
	proj := Project{Kind: "pack", Checks: []Check{{
		Name: "build", Argv: []string{echo, hostile}, Timeout: 20 * time.Second,
	}}}
	rep := RunChecks(context.Background(), t.TempDir(), proj)
	if len(rep.Results) != 1 || !rep.Results[0].Passed {
		t.Fatalf("check did not pass: %+v", rep.Results)
	}
	if got := strings.TrimSpace(rep.Results[0].Output); got != hostile {
		t.Fatalf("the shell got at the argument:\n got %q\nwant %q", got, hostile)
	}
}

// An argv check still has to be shown to a person — in an approval prompt and
// in the repair turn — so it needs one readable rendering.
func TestDisplayCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Check
		want string
	}{
		{"shell check unchanged", Check{Command: "go build ./..."}, "go build ./..."},
		{"argv joined", Check{Argv: []string{"ue-build", "--project", "/p"}}, "ue-build --project /p"},
		{"argument with spaces is quoted", Check{Argv: []string{"ue-build", "--project", `C:\Unreal Projects\My Game`}},
			`ue-build --project "C:\Unreal Projects\My Game"`},
		{"empty", Check{}, ""},
	} {
		if got := tc.c.Display(); got != tc.want {
			t.Errorf("%s: Display() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The gate is the one seam an approval or a deny rule hangs off, so an argv
// check must reach it too — as its display form, since the gate takes a
// string.
func TestArgvCheckGoesThroughTheGate(t *testing.T) {
	var asked []string
	proj := Project{Kind: "pack", Checks: []Check{{
		Name: "build", Argv: []string{"ue-build", "--project", "/p"}, Timeout: time.Second,
	}}}
	rep := RunChecksGated(context.Background(), t.TempDir(), proj, func(cmd string) bool {
		asked = append(asked, cmd)
		return false
	})
	if len(asked) != 1 || asked[0] != "ue-build --project /p" {
		t.Fatalf("gate saw %v", asked)
	}
	if rep.Declined != "ue-build --project /p" {
		t.Fatalf("Declined = %q", rep.Declined)
	}
	if len(rep.Results) != 0 {
		t.Fatalf("a declined check must not run: %+v", rep.Results)
	}
}

// A check is written down in a config file or a pack manifest, so the ways of
// writing it wrongly are worth catching where it was written.
func TestValidateCheck(t *testing.T) {
	ok := []Check{
		{Name: "a", Command: "go build ./..."},
		{Name: "b", Argv: []string{"ue-build", "--project", "/p"}, Parser: "ubt"},
		{Name: "c", Command: "go test ./...", Fallback: "go test ./... -short"},
	}
	for _, c := range ok {
		if err := ValidateCheck(c); err != nil {
			t.Errorf("%s rejected: %v", c.Name, err)
		}
	}
	bad := []struct {
		name   string
		c      Check
		wantIn string
	}{
		{"neither", Check{Name: "x"}, "command"},
		{"both", Check{Name: "x", Command: "go build", Argv: []string{"go", "build"}}, "not both"},
		{"empty argv element", Check{Name: "x", Argv: []string{"ue-build", ""}}, "empty"},
		{"unknown parser", Check{Name: "x", Command: "go build", Parser: "ubtparse"}, "unknown parser"},
		// Fallback is a shell string; pairing it with argv would quietly run a
		// shell for a check that asked not to have one.
		{"argv with fallback", Check{Name: "x", Argv: []string{"ue-build"}, Fallback: "ue-build --clean"}, "fallback"},
		{"no name", Check{Command: "go build"}, "name"},
	}
	for _, tc := range bad {
		err := ValidateCheck(tc.c)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantIn) {
			t.Errorf("%s: message %q does not mention %q", tc.name, err, tc.wantIn)
		}
	}
}

// skipReason reads the program name out of the command string. An argv check
// has no command string, and indexing the empty field list panics — in the
// middle of handling a failure, which is the worst place for it.
func TestSkipReasonDoesNotPanicOnAnArgvCheck(t *testing.T) {
	c := Check{Name: "tests", Argv: []string{"/usr/bin/python3", "-m", "pytest"}}
	note, skip := skipReason(c, "ImportError: No module named pytest", nil)
	if !skip {
		t.Fatal("expected the missing-runner skip")
	}
	if !strings.Contains(note, "python3") {
		t.Errorf("note does not name the interpreter: %q", note)
	}
}

// The repair turn names the command that failed; for an argv check that must
// be its display form, not an empty string.
func TestModelSummaryNamesAnArgvCommand(t *testing.T) {
	c := Check{Name: "build", Argv: []string{"ue-build", "--project", "/p"}}
	rep := &Report{Results: []CheckResult{{Check: c, Output: "boom"}}}
	if got := rep.ModelSummary(); !strings.Contains(got, "ue-build --project /p") {
		t.Errorf("summary does not name the command:\n%s", got)
	}
}
