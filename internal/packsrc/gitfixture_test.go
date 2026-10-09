package packsrc

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func git(t *testing.T, dir string, argv ...string) string {
	t.Helper()
	cmd := exec.Command("git", argv...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", argv, dir, err, out)
	}
	return string(out)
}

// packFiles writes a minimal but valid pack: a manifest, a core, and an
// executable wrapper, which is what Review Focus 1 is about.
func packFiles(t *testing.T, root, name, version string) {
	t.Helper()
	mustMkdir(t, filepath.Join(root, "tools"))
	mustWrite(t, filepath.Join(root, "pack.json"), `{
  "schema": 1,
  "name": "`+name+`",
  "version": "`+version+`",
  "description": "a pack for tests",
  "core": { "file": "core.md", "budget_tokens": 1500 }
}
`, 0o644)
	mustWrite(t, filepath.Join(root, "core.md"), "Rules for "+name+".\n", 0o644)
	mustWrite(t, filepath.Join(root, "tools", "check"), "#!/bin/sh\nexit 0\n", 0o755)
}

// packsRepo is a checkout shaped like be-skill-packs: packs/<name>/...
func packsRepo(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		packFiles(t, filepath.Join(dir, "packs", n), n, "0.1.0")
	}
	return dir
}

// gitPacksRepo is packsRepo with a git history, so it can be cloned.
func gitPacksRepo(t *testing.T, names ...string) (dir, head string) {
	t.Helper()
	dir = packsRepo(t, names...)
	git(t, dir, "init", "--quiet", "--initial-branch=main")
	git(t, dir, "config", "user.email", "fixture@example.com")
	git(t, dir, "config", "user.name", "Fixture")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "--quiet", "-m", "packs")
	head = trim(git(t, dir, "rev-parse", "HEAD"))
	return dir, head
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	mustMkdir(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
