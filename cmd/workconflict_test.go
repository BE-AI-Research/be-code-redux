package cmd

import (
	"os"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/workledger"
)

// The owner's ruling is "write last, never refuse" wherever nobody can be
// asked. Attendance therefore has to be decided from facts the caller knows,
// not inferred from whether an approver exists — `be-code run` installs one
// unconditionally, so inferring from it made every headless and -y run refuse
// the write instead of keeping the old version and writing.
//
// The two tests that used to live here asserted the refusal, which was the
// bug: they passed because headlessApprover declined work_conflict.
func TestConflictAttendance(t *testing.T) {
	cases := []struct {
		name                 string
		fired, headless, tty bool
		want                 workledger.Attendance
	}{
		{"a person at a terminal", false, false, true, workledger.Attended},
		{"a fired scheduled turn has no deadline to ask within", true, false, true, workledger.Unattended},
		{"a headless run has nobody at all", false, true, false, workledger.Unattended},
		{"a headless run with a terminal still has nobody watching", false, true, true, workledger.Unattended},
		{"an interactive session whose stdin is a pipe", false, false, false, workledger.Unattended},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := conflictAttendance(c.fired, c.headless, c.tty); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// And headlessApprover must *not* special-case work_conflict any more: a run
// with nobody to ask never raises it, so the only runs that do have a person
// at the terminal, and refusing it there was the defect.
func TestHeadlessApproverDoesNotSpecialCaseWorkConflict(t *testing.T) {
	b, err := os.ReadFile("commands.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `action == "work_conflict"`) {
		t.Error("headlessApprover still classifies work_conflict; a run with nobody to ask does not raise it, " +
			"and refusing it when a person is there is what broke the write-last ruling")
	}
}

// Every ledger hook the registry offers is wired, including the one edit_file
// asks: a hook left unassigned is a detection that silently never fires.
func TestBuildAgentWiresTheChangedHook(t *testing.T) {
	b, err := os.ReadFile("root.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "reg.OnChanged = workLedger.Changed") {
		t.Error("buildAgent does not wire Registry.OnChanged, so edit_file can never say a file moved")
	}
}

// The session host is a detached process whose stdin is nil — /dev/null, a
// character device — so stdinIsTTY happened to say "terminal" and the default
// interactive mode was Attended by accident. A host serves terminals through
// its socket, and that is what makes it attended, not its own stdin.
func TestAttendedHereCountsAServedHost(t *testing.T) {
	savedTTY, savedHost := stdinIsTTY, flagSessionHost
	t.Cleanup(func() { stdinIsTTY, flagSessionHost = savedTTY, savedHost })
	stdinIsTTY = func() bool { return false }

	flagSessionHost = ""
	if attendedHere() {
		t.Error("a pipe on stdin and no host is unattended")
	}
	flagSessionHost = "ABC123"
	if !attendedHere() {
		t.Error("a session host has terminals attached; it is attended whatever its own stdin is")
	}
	flagSessionHost = ""
	stdinIsTTY = func() bool { return true }
	if !attendedHere() {
		t.Error("a terminal on stdin is attended")
	}
}
