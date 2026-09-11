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
