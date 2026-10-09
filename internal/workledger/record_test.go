package workledger

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func stubAlive(t *testing.T, live map[int]bool) {
	t.Helper()
	saved := Alive
	Alive = func(pid int) bool { return live[pid] }
	t.Cleanup(func() { Alive = saved })
}

func rec(pid int, ws string) Record {
	now := time.Now().UTC().Format(time.RFC3339)
	return Record{PID: pid, Started: now, Updated: now, Workspace: ws, Branch: "main"}
}

func TestSaveAndReadBack(t *testing.T) {
	dir := t.TempDir()
	stubAlive(t, map[int]bool{41: true, 42: true})

	if err := Save(dir, rec(41, "/w")); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, rec(42, "/w")); err != nil {
		t.Fatal(err)
	}
	got := Others(dir, "/w", 41)
	if len(got) != 1 || got[0].PID != 42 {
		t.Fatalf("Others should report the other session only, got %+v", got)
	}
}

// A record is 0600: it names a workspace path and what somebody is working on.
func TestRecordIsPrivate(t *testing.T) {
	dir := t.TempDir()
	r := rec(41, "/w")
	if err := Save(dir, r); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(r.Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("record mode is %v, want 0600", st.Mode().Perm())
	}
}

// A dead process's record is pruned, not reported.
func TestADeadSessionsRecordIsPruned(t *testing.T) {
	dir := t.TempDir()
	stubAlive(t, map[int]bool{41: true}) // 42 is gone
	if err := Save(dir, rec(41, "/w")); err != nil {
		t.Fatal(err)
	}
	gone := rec(42, "/w")
	if err := Save(dir, gone); err != nil {
		t.Fatal(err)
	}
	if got := Others(dir, "/w", 41); len(got) != 0 {
		t.Fatalf("a dead session was reported: %+v", got)
	}
	if _, err := os.Stat(gone.Path(dir)); !os.IsNotExist(err) {
		t.Error("a dead session's record was not removed from disk")
	}
}

// Global constraint: a pid that cannot be queried counts as alive. The
// Windows lesson — os.FindProcess asks for a privilege a non-elevated
// terminal is refused for an elevated session, and live.List then deleted a
// live record and the launch forked a copy onto a copy of the session.
func TestAnUnqueryablePIDCountsAlive(t *testing.T) {
	dir := t.TempDir()
	saved := Alive
	Alive = func(int) bool { return true } // procattr.Alive's "access denied" answer
	t.Cleanup(func() { Alive = saved })

	r := rec(42, "/w")
	if err := Save(dir, r); err != nil {
		t.Fatal(err)
	}
	if got := Others(dir, "/w", 41); len(got) != 1 {
		t.Fatal("a record whose pid cannot be queried must be kept, not pruned")
	}
}

// Review Focus 3: two spellings of one workspace must see each other, or the
// feature silently never fires. live.SameWorkspace resolves links and folds
// case on Windows; == does not.
func TestTwoSpellingsOfOneWorkspaceSeeEachOther(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	dir := t.TempDir()
	stubAlive(t, map[int]bool{42: true})
	if err := Save(dir, rec(42, real)); err != nil {
		t.Fatal(err)
	}
	if got := Others(dir, link, 41); len(got) != 1 {
		t.Errorf("a session reached through a symlink did not see the other: %+v", got)
	}
}

// Review Focus 2: a pid reused by an unrelated process makes a dead session
// look alive, and its Writing list is then fiction. The record cannot prove
// identity, so it says so rather than asserting work nobody is doing.
func TestAStaleRecordWithALivePIDIsMarkedSuspect(t *testing.T) {
	dir := t.TempDir()
	stubAlive(t, map[int]bool{42: true})
	old := rec(42, "/w")
	old.Updated = time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	old.Writing = []string{"internal/thing.go"}
	if err := Save(dir, old); err != nil {
		t.Fatal(err)
	}
	got := Others(dir, "/w", 41)
	if len(got) != 1 {
		t.Fatalf("got %d records, want 1", len(got))
	}
	if !got[0].Suspect {
		t.Error("a record untouched for longer than StaleAfter with a live pid must be marked suspect: the pid may have been reused")
	}
	if len(got[0].Writing) != 0 {
		t.Error("a suspect record's Writing list must be dropped, not reported as current work")
	}
}

func TestRemoveTakesOnlyThisSessionsRecord(t *testing.T) {
	dir := t.TempDir()
	stubAlive(t, map[int]bool{41: true, 42: true})
	mine, theirs := rec(41, "/w"), rec(42, "/w")
	if err := Save(dir, mine); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, theirs); err != nil {
		t.Fatal(err)
	}
	if err := Remove(dir, mine.PID, mine.Started); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mine.Path(dir)); !os.IsNotExist(err) {
		t.Error("my record is still there")
	}
	if _, err := os.Stat(theirs.Path(dir)); err != nil {
		t.Error("somebody else's record was removed")
	}
}

// Never fail a session: an unreadable record is ignored and removed, like a
// pruned live record.
func TestAnUnparsableRecordIsIgnored(t *testing.T) {
	dir := t.TempDir()
	stubAlive(t, map[int]bool{42: true})
	if err := os.WriteFile(filepath.Join(dir, "99-bad.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Save(dir, rec(42, "/w")); err != nil {
		t.Fatal(err)
	}
	got := Others(dir, "/w", 41)
	if len(got) != 1 {
		t.Fatalf("a corrupt record broke the listing: %+v", got)
	}
}

func TestOthersOnAMissingDirectoryIsEmptyNotAnError(t *testing.T) {
	if got := Others(filepath.Join(t.TempDir(), "nope"), "/w", 41); len(got) != 0 {
		t.Errorf("got %+v, want nothing", got)
	}
}

// I2: the seen store lives in the same directory and is named
// <pid>-<started>.seen.json, which matches a ".json" filter — and
// unmarshalling {"path":"hash"} into Record succeeds with PID 0, so a
// name-based filter took the "not a record" branch and deleted it. Every
// Others() call was removing every session's seen file, including its own.
func TestOthersLeavesFilesItDoesNotRecognise(t *testing.T) {
	dir := t.TempDir()
	stubAlive(t, map[int]bool{42: true})
	if err := Save(dir, rec(42, "/w")); err != nil {
		t.Fatal(err)
	}
	seen := filepath.Join(dir, "4242-20261009T120000Z.seen.json")
	if err := os.WriteFile(seen, []byte(`{"/w/a.go":"abc"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(dir, "notes.json")
	if err := os.WriteFile(stray, []byte(`{"anything":1}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := Others(dir, "/w", 1); len(got) != 1 {
		t.Fatalf("got %d records, want 1: %+v", len(got), got)
	}
	for _, p := range []string{seen, stray} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was deleted; Others must never remove a file it cannot classify", filepath.Base(p))
		}
	}
}
