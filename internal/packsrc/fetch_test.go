package packsrc

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFetchFromAPacksCheckout(t *testing.T) {
	src := packsRepo(t, "assist", "ue5")
	s, err := Resolve(src, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Fetch(context.Background(), s, "assist")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(got.Dir)
	if got.Pack != "assist" || got.Version != "0.1.0" {
		t.Errorf("got %q %q, want assist 0.1.0", got.Pack, got.Version)
	}
	if _, err := os.Stat(filepath.Join(got.Dir, "core.md")); err != nil {
		t.Errorf("core.md did not come across: %v", err)
	}
	if got.Commit != "" {
		t.Errorf("a path source has no commit, got %q", got.Commit)
	}
}

// A directory that is itself a pack, rather than a packs checkout.
func TestFetchFromABarePackDirectory(t *testing.T) {
	dir := t.TempDir()
	packFiles(t, dir, "assist", "0.2.0")
	s, err := Resolve(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Fetch(context.Background(), s, "")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(got.Dir)
	if got.Pack != "assist" || got.Version != "0.2.0" {
		t.Errorf("got %q %q, want assist 0.2.0", got.Pack, got.Version)
	}
}

// Review Focus 1: pack tools are executables, and a copy that drops the mode
// breaks every check at the first build.
func TestFetchKeepsTheExecutableBit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no executable bit on Windows")
	}
	src := packsRepo(t, "assist")
	s, _ := Resolve(src, "")
	got, err := Fetch(context.Background(), s, "assist")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(got.Dir)
	st, err := os.Stat(filepath.Join(got.Dir, "tools", "check"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o111 == 0 {
		t.Errorf("tools/check came across as %v: a pack's wrappers must stay executable", st.Mode().Perm())
	}
}

// Review Focus 3: a fetched pack is untrusted content. A symlink pointing out
// of the pack would be read, or run, by a check.
func TestFetchRefusesASymlinkOutOfThePack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privilege on Windows")
	}
	src := packsRepo(t, "assist")
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "packs", "assist", "tools", "escape")); err != nil {
		t.Fatal(err)
	}
	s, _ := Resolve(src, "")
	_, err := Fetch(context.Background(), s, "assist")
	if err == nil {
		t.Fatal("a pack containing a symlink out of itself was fetched")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("the error should say what was wrong: %v", err)
	}
}

func TestFetchFromAGitRepo(t *testing.T) {
	src, head := gitPacksRepo(t, "assist")
	s, err := Resolve("file://"+src, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Fetch(context.Background(), s, "assist")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(got.Dir)
	if got.Pack != "assist" {
		t.Errorf("pack = %q", got.Pack)
	}
	if got.Commit != head {
		t.Errorf("commit = %q, want %q", got.Commit, head)
	}
}

func TestFetchAtATag(t *testing.T) {
	src, _ := gitPacksRepo(t, "assist")
	git(t, src, "tag", "v1.0.0")
	// Move main on, so "at the tag" and "at the default branch" differ.
	mustWrite(t, filepath.Join(src, "packs", "assist", "core.md"), "Changed after the tag.\n", 0o644)
	git(t, src, "add", "-A")
	git(t, src, "commit", "--quiet", "-m", "after the tag")

	s, _ := Resolve("file://"+src, "v1.0.0")
	got, err := Fetch(context.Background(), s, "assist")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(got.Dir)
	body, err := os.ReadFile(filepath.Join(got.Dir, "core.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "Changed after the tag") {
		t.Error("fetched the default branch rather than the tag")
	}
}

// Review Focus 4: `clone --depth 1 --branch <sha>` is refused by most
// servers, so a commit SHA needs its own path.
func TestFetchAtACommitSHA(t *testing.T) {
	src, head := gitPacksRepo(t, "assist")
	mustWrite(t, filepath.Join(src, "packs", "assist", "core.md"), "Later.\n", 0o644)
	git(t, src, "add", "-A")
	git(t, src, "commit", "--quiet", "-m", "later")

	s, _ := Resolve("file://"+src, head)
	got, err := Fetch(context.Background(), s, "assist")
	if err != nil {
		t.Fatalf("a commit SHA must be fetchable: %v", err)
	}
	defer os.RemoveAll(got.Dir)
	if got.Commit != head {
		t.Errorf("commit = %q, want the SHA asked for %q", got.Commit, head)
	}
	body, _ := os.ReadFile(filepath.Join(got.Dir, "core.md"))
	if strings.Contains(string(body), "Later") {
		t.Error("fetched a later commit than the SHA asked for")
	}
}

func TestFetchNamesWhatTheSourceHasWhenThePackIsMissing(t *testing.T) {
	src := packsRepo(t, "assist", "ue5")
	s, _ := Resolve(src, "")
	_, err := Fetch(context.Background(), s, "nope")
	if err == nil {
		t.Fatal("fetching a pack the source does not have succeeded")
	}
	for _, want := range []string{"nope", "assist", "ue5"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should name %q: %v", want, err)
		}
	}
}

func TestFetchRefusesABadPackName(t *testing.T) {
	src := packsRepo(t, "assist")
	s, _ := Resolve(src, "")
	if _, err := Fetch(context.Background(), s, "../../etc"); err == nil {
		t.Fatal("a pack name that escapes the directory was accepted")
	}
}

func TestListReportsEveryPack(t *testing.T) {
	src := packsRepo(t, "assist", "ue5", "ui-web")
	s, _ := Resolve(src, "")
	got, err := List(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d packs, want 3: %+v", len(got), got)
	}
	seen := map[string]string{}
	for _, a := range got {
		seen[a.Name] = a.Version
	}
	for _, n := range []string{"assist", "ue5", "ui-web"} {
		if seen[n] != "0.1.0" {
			t.Errorf("%s: version %q, want 0.1.0", n, seen[n])
		}
	}
}

func TestGitMissingSaysSo(t *testing.T) {
	src, _ := gitPacksRepo(t, "assist")
	s, _ := Resolve("file://"+src, "")
	t.Setenv("PATH", t.TempDir()) // a PATH with no git on it
	_, err := Fetch(context.Background(), s, "assist")
	if err == nil {
		t.Fatal("fetching with no git on PATH succeeded")
	}
	if !strings.Contains(err.Error(), "git") {
		t.Errorf("the error should name git: %v", err)
	}
}

func TestEveryExecCommandIsHidden(t *testing.T) {
	srcs, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range srcs {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		body := string(b)
		if !strings.Contains(body, "exec.Command") {
			continue
		}
		if !strings.Contains(body, "procattr.Hide") {
			t.Errorf("%s runs a child process without procattr.Hide: on Windows the detached "+
				"session host would open a visible console window for it", f)
		}
	}
}

// Both clone forms put "--" before their positionals, so a spec or a ref that
// git would otherwise read as an option is a positional and nothing else.
func TestClonePutsADoubleDashBeforeItsPositionals(t *testing.T) {
	b, err := os.ReadFile("fetch.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), `"--", s.Spec`); n < 2 {
		t.Errorf("clone passes s.Spec after \"--\" in %d place(s); both the SHA form and the branch form must", n)
	}
}

// Two directories whose manifests claim one name are an error, never a pick
// by ReadDir order: installing the wrong one writes somebody else's rules into
// the system prompt.
func TestLocateRefusesTwoDirectoriesClaimingOneName(t *testing.T) {
	dir := packsRepo(t, "assist")
	packFiles(t, filepath.Join(dir, "packs", "other"), "assist", "0.2.0")
	_, err := Fetch(context.Background(), Source{Spec: dir, Kind: KindPath}, "assist")
	if err == nil || !strings.Contains(err.Error(), "assist") || !strings.Contains(err.Error(), "packs/other") {
		t.Fatalf("want an error naming both directories, got %v", err)
	}
}
