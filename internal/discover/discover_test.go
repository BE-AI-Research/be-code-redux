package discover

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func goFixture(t *testing.T) string {
	root := t.TempDir()
	write(t, root, "go.mod", "module example.com/app\n\ngo 1.22\n")
	write(t, root, "cmd/app/main.go", "package main\n\nfunc main() {}\n")
	write(t, root, "internal/core/core.go", "package core\n\nfunc Do() {}\n")
	write(t, root, "internal/core/core_test.go", "package core\n")
	write(t, root, "README.md", "# App\n\nDoes things.\n")
	write(t, root, "Makefile", "build:\n\tgo build ./...\n")
	write(t, root, "vendor/x/x.go", "package x\n")
	write(t, root, "node_modules/y/index.js", "x")
	return root
}

func TestScanGoProject(t *testing.T) {
	f, err := Scan(goFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != "go" || len(f.Checks) == 0 || !strings.Contains(strings.Join(f.Checks, " "), "go test") {
		t.Fatalf("kind/checks: %q %v", f.Kind, f.Checks)
	}
	if f.Languages[0].Ext != ".go" || f.Languages[0].Files != 3 {
		t.Fatalf("languages: %+v (vendor/node_modules must be skipped)", f.Languages)
	}
	for _, want := range []string{"go.mod", "README.md", "Makefile"} {
		if !contains(f.KeyFiles, want) {
			t.Fatalf("key files lack %s: %v", want, f.KeyFiles)
		}
	}
	if !contains(f.EntryPoints, "cmd/app") {
		t.Fatalf("entry points: %v", f.EntryPoints)
	}
	if !contains(f.TestDirs, "internal/core (1 _test.go)") {
		t.Fatalf("test dirs: %v", f.TestDirs)
	}
	if !strings.HasPrefix(f.ReadmeHead, "# App") {
		t.Fatalf("readme head %q", f.ReadmeHead)
	}
	if f.Files != 6 { // go.mod, main.go, core.go, core_test.go, README.md, Makefile
		t.Fatalf("files %d", f.Files)
	}
	md := f.Markdown()
	for _, want := range []string{"## Languages", ".go", "## Build and test", "go test ./...", "## Layout", "internal/core"} {
		if !strings.Contains(md, want) {
			t.Fatalf("fact sheet lacks %q:\n%s", want, md)
		}
	}
	names := f.Names()
	for _, want := range []string{"cmd/app", "go.mod", "go test ./...", "internal"} {
		if !contains(names, want) {
			t.Fatalf("names lack %q: %v", want, names)
		}
	}
}

func TestScanNodeAndPython(t *testing.T) {
	root := t.TempDir()
	write(t, root, "package.json", `{"name":"web","main":"src/index.js","bin":{"web":"cli.js"},"scripts":{"build":"tsc","test":"vitest"}}`)
	write(t, root, "src/index.js", "x")
	write(t, root, ".eslintrc.json", "{}")
	f, _ := Scan(root)
	if f.Kind != "node" || !contains(f.EntryPoints, "src/index.js") || !contains(f.EntryPoints, "bin web") || !contains(f.EntryPoints, "script build") {
		t.Fatalf("node: %q %v", f.Kind, f.EntryPoints)
	}
	if !contains(f.Tooling, ".eslintrc.json") {
		t.Fatalf("tooling: %v", f.Tooling)
	}
	root = t.TempDir()
	write(t, root, "pyproject.toml", "[project]\nname = \"tool\"\n[project.scripts]\ntool = \"tool.cli:main\"\n[tool.black]\nline-length = 100\n")
	write(t, root, "tool/cli.py", "x")
	write(t, root, "tests/test_cli.py", "x")
	f, _ = Scan(root)
	if f.Kind != "python" || !contains(f.EntryPoints, "script tool") || !contains(f.TestDirs, "tests") || !contains(f.Tooling, "pyproject [tool.black]") {
		t.Fatalf("python: %q %v %v %v", f.Kind, f.EntryPoints, f.TestDirs, f.Tooling)
	}
}

func TestScanEmptyAndGit(t *testing.T) {
	f, err := Scan(t.TempDir())
	if err != nil || f.Kind != "none" || f.Files != 0 {
		t.Fatalf("empty: %+v %v", f, err)
	}
	if !strings.Contains(f.Markdown(), "no source files") {
		t.Fatal("empty sheet must say so")
	}
	root := goFixture(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	for _, c := range [][]string{{"init", "-q"}, {"-c", "user.email=t@t", "-c", "user.name=t", "add", "."}, {"-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "-m", "first commit"}} {
		cmd := exec.Command("git", c...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", c, err, out)
		}
	}
	write(t, root, "dirty.go", "package main\n")
	f, _ = Scan(root)
	if f.Git.Branch == "" || len(f.Git.Recent) != 1 || f.Git.Recent[0] != "first commit" || f.Git.Dirty != 1 {
		t.Fatalf("git state: %+v", f.Git)
	}
}

func TestScanIsBounded(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 300; i++ {
		write(t, root, filepath.Join("gen", "f"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+".go"), "package gen\n")
	}
	maxFiles = 100
	defer func() { maxFiles = 20000 }()
	f, _ := Scan(root)
	if !f.Truncated || f.Files > 100 {
		t.Fatalf("bound: truncated=%v files=%d", f.Truncated, f.Files)
	}
	if !strings.Contains(f.Markdown(), "truncated") {
		t.Fatal("sheet must say it was truncated")
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
