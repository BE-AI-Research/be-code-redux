package workledger

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitRepo(t *testing.T) string {
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
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "--quiet", "-m", "seed")
	return dir
}

// A workspace file goes to a git branch, and the HowTo line is the command
// that brings it back — the same shape /init prints.
func TestKeepOldSnapshotsAWorkspaceFileToABranch(t *testing.T) {
	ws := gitRepo(t)
	f := filepath.Join(ws, "doc.md")
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := KeepOld(context.Background(), ws, f)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != RestoreGit {
		t.Fatalf("kind = %v, want RestoreGit", got.Kind)
	}
	if !strings.HasPrefix(got.Branch, "be-code/pre-overwrite/") {
		t.Errorf("branch = %q", got.Branch)
	}
	if !strings.Contains(got.HowTo, "git restore") || !strings.Contains(got.HowTo, got.Branch) {
		t.Errorf("HowTo must be the command that restores it: %q", got.HowTo)
	}
}

// Review Focus 1: outside a repository there is no branch, so the old version
// goes to a sidecar and the caller can still tell the person where it is.
func TestKeepOldOutsideARepositoryUsesASidecar(t *testing.T) {
	ws := t.TempDir() // not a repo
	f := filepath.Join(ws, "doc.md")
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := KeepOld(context.Background(), ws, f)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != RestoreSidecar {
		t.Fatalf("kind = %v, want RestoreSidecar", got.Kind)
	}
	body, rerr := os.ReadFile(got.Sidecar)
	if rerr != nil {
		t.Fatalf("the sidecar is not there: %v", rerr)
	}
	if string(body) != "theirs\n" {
		t.Errorf("the sidecar holds %q", body)
	}
	if !strings.Contains(got.HowTo, got.Sidecar) {
		t.Errorf("HowTo must name the sidecar: %q", got.HowTo)
	}
}

// A dotdir file is in no repository, so it always goes to a sidecar even when
// the workspace is one.
func TestKeepOldOfAFileOutsideTheWorkspaceUsesASidecar(t *testing.T) {
	ws := gitRepo(t)
	outside := filepath.Join(t.TempDir(), "packs.json")
	if err := os.WriteFile(outside, []byte(`{"forced":{"a":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := KeepOld(context.Background(), ws, outside)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != RestoreSidecar {
		t.Fatalf("kind = %v, want RestoreSidecar for a file outside the workspace", got.Kind)
	}
	if st, serr := os.Stat(got.Sidecar); serr != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("a dotdir sidecar must be 0600: %v %v", st, serr)
	}
}

// Review Focus 1, the hard half: with no restore point available at all, this
// must fail. The caller's unattended write is only acceptable because it is
// recoverable (spec §12).
func TestKeepOldFailsWhenItCanKeepNothing(t *testing.T) {
	ws := t.TempDir()
	sub := filepath.Join(ws, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(sub, "doc.md")
	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	if err := os.Chmod(sub, 0o500); err != nil { // no new files beside it
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sub, 0o755) })

	if _, err := KeepOld(context.Background(), ws, f); err == nil {
		t.Fatal("KeepOld reported success with nowhere to keep the old version")
	}
}

// A file that does not exist has nothing to keep, and that is not a failure:
// there is no old version to lose.
func TestKeepOldOfAMissingFileIsANoop(t *testing.T) {
	ws := t.TempDir()
	got, err := KeepOld(context.Background(), ws, filepath.Join(ws, "nope.md"))
	if err != nil {
		t.Fatalf("a missing file is nothing to keep, not an error: %v", err)
	}
	if got.Sidecar != "" || got.Branch != "" {
		t.Errorf("nothing should have been kept: %+v", got)
	}
}

func TestPruneSidecarsDropsOldOnesOnly(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "packs.json.overwritten-20200101-000000")
	recent := filepath.Join(dir, "packs.json.overwritten-20991231-235959")
	other := filepath.Join(dir, "packs.json")
	for _, p := range []string{old, recent, other} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	long := time.Now().Add(-365 * 24 * time.Hour)
	if err := os.Chtimes(old, long, long); err != nil {
		t.Fatal(err)
	}

	n := PruneSidecars(dir, 168*time.Hour)
	if n != 1 {
		t.Errorf("pruned %d, want 1", n)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("the old sidecar is still there")
	}
	for _, p := range []string{recent, other} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed and should not have been", filepath.Base(p))
		}
	}
}
