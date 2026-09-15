package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func gitWorkspace(t *testing.T) *Registry {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	ctx := context.Background()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nfunc Alpha() int {\n\treturn 1\n}\n"), 0o644)
	for _, c := range []string{"git init -q -b main", "git config user.email t@t", "git config user.name t", "git add -A", "git commit -qm one"} {
		if out, err := RunShell(ctx, dir, c, 30*time.Second); err != nil {
			t.Fatalf("%s: %v %s", c, err, out)
		}
	}
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package a\n\nfunc Alpha() int {\n\treturn 2\n}\n\nfunc Beta() {}\n"), 0o644)
	for _, c := range []string{"git add -A", "git commit -qm two"} {
		RunShell(ctx, dir, c, 30*time.Second)
	}
	reg, err := NewRegistry(dir, func(string, string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func byName(ts []Tool, name string) Tool {
	for _, t := range ts {
		if t.Name() == name {
			return t
		}
	}
	return nil
}

func TestLookupFindsSymbolsWithFunctionContext(t *testing.T) {
	reg := gitWorkspace(t)
	ts := NewGitTools(reg, func() (string, string) { return "", "" }, false)
	if len(ts) != 4 {
		t.Fatalf("%d tools", len(ts))
	}
	r := byName(ts, "lookup").Run(context.Background(), map[string]any{"query": "Alpha", "symbol": true})
	if r.IsError || !strings.Contains(r.Content, "a.go:3") || !strings.Contains(r.Content, "return 2") {
		t.Fatalf("lookup: %+v", r)
	}
	if r := byName(ts, "lookup").Run(context.Background(), map[string]any{}); !r.IsError || r.Content != "lookup needs a query" {
		t.Fatalf("empty: %+v", r)
	}
	if r := byName(ts, "lookup").Run(context.Background(), map[string]any{"query": "Alp.a", "regex": true}); r.IsError || !strings.Contains(r.Content, "a.go:3") {
		t.Fatalf("regex: %+v", r)
	}
}

func TestHistoryModes(t *testing.T) {
	reg := gitWorkspace(t)
	h := byName(NewGitTools(reg, nil, false), "history")
	ctx := context.Background()
	if r := h.Run(ctx, map[string]any{"path": "a.go", "symbol": "Alpha"}); r.IsError || !strings.Contains(r.Content, "two") || !strings.Contains(r.Content, "return 2") {
		t.Fatalf("symbol: %+v", r)
	}
	if r := h.Run(ctx, map[string]any{"path": "a.go", "query": "Beta"}); r.IsError || !strings.Contains(r.Content, "two") || strings.Contains(r.Content, "one\n") {
		t.Fatalf("pickaxe: %+v", r)
	}
	if r := h.Run(ctx, map[string]any{"path": "a.go", "blame": true, "lines": "3,4"}); r.IsError || !strings.Contains(r.Content, "return 2") {
		t.Fatalf("blame: %+v", r)
	}
	if r := h.Run(ctx, map[string]any{"path": "a.go"}); !r.IsError || !strings.Contains(r.Content, "one of symbol, lines, query or blame") {
		t.Fatalf("no mode: %+v", r)
	}
}

func TestShowAndChanges(t *testing.T) {
	reg := gitWorkspace(t)
	ts := NewGitTools(reg, func() (string, string) {
		out, _ := RunShell(context.Background(), reg.Root, "git rev-parse HEAD~1", 10*time.Second)
		return strings.TrimSpace(out), ""
	}, false)
	ctx := context.Background()
	if r := byName(ts, "show").Run(ctx, map[string]any{"rev": "HEAD~1", "path": "a.go"}); r.IsError || !strings.Contains(r.Content, "return 1") || strings.Contains(r.Content, "Beta") {
		t.Fatalf("show: %+v", r)
	}
	if r := byName(ts, "show").Run(ctx, map[string]any{"rev": "HEAD~1", "path": "../etc/passwd"}); !r.IsError {
		t.Fatal("show escaped the root")
	}
	r := byName(ts, "changes").Run(ctx, map[string]any{})
	if r.IsError || !strings.Contains(r.Content, "a.go") || !strings.Contains(r.Content, "1 file changed") {
		t.Fatalf("changes since baseline: %+v", r)
	}
	r = byName(ts, "changes").Run(ctx, map[string]any{"path": "a.go"})
	if r.IsError || !strings.Contains(r.Content, "+func Beta") {
		t.Fatalf("changes diff: %+v", r)
	}
	if r := byName(ts, "changes").Run(ctx, map[string]any{"since": "HEAD"}); r.IsError || !strings.Contains(r.Content, "no changes") {
		t.Fatalf("clean: %+v", r)
	}
}

func TestGitToolsOutsideARepository(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\nfunc Only() {}\n"), 0o644)
	reg, _ := NewRegistry(dir, func(string, string) bool { return true })
	ts := NewGitTools(reg, nil, false)
	if r := byName(ts, "lookup").Run(context.Background(), map[string]any{"query": "Only"}); r.IsError || !strings.Contains(r.Content, "x.go:2") {
		t.Fatalf("lookup fallback: %+v", r)
	}
	for _, name := range []string{"history", "show", "changes"} {
		if r := byName(ts, name).Run(context.Background(), map[string]any{"path": "x.go", "symbol": "Only"}); !r.IsError || r.Content != "not a git repository" {
			t.Fatalf("%s outside repo: %+v", name, r)
		}
	}
	if len(NewGitTools(reg, nil, true)) != 1 {
		t.Fatal("minimal must register lookup only")
	}
}

// ---- fix round 1 -----------------------------------------------------------

// gitSubWorkspace builds a repository whose workspace root is a *subdirectory*,
// with a same-named file at the repository top, so the confinement fixes
// (cwd-relative show, pathspec magic, repo-wide diff) have something to escape to.
func gitSubWorkspace(t *testing.T) *Registry {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	ctx := context.Background()
	os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("package top\n\n// TOPMARKER\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "toplevel.go"), []byte("package top\n\nfunc TopOnly() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "a.go"), []byte("package sub\n\n// SUBMARKER\nfunc SubOnly() {}\n"), 0o644)
	for _, c := range []string{"git init -q -b main", "git config user.email t@t", "git config user.name t", "git add -A", "git commit -qm one"} {
		if out, err := RunShell(ctx, dir, c, 30*time.Second); err != nil {
			t.Fatalf("%s: %v %s", c, err, out)
		}
	}
	os.WriteFile(filepath.Join(dir, "toplevel.go"), []byte("package top\n\nfunc TopOnly() {}\n\nfunc TopNew() {}\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "sub", "a.go"), []byte("package sub\n\n// SUBMARKER\nfunc SubOnly() {}\n\nfunc SubNew() {}\n"), 0o644)
	for _, c := range []string{"git add -A", "git commit -qm two"} {
		RunShell(ctx, dir, c, 30*time.Second)
	}
	reg, err := NewRegistry(filepath.Join(dir, "sub"), func(string, string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// Item 1: git is run as an argument vector, so shell metacharacters in a
// query are data. Nothing is executed and the literal text is found.
func TestLookupQueryIsNeverShellInterpreted(t *testing.T) {
	reg := gitWorkspace(t)
	ctx := context.Background()
	os.WriteFile(filepath.Join(reg.Root, "q.go"), []byte("package a\n\n// marker it's $(x) `y` end\n"), 0o644)
	for _, c := range []string{"git add -A", "git commit -qm three"} {
		RunShell(ctx, reg.Root, c, 30*time.Second)
	}
	lookup := byName(NewGitTools(reg, nil, false), "lookup")
	if r := lookup.Run(ctx, map[string]any{"query": "it's $(x)"}); r.IsError || !strings.Contains(r.Content, "q.go:3") {
		t.Fatalf("quoted literal: %+v", r)
	}
	if r := lookup.Run(ctx, map[string]any{"query": "`y` end"}); r.IsError || !strings.Contains(r.Content, "q.go:3") {
		t.Fatalf("backticks: %+v", r)
	}
	// A query holding a newline must not be split into a second command,
	// and substitutions must not run: no file may appear.
	for _, q := range []string{"$(touch pwned)", "`touch pwned2`", "zzq1\nzzq2", "; touch pwned3"} {
		if r := lookup.Run(ctx, map[string]any{"query": q}); r.IsError {
			t.Fatalf("query %q errored: %+v", q, r)
		}
	}
	for _, f := range []string{"pwned", "pwned2", "pwned3"} {
		if _, err := os.Stat(filepath.Join(reg.Root, f)); err == nil {
			t.Fatalf("%s was created: the query reached a shell", f)
		}
	}
}

// Item 2: "rev:path" is repo-top-relative; show must read the workspace's file.
func TestShowIsRelativeToTheWorkspaceNotTheRepoTop(t *testing.T) {
	reg := gitSubWorkspace(t)
	show := byName(NewGitTools(reg, nil, false), "show")
	r := show.Run(context.Background(), map[string]any{"path": "a.go"})
	if r.IsError || !strings.Contains(r.Content, "SUBMARKER") || strings.Contains(r.Content, "TOPMARKER") {
		t.Fatalf("show read the repo top: %+v", r)
	}
}

// Item 3: a pathspec beginning with ":" is git magic and escapes the root.
func TestPathspecMagicIsRejected(t *testing.T) {
	reg := gitSubWorkspace(t)
	ts := NewGitTools(reg, nil, false)
	ctx := context.Background()
	cases := []struct {
		tool string
		args map[string]any
		want string
	}{
		{"lookup", map[string]any{"query": "TOPMARKER", "path": ":/a.go"}, "bad path"},
		{"history", map[string]any{"path": ":/a.go", "query": "TopOnly"}, "bad path"},
		{"changes", map[string]any{"path": ":/a.go"}, "bad path"},
		{"show", map[string]any{"path": ":(top)a.go"}, "bad path"},
		// A revision is pathspec magic just as readily as a path is:
		// "show" builds "<rev>:./<path>" and "changes" passes since
		// straight to git diff.
		{"show", map[string]any{"path": "a.go", "rev": ":(exclude)zzz"}, "bad revision"},
		{"changes", map[string]any{"since": ":(exclude)zzz"}, "bad revision"},
	}
	for _, c := range cases {
		r := byName(ts, c.tool).Run(ctx, c.args)
		if !r.IsError || r.Content != c.want {
			t.Fatalf("%s took pathspec magic: %+v", c.tool, r)
		}
		if strings.Contains(r.Content, "TOPMARKER") || strings.Contains(r.Content, "toplevel") {
			t.Fatalf("%s leaked a file outside the root: %+v", c.tool, r)
		}
	}
	// The ordinary case still works: the nested workspace's own a.go, not
	// the repository top's.
	r := byName(ts, "show").Run(ctx, map[string]any{"path": "a.go", "rev": "HEAD"})
	if r.IsError || !strings.Contains(r.Content, "SUBMARKER") {
		t.Fatalf("show HEAD: %+v", r)
	}
	if strings.Contains(r.Content, "TOPMARKER") {
		t.Fatalf("show read the top-level a.go: %+v", r)
	}
}

// Item 4: the diff stat is confined to the workspace, not the whole repository.
func TestChangesStaysInsideTheWorkspace(t *testing.T) {
	reg := gitSubWorkspace(t)
	changes := byName(NewGitTools(reg, nil, false), "changes")
	r := changes.Run(context.Background(), map[string]any{"since": "HEAD~1"})
	if r.IsError || !strings.Contains(r.Content, "sub/a.go") {
		t.Fatalf("changes: %+v", r)
	}
	if strings.Contains(r.Content, "toplevel.go") {
		t.Fatalf("changes listed a file outside the workspace: %+v", r)
	}
}

// Item 5: a file the model just created is findable before it is staged.
func TestLookupFindsUntrackedFiles(t *testing.T) {
	reg := gitWorkspace(t)
	os.WriteFile(filepath.Join(reg.Root, "fresh.go"), []byte("package a\n\nfunc FreshlyMade() {}\n"), 0o644)
	r := byName(NewGitTools(reg, nil, false), "lookup").Run(context.Background(), map[string]any{"query": "FreshlyMade"})
	if r.IsError || !strings.Contains(r.Content, "fresh.go:3") {
		t.Fatalf("untracked: %+v", r)
	}
}

// Item 6: a partial result keeps the cause that cut it short.
func TestGitErrLabelsPartialOutput(t *testing.T) {
	r := gitErr(fmt.Errorf("timed out after 20s"), "partial output\n")
	if !r.IsError || r.Content != "partial output\n(timed out after 20s)" {
		t.Fatalf("partial: %+v", r)
	}
	if r := gitErr(fmt.Errorf("exit status 128"), ""); r.Content != "exit status 128" {
		t.Fatalf("no output: %+v", r)
	}
}

// Item 7: the non-repo fallback quotes a literal query for the regex search,
// and honours the "dir" alias for the path.
func TestLookupFallbackQuotesAndForwardsDir(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "pkg"), 0o755)
	os.WriteFile(filepath.Join(dir, "pkg", "x.go"), []byte("package x\nfunc Only() {}\n"), 0o644)
	reg, _ := NewRegistry(dir, func(string, string) bool { return true })
	lookup := byName(NewGitTools(reg, nil, true), "lookup")
	r := lookup.Run(context.Background(), map[string]any{"query": "Only()", "dir": "pkg"})
	if r.IsError || !strings.Contains(r.Content, "x.go:2") {
		t.Fatalf("fallback: %+v", r)
	}
}

// Item 8: -L arguments are validated before they reach git.
func TestHistoryValidatesLinesAndSymbol(t *testing.T) {
	reg := gitWorkspace(t)
	h := byName(NewGitTools(reg, nil, false), "history")
	ctx := context.Background()
	if r := h.Run(ctx, map[string]any{"path": "a.go", "lines": "3-4"}); !r.IsError || r.Content != "lines must be a,b" {
		t.Fatalf("lines: %+v", r)
	}
	if r := h.Run(ctx, map[string]any{"path": "a.go", "symbol": "Alpha:../../etc/passwd"}); !r.IsError || r.Content != "bad symbol" {
		t.Fatalf("symbol: %+v", r)
	}
}

// Item 9: a model that sends 1 for a boolean still gets the behaviour.
func TestLookupAcceptsNumericBoolean(t *testing.T) {
	reg := gitWorkspace(t)
	r := byName(NewGitTools(reg, nil, false), "lookup").Run(context.Background(), map[string]any{"query": "Alpha", "symbol": float64(1)})
	if r.IsError || !strings.Contains(r.Content, "return 2") {
		t.Fatalf("numeric symbol: %+v", r)
	}
}
