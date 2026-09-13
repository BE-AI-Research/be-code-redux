package discover

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func TestScanHonoursRootGitignore(t *testing.T) {
	root := t.TempDir()
	write(t, root, ".gitignore", "coverage/\nout\n!keep/\n")
	write(t, root, "coverage/report.go", "package coverage\n")
	write(t, root, "out/gen.go", "package out\n")
	write(t, root, "keep/keep.go", "package keep\n")
	write(t, root, "main.go", "package main\n")
	f, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if f.Files != 3 { // .gitignore, main.go, keep/keep.go (coverage/ and out/ skipped)
		t.Fatalf("files: %d (want coverage/ and out/ skipped)", f.Files)
	}
	for _, d := range f.Layout {
		if d.Path == "coverage" || d.Path == "out" {
			t.Fatalf("layout should not include ignored dir %q: %+v", d.Path, f.Layout)
		}
	}
	found := false
	for _, d := range f.Layout {
		if d.Path == "keep" {
			found = true
		}
	}
	if !found {
		t.Fatalf("negated gitignore line must not suppress keep/: %+v", f.Layout)
	}
}

func TestScanTimeBoundHitsDirectories(t *testing.T) {
	root := t.TempDir()
	// Purely directories, no files: the old code only checked the time
	// bound in the file branch, so a directory-only tree never tripped
	// Truncated no matter how long the walk took. Assert the bound fires
	// even here.
	for i := 0; i < 200; i++ {
		if err := os.MkdirAll(filepath.Join(root, fmt.Sprintf("d%03d", i), "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := maxWalk
	maxWalk = 0
	defer func() { maxWalk = old }()
	done := make(chan struct{})
	var f Facts
	go func() {
		f, _ = Scan(root)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Scan did not return promptly with maxWalk=0")
	}
	if !f.Truncated {
		t.Fatalf("expected Truncated with maxWalk=0: %+v", f)
	}
}

func TestCountLinesCapped(t *testing.T) {
	root := t.TempDir()
	p := filepath.Join(root, "big.txt")
	var b strings.Builder
	for b.Len() < 5<<20 {
		b.WriteString("line of text\n")
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan int)
	go func() { done <- countLines(p) }()
	select {
	case n := <-done:
		if n <= 0 {
			t.Fatalf("expected some lines counted, got %d", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("countLines did not return promptly on a 5 MiB file")
	}
}

func TestScanReadmeKeyFileCaseInsensitive(t *testing.T) {
	root := t.TempDir()
	write(t, root, "readme.md", "# lower\n")
	f, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(f.KeyFiles, "readme.md") {
		t.Fatalf("key files lack lowercase readme.md: %v", f.KeyFiles)
	}
	if !contains(f.Names(), "readme.md") {
		t.Fatalf("names lack lowercase readme.md: %v", f.Names())
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

// TestScanParsesMakefileTargets pins the shallow makefile parse: every real
// target from every makefile at the root, and none of the lines that only
// look like one (.PHONY and friends, a pattern rule, a variable
// assignment, a recipe line).
func TestScanParsesMakefileTargets(t *testing.T) {
	root := goFixture(t)
	write(t, root, "Makefile", strings.Join([]string{
		"VERSION := 1.2.3",
		"CFLAGS ?= -g",
		"",
		".PHONY: build test",
		"# a comment",
		"build:",
		"\tgo build ./...",
		"test: build",
		"\tgo test ./...",
		"%.o: %.c",
		"\tcc -c $<",
		".DEFAULT_GOAL := build",
		"$(BINARY):",
		"\ttouch $@",
	}, "\n")+"\n")
	write(t, root, "build.mk", "verify: vet build test\n\t@echo ok\nvet:\n\tgo vet ./...\n")

	f, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"build", "test", "verify", "vet"}
	if len(f.MakeTargets) != len(want) {
		t.Fatalf("targets %v, want %v", f.MakeTargets, want)
	}
	for i, w := range want {
		if f.MakeTargets[i] != w {
			t.Fatalf("targets %v, want %v", f.MakeTargets, want)
		}
	}
	for _, bad := range []string{"VERSION", "CFLAGS", ".PHONY", ".DEFAULT_GOAL", "%.o", "$(BINARY)"} {
		if contains(f.MakeTargets, bad) {
			t.Errorf("%q must not be a target: %v", bad, f.MakeTargets)
		}
	}
	if !contains(f.MakeFiles, "Makefile") || !contains(f.MakeFiles, "build.mk") {
		t.Fatalf("make files %v", f.MakeFiles)
	}
	if !strings.Contains(f.Markdown(), "## Make targets") || !strings.Contains(f.Markdown(), "`verify`") {
		t.Fatalf("fact sheet lacks the make targets:\n%s", f.Markdown())
	}
	names := f.Names()
	for _, want := range []string{"make build", "make -f build.mk verify", "make -f Makefile build", "go run .", "go test ./internal/core", "go vet ./..."} {
		if !contains(names, want) {
			t.Errorf("names lack %q", want)
		}
	}
}

// TestNamesCoverNodeCommandsAndFiles: the scripts and bins a package.json
// declares are citable invocations, and every discovered file is a citable
// name.
func TestNamesCoverNodeCommandsAndFiles(t *testing.T) {
	root := t.TempDir()
	write(t, root, "package.json", `{"name":"web","bin":{"web":"cli.js"},"scripts":{"build":"tsc","test":"vitest"}}`)
	write(t, root, "src/index.ts", "export {}\n")
	write(t, root, "src/util/fmt.ts", "export {}\n")

	f, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(f.NPMScripts, "build") || !contains(f.NPMScripts, "test") || !contains(f.NPMBins, "web") {
		t.Fatalf("package.json: scripts=%v bins=%v", f.NPMScripts, f.NPMBins)
	}
	names := f.Names()
	for _, want := range []string{"npm run build", "npm run test", "npm test", "npm install", "npx web", "src/index.ts", "src/util/fmt.ts", "package.json"} {
		if !contains(names, want) {
			t.Errorf("names lack %q", want)
		}
	}
}

// TestFilePathsAreBounded: FilePaths is every discovered file only up to
// maxNamedFiles, so a large repo cannot turn the names list into megabytes.
func TestFilePathsAreBounded(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < maxNamedFiles+50; i++ {
		write(t, root, fmt.Sprintf("gen/f%03d.go", i), "package gen\n")
	}
	f, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	if f.Files != maxNamedFiles+50 {
		t.Fatalf("files %d", f.Files)
	}
	if len(f.FilePaths) != maxNamedFiles {
		t.Fatalf("file paths %d, want %d", len(f.FilePaths), maxNamedFiles)
	}
}

// TestNotesFallbackFitsTheLimit: the fallback document is also the project
// notes, so it must fit the caller's cap, end at a line boundary and leave
// out the repo map — while keeping the part a session actually needs.
func TestNotesFallbackFitsTheLimit(t *testing.T) {
	root := goFixture(t)
	f, err := Scan(root)
	if err != nil {
		t.Fatal(err)
	}
	f.RepoMap = "## huge\n" + strings.Repeat("func Something()\n", 2000)
	if !strings.Contains(f.Markdown(), "## Symbols") {
		t.Fatal("the model's fact sheet must still carry the repo map")
	}
	const limit = 300
	out := f.NotesFallback(limit)
	if len(out) > limit {
		t.Fatalf("fallback is %d bytes, over the %d limit", len(out), limit)
	}
	if !strings.HasSuffix(out, "\n") {
		t.Fatalf("fallback must end at a line boundary:\n%q", out)
	}
	if strings.Contains(out, "## Symbols") || strings.Contains(out, "func Something()") {
		t.Fatalf("fallback must drop the repo map:\n%s", out)
	}
	if !strings.Contains(out, "## Build and test") || !strings.Contains(out, "go test ./...") {
		t.Fatalf("fallback lost the build/test section:\n%s", out)
	}
	// Under the limit it is the whole (symbol-less) sheet, untrimmed.
	if whole := f.NotesFallback(1 << 20); !strings.Contains(whole, "## Layout") || strings.Contains(whole, "## Symbols") {
		t.Fatalf("untrimmed fallback:\n%s", whole)
	}
}
