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
