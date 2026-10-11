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

	got, err := Resolve(context.Background(), conflictOn(t, ws), ask, Attended, func(string) {})
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
		func(string, string) bool { return false }, Attended, func(string) {})
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
		Unattended, func(s string) { notices = append(notices, s) })
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
	got, err := Resolve(context.Background(), conflictOn(t, ws), nil, Unattended,
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
	_, err := Resolve(context.Background(), c, nil, Unattended, func(string) {})
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
	// "show me" is deliberately absent: it named a key no UI handled, so the
	// difference is in the detail instead (TestDetailShowsTheDifference...).
	for _, want := range []string{"003-thing.md", "48231", "feat/thing", "keep mine", "keep theirs"} {
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

// C2: the owner's ruling is "write last, never refuse" wherever nobody can be
// asked. Headless installs an approver unconditionally, so `ask == nil` is the
// wrong test for "nobody to ask" — it never holds in `be-code run`, and the
// README this branch adds promises the unattended path for exactly that case.
// Unattended is a fact the caller knows, so the caller passes it in.
func TestResolveUnattendedByFlagWritesAfterKeepingTheOldVersion(t *testing.T) {
	ws := gitRepo(t)
	var notices []string
	// An approver that is present but cannot reach a person — headlessApprover
	// under -y, which refuses work_conflict by design.
	refuser := func(string, string) bool { return false }

	got, err := Resolve(context.Background(), conflictOn(t, ws), refuser,
		Unattended, func(s string) { notices = append(notices, s) })
	if err != nil {
		t.Fatal(err)
	}
	if got != KeepMine {
		t.Errorf("decision = %v, want KeepMine: an unattended run finishes its work", got)
	}
	if len(notices) == 0 {
		t.Error("an unattended overwrite must say where the old version went")
	}
}

// And an attended run still asks, even though the same parameter carries it.
func TestResolveAttendedStillAsks(t *testing.T) {
	ws := gitRepo(t)
	asked := 0
	got, err := Resolve(context.Background(), conflictOn(t, ws),
		func(string, string) bool { asked++; return false }, Attended, func(string) {})
	if err != nil {
		t.Fatal(err)
	}
	if asked != 1 {
		t.Errorf("asked %d times, want 1", asked)
	}
	if got != KeepTheirs {
		t.Errorf("decision = %v, want KeepTheirs", got)
	}
}

// I9: the detail a person reads says "keep mine — write this session's
// version; theirs is kept and the notice says where". When KeepOld fails
// nothing is kept, and saying nothing makes that sentence a lie. The write
// still proceeds — they chose it, having seen the difference — but they are
// told.
func TestAttendedYesSaysSoWhenTheOldVersionCouldNotBeKept(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	ws := t.TempDir() // not a repo, so a sidecar is the only route
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

	var notices []string
	got, err := Resolve(context.Background(),
		Conflict{Path: f, Workspace: ws, Theirs: []byte("theirs\n")},
		func(string, string) bool { return true }, Attended,
		func(s string) { notices = append(notices, s) })
	if err != nil {
		t.Fatalf("a person's yes must still write: %v", err)
	}
	if got != KeepMine {
		t.Errorf("decision = %v, want KeepMine", got)
	}
	joined := strings.Join(notices, " ")
	if !strings.Contains(joined, "could not be kept") {
		t.Errorf("the person was told theirs would be kept and it was not; say so: %q", joined)
	}
}

// I5: the detail advertised "d show me", which no UI handles — a dead key in
// the TUI and, in plain mode, read as "no", so pressing it abandoned the
// write. The spec's reason for making this an approval rather than a yes/no
// was that it carries a diff; so it carries one, and the key that promised
// one and did nothing is gone.
func TestDetailShowsTheDifferenceAndAdvertisesNoDeadKey(t *testing.T) {
	c := Conflict{
		Path:      "/w/a.go",
		Workspace: "/w",
		Theirs:    []byte("package main\n\nfunc theirs() {}\n"),
		Mine:      []byte("package main\n\nfunc mine() {}\n"),
	}
	d := c.Detail()

	if !strings.Contains(d, "theirs()") || !strings.Contains(d, "mine()") {
		t.Errorf("the detail must show both sides of the change:\n%s", d)
	}
	if !strings.Contains(d, "-") || !strings.Contains(d, "+") {
		t.Errorf("the detail must render it as a difference:\n%s", d)
	}
	// No key that no UI handles.
	if strings.Contains(d, "show me") || strings.Contains(d, "d  ") {
		t.Errorf("the detail still advertises a key no UI handles:\n%s", d)
	}
	for _, want := range []string{"keep mine", "keep theirs"} {
		if !strings.Contains(d, want) {
			t.Errorf("the detail should still spell out %q:\n%s", want, d)
		}
	}
}

// A huge change must not put thousands of lines into an approval modal.
func TestDetailBoundsTheDifference(t *testing.T) {
	var theirs, mine strings.Builder
	for i := 0; i < 500; i++ {
		theirs.WriteString("theirs line\n")
		mine.WriteString("mine line\n")
	}
	c := Conflict{Path: "/w/a.go", Workspace: "/w",
		Theirs: []byte(theirs.String()), Mine: []byte(mine.String())}
	d := c.Detail()
	if n := strings.Count(d, "\n"); n > 60 {
		t.Errorf("the detail is %d lines; a modal cannot show that", n)
	}
	if !strings.Contains(d, "more") {
		t.Errorf("a truncated difference must say it was truncated:\n%s", d)
	}
}

// A record Others marked Suspect — alive pid, hour-old record — reads with
// the same confidence as a fresh one unless the detail says otherwise.
func TestDetailSaysWhenTheRecordIsStale(t *testing.T) {
	c := Conflict{Path: "/w/a.go", Workspace: "/w", Theirs: []byte("t\n"), Mine: []byte("m\n"),
		Others: []Record{{PID: 7, Branch: "main", Suspect: true}}}
	if got := c.Detail(); !strings.Contains(got, "stale") {
		t.Errorf("a stale record is not marked in the detail:\n%s", got)
	}
}
