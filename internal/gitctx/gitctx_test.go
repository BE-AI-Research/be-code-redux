package gitctx

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/tools"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	ctx := context.Background()
	for _, cmd := range []string{
		"git init -q -b main",
		"git config user.email t@t.local",
		"git config user.name t",
		"sh -c 'echo hello > a.txt'",
		"git add -A",
		"git commit -qm initial",
	} {
		if out, err := tools.RunShell(ctx, dir, cmd, 30*time.Second); err != nil {
			t.Fatalf("%s: %v %s", cmd, err, out)
		}
	}
	return dir
}

func TestSummaryAndCommit(t *testing.T) {
	dir := gitRepo(t)
	ctx := context.Background()

	if !IsRepo(ctx, dir) {
		t.Fatal("IsRepo false for real repo")
	}
	if IsRepo(ctx, t.TempDir()) {
		t.Fatal("IsRepo true for plain dir")
	}

	s := Summary(ctx, dir)
	if !strings.Contains(s, "main") || !strings.Contains(s, "clean") {
		t.Fatalf("summary: %q", s)
	}

	// Dirty the tree.
	tools.RunShell(ctx, dir, "sh -c 'echo change >> a.txt && echo new > b.txt'", 10*time.Second)
	s = Summary(ctx, dir)
	if !strings.Contains(s, "a.txt") || !strings.Contains(s, "b.txt") {
		t.Fatalf("dirty summary missing files: %q", s)
	}
	if DiffStat(ctx, dir) == "" {
		t.Fatal("empty diffstat on dirty tree")
	}

	line, err := Commit(ctx, dir, "test: add change")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "test: add change") {
		t.Fatalf("log line: %q", line)
	}

	// Empty commit refused.
	if _, err := Commit(ctx, dir, "nothing"); err == nil {
		t.Fatal("empty commit should fail")
	}
}

func TestSnapshotCapturesWorkTreeWithoutTouchingCheckout(t *testing.T) {
	dir := gitRepo(t)
	ctx := context.Background()
	for _, cmd := range []string{
		"sh -c 'echo changed > a.txt'",
		"sh -c 'echo new > new.txt'",
		"sh -c 'echo junk > ignored.log'",
		"sh -c 'echo \"*.log\" > .gitignore'",
	} {
		if out, err := tools.RunShell(ctx, dir, cmd, 30*time.Second); err != nil {
			t.Fatalf("%s: %v %s", cmd, err, out)
		}
	}
	headBefore, _ := git(ctx, dir, "rev-parse HEAD")
	statusBefore, _ := git(ctx, dir, "status --porcelain")

	branch, err := Snapshot(ctx, dir, "be-code: restore point before init")
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !strings.HasPrefix(branch, "be-code/pre-init/") {
		t.Fatalf("branch = %q, want be-code/pre-init/<stamp>", branch)
	}

	// The checkout is untouched: same HEAD, same status (new.txt still untracked,
	// a.txt still unstaged), nothing added to the real index.
	if head, _ := git(ctx, dir, "rev-parse HEAD"); head != headBefore {
		t.Fatalf("HEAD moved: %s -> %s", headBefore, head)
	}
	if status, _ := git(ctx, dir, "status --porcelain"); status != statusBefore {
		t.Fatalf("status changed:\nbefore:\n%s\nafter:\n%s", statusBefore, status)
	}
	if cur, _ := git(ctx, dir, "branch --show-current"); cur != "main" {
		t.Fatalf("current branch = %q, want main", cur)
	}

	// The branch holds the modified file, the untracked file, and not the ignored one,
	// and descends from the HEAD it was taken on.
	files, _ := git(ctx, dir, "ls-tree -r --name-only "+branch)
	for _, want := range []string{"a.txt", "new.txt", ".gitignore"} {
		if !strings.Contains(files, want) {
			t.Errorf("snapshot lacks %s: %q", want, files)
		}
	}
	if strings.Contains(files, "ignored.log") {
		t.Errorf("snapshot includes ignored file: %q", files)
	}
	if content, _ := git(ctx, dir, "show "+branch+":a.txt"); content != "changed" {
		t.Errorf("a.txt in snapshot = %q, want %q", content, "changed")
	}
	if parent, _ := git(ctx, dir, "rev-parse "+branch+"^"); parent != headBefore {
		t.Errorf("snapshot parent = %s, want HEAD %s", parent, headBefore)
	}
	if msg, _ := git(ctx, dir, "log -1 --format=%s "+branch); msg != "be-code: restore point before init" {
		t.Errorf("message = %q", msg)
	}
}

func TestSnapshotTwiceKeepsBothBranches(t *testing.T) {
	dir := gitRepo(t)
	ctx := context.Background()
	first, err := Snapshot(ctx, dir, "one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := Snapshot(ctx, dir, "two")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("second snapshot reused branch %q; the initial restore point must never be moved", first)
	}
	for _, b := range []string{first, second} {
		if _, err := git(ctx, dir, "rev-parse --verify refs/heads/"+b); err != nil {
			t.Errorf("branch %s missing: %v", b, err)
		}
	}
}

func TestSnapshotInEmptyRepoAndOutsideRepo(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	ctx := context.Background()
	empty := t.TempDir()
	for _, cmd := range []string{"git init -q -b main", "sh -c 'echo x > f.txt'"} {
		if out, err := tools.RunShell(ctx, empty, cmd, 30*time.Second); err != nil {
			t.Fatalf("%s: %v %s", cmd, err, out)
		}
	}
	branch, err := Snapshot(ctx, empty, "first")
	if err != nil {
		t.Fatalf("Snapshot in repo with no commits: %v", err)
	}
	if files, _ := git(ctx, empty, "ls-tree -r --name-only "+branch); !strings.Contains(files, "f.txt") {
		t.Errorf("snapshot in empty repo lacks f.txt: %q", files)
	}
	if head, err := git(ctx, empty, "rev-parse --verify -q HEAD"); err == nil {
		t.Errorf("snapshot created HEAD in an unborn repo: %s", head)
	}

	if _, err := Snapshot(ctx, t.TempDir(), "x"); err == nil {
		t.Fatal("Snapshot outside a repo should fail")
	}
}
