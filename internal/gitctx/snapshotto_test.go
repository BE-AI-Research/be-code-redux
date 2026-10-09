package gitctx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func repoWithACommit(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(argv ...string) {
		cmd := exec.Command("git", argv...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", argv, err, out)
		}
	}
	run("init", "--quiet", "--initial-branch=main")
	run("config", "user.email", "fixture@example.com")
	run("config", "user.name", "Fixture")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "--quiet", "-m", "first")
	return dir
}

func gitOut(t *testing.T, dir string, argv ...string) string {
	t.Helper()
	cmd := exec.Command("git", argv...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", argv, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestSnapshotToUsesTheGivenPrefix(t *testing.T) {
	dir := repoWithACommit(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("about to be overwritten\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	branch, err := SnapshotTo(context.Background(), dir, SnapshotPrefixOverwrite, "be-code: before an unattended overwrite")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(branch, SnapshotPrefixOverwrite) {
		t.Errorf("branch = %q, want the overwrite prefix", branch)
	}
	// The content at that branch is what was about to be lost.
	body := gitOut(t, dir, "show", branch+":a.txt")
	if body != "about to be overwritten" {
		t.Errorf("the snapshot holds %q", body)
	}
}

// The snapshot must not move HEAD, the index or the checkout — the whole
// point of the private GIT_INDEX_FILE.
func TestSnapshotToLeavesTheCheckoutAlone(t *testing.T) {
	dir := repoWithACommit(t)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("working\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	headBefore := gitOut(t, dir, "rev-parse", "HEAD")
	branchBefore := gitOut(t, dir, "rev-parse", "--abbrev-ref", "HEAD")

	if _, err := SnapshotTo(context.Background(), dir, SnapshotPrefixOverwrite, "m"); err != nil {
		t.Fatal(err)
	}
	if got := gitOut(t, dir, "rev-parse", "HEAD"); got != headBefore {
		t.Error("HEAD moved")
	}
	if got := gitOut(t, dir, "rev-parse", "--abbrev-ref", "HEAD"); got != branchBefore {
		t.Error("the checked-out branch changed")
	}
	body, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "working\n" {
		t.Errorf("the working tree changed: %q", body)
	}
	if out := gitOut(t, dir, "status", "--porcelain"); !strings.Contains(out, "a.txt") {
		t.Errorf("the uncommitted change is no longer uncommitted: %q", out)
	}
}

// Snapshot keeps its signature and its prefix, so /init's callers and tests
// do not move.
func TestSnapshotStillUsesTheInitPrefix(t *testing.T) {
	dir := repoWithACommit(t)
	branch, err := Snapshot(context.Background(), dir, "be-code: restore point before init")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(branch, SnapshotPrefixInit) {
		t.Errorf("branch = %q, want the init prefix", branch)
	}
	if SnapshotPrefix != SnapshotPrefixInit {
		t.Error("SnapshotPrefix must stay an alias for the init prefix")
	}
}

// Review Focus 1, first half: outside a repository there is nothing to
// snapshot to, and the caller has to be able to tell.
func TestSnapshotToOutsideARepositorySaysSo(t *testing.T) {
	_, err := SnapshotTo(context.Background(), t.TempDir(), SnapshotPrefixOverwrite, "m")
	if err == nil {
		t.Fatal("snapshotting a directory that is not a repository succeeded")
	}
	if !strings.Contains(err.Error(), "not a git repository") {
		t.Errorf("the error should say what is wrong: %v", err)
	}
}

// Two snapshots in the same second must not collide.
func TestTwoSnapshotsInOneSecondGetDifferentBranches(t *testing.T) {
	dir := repoWithACommit(t)
	a, err := SnapshotTo(context.Background(), dir, SnapshotPrefixOverwrite, "m")
	if err != nil {
		t.Fatal(err)
	}
	b, err := SnapshotTo(context.Background(), dir, SnapshotPrefixOverwrite, "m")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Errorf("both snapshots are on %q", a)
	}
}
