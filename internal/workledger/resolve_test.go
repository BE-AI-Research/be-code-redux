package workledger

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func conflictOn(t *testing.T, ws string) Conflict {
	t.Helper()
	f := filepath.Join(ws, "doc.md")
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return Conflict{Path: f, Workspace: ws, Theirs: []byte("theirs\n")}
}

func TestResolveAsksAndKeepsMineOnYes(t *testing.T) {
	ws := gitRepo(t)
	var asked string
	ask := func(action, detail string) bool { asked = action; return true }

	got, err := Resolve(context.Background(), conflictOn(t, ws), ask, false, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if got != KeepMine {
		t.Errorf("decision = %v, want KeepMine", got)
	}
	if asked != "work_conflict" {
		t.Errorf("asked %q, want work_conflict", asked)
	}
}

func TestResolveKeepsTheirsOnNo(t *testing.T) {
	ws := gitRepo(t)
	got, err := Resolve(context.Background(), conflictOn(t, ws),
		func(string, string) bool { return false }, false, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if got != KeepTheirs {
		t.Errorf("decision = %v, want KeepTheirs", got)
	}
}

// Spec §7: with nobody to ask, write last — but snapshot first and say where
// the old version went. Never refuse.
func TestResolveDuringAFiredTurnWritesAfterKeepingTheOldVersion(t *testing.T) {
	ws := gitRepo(t)
	var notices []string
	got, err := Resolve(context.Background(), conflictOn(t, ws),
		func(string, string) bool { t.Fatal("a fired turn must not be asked"); return false },
		true, func(s string) { notices = append(notices, s) })
	if err != nil {
		t.Fatal(err)
	}
	if got != KeepMine {
		t.Errorf("decision = %v, want KeepMine: an unattended turn finishes its work", got)
	}
	if len(notices) == 0 {
		t.Fatal("an unattended overwrite must say where the old version went")
	}
	joined := strings.Join(notices, " ")
	if !strings.Contains(joined, "be-code/pre-overwrite/") {
		t.Errorf("the notice must name the restore point: %q", joined)
	}
	if !strings.Contains(joined, "git restore") {
		t.Errorf("the notice must give the command that restores it: %q", joined)
	}
}

// A nil asker is the same situation: nobody to ask.
func TestResolveWithNoAskerWritesAfterKeepingTheOldVersion(t *testing.T) {
	ws := gitRepo(t)
	var notices []string
	got, err := Resolve(context.Background(), conflictOn(t, ws), nil, false,
		func(s string) { notices = append(notices, s) })
	if err != nil {
		t.Fatal(err)
	}
	if got != KeepMine {
		t.Errorf("decision = %v, want KeepMine", got)
	}
	if len(notices) == 0 {
		t.Error("an unattended overwrite must say where the old version went")
	}
}

// Review Focus 5: the "write last" premise is that the old version survives.
// With nowhere to keep it, the write must not proceed.
func TestResolveUnattendedRefusesWhenNothingCanBeKept(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	ws := t.TempDir() // not a repo, so a sidecar is the only option
	sub := filepath.Join(ws, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(sub, "doc.md")
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sub, 0o755) })

	c := Conflict{Path: f, Workspace: ws, Theirs: []byte("theirs\n")}
	_, err := Resolve(context.Background(), c, nil, true, func(string) {})
	if err == nil {
		t.Fatal("an unattended overwrite proceeded with nowhere to keep the old version")
	}
}

// The detail is what a person reads to choose. It has to name the file, what
// each answer does, and — when a record says so — who changed it.
func TestDetailNamesTheFileTheOptionsAndWhoChangedIt(t *testing.T) {
	c := Conflict{
		Path:      "/w/.be-code/tasks/003-thing.md",
		Workspace: "/w",
		Theirs:    []byte("theirs\n"),
		Others:    []Record{{PID: 48231, Branch: "feat/thing", Task: "003"}},
	}
	d := c.Detail()
	for _, want := range []string{"003-thing.md", "48231", "feat/thing", "keep mine", "keep theirs", "show me"} {
		if !strings.Contains(d, want) {
			t.Errorf("the detail should mention %q:\n%s", want, d)
		}
	}
}

// A conflict with no work record still has to explain itself: the change may
// have come from a person's editor rather than another session.
func TestDetailWithNoRecordStillSaysWhatHappened(t *testing.T) {
	c := Conflict{Path: "/w/a.go", Workspace: "/w", Theirs: []byte("x")}
	d := c.Detail()
	if !strings.Contains(d, "a.go") {
		t.Errorf("the detail must name the file:\n%s", d)
	}
	if strings.Contains(d, "pid 0") {
		t.Errorf("no record means no pid to name:\n%s", d)
	}
	if !strings.Contains(d, "changed since") {
		t.Errorf("the detail must say what happened:\n%s", d)
	}
}
