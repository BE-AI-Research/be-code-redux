package tools

import (
	"context"
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
