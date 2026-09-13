package ui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// scripted is a provider.Provider stand-in whose Chat reply is scripted per
// test. It embeds nullProvider (from repl_test.go) for the other methods.
type scripted struct {
	nullProvider
	reply func(provider.ChatRequest) string
}

func (s scripted) Chat(ctx context.Context, req provider.ChatRequest, onDelta provider.StreamFunc) (*provider.ChatResponse, error) {
	return &provider.ChatResponse{Content: s.reply(req)}, nil
}

func scriptedProvider(reply func(provider.ChatRequest) string) provider.Provider {
	return scripted{reply: reply}
}

func TestRunInitOutsideGitWritesBackupAndRefreshes(t *testing.T) {
	r := newTestREPL(t)
	root := r.Agent.Tools.Root
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n\ngo 1.22\n"), 0o644)
	os.MkdirAll(filepath.Join(root, "cmd/x"), 0o755)
	os.WriteFile(filepath.Join(root, "cmd/x/main.go"), []byte("package main\nfunc main(){}\n"), 0o644)
	os.WriteFile(filepath.Join(root, "BECODE.md"), []byte("old notes\n"), 0o644)
	r.Provider = scriptedProvider(func(req provider.ChatRequest) string {
		return "# X\n\nA Go tool. Build: `go build ./...`. Entry: cmd/x. Module file go.mod.\n"
	})
	r.Agent.Provider = r.Provider
	var previews []string
	path, err := RunInit(context.Background(), r.Agent, InitOptions{Root: root,
		Approve: func(p string) bool { previews = append(previews, p); return true }, Log: func(string) {}})
	if err != nil || path != filepath.Join(root, "BECODE.md") {
		t.Fatalf("path %q err %v", path, err)
	}
	if len(previews) != 1 || !strings.Contains(previews[0], "A Go tool") {
		t.Fatalf("approval preview: %v", previews)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "BECODE.md.bak")); string(b) != "old notes\n" {
		t.Fatalf("backup: %q", b)
	}
	if !strings.Contains(r.Agent.History.System.Content, "A Go tool") {
		t.Fatal("system prompt not refreshed with the new notes")
	}
}

func gitInRoot(t *testing.T, root string, cmds ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	var out string
	for _, cmd := range cmds {
		o, err := tools.RunShell(context.Background(), root, cmd, 30*time.Second)
		if err != nil {
			t.Fatalf("%s: %v %s", cmd, err, o)
		}
		out = strings.TrimSpace(o)
	}
	return out
}

func TestRunInitInGitRepoMakesRestoreBranchInsteadOfBackup(t *testing.T) {
	r := newTestREPL(t)
	root := r.Agent.Tools.Root
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(root, "BECODE.md"), []byte("old notes\n"), 0o644)
	gitInRoot(t, root, "git init -q -b main", "git config user.email t@t.local", "git config user.name t",
		"git add -A", "git commit -qm initial", "sh -c 'echo scratch > notes.txt'")
	r.Provider = scriptedProvider(func(provider.ChatRequest) string { return "# X\nUses go.mod; build with `go build ./...`.\n" })
	r.Agent.Provider = r.Provider
	var lines []string
	_, err := RunInit(context.Background(), r.Agent, InitOptions{Root: root, Log: func(l string) { lines = append(lines, l) }})
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "BECODE.md.bak")); statErr == nil {
		t.Fatal("BECODE.md.bak written inside a git repo; the restore branch replaces it")
	}
	branches := gitInRoot(t, root, "git for-each-ref --format='%(refname:short)' refs/heads/be-code/pre-init/")
	if branches == "" || strings.Contains(branches, "\n") {
		t.Fatalf("want exactly one restore branch, got %q", branches)
	}
	if old := gitInRoot(t, root, "git show "+branches+":BECODE.md"); old != "old notes" {
		t.Fatalf("restore branch BECODE.md = %q", old)
	}
	if untracked := gitInRoot(t, root, "git show "+branches+":notes.txt"); untracked != "scratch" {
		t.Fatalf("restore branch lacks the untracked file: %q", untracked)
	}
	if status := gitInRoot(t, root, "git status --porcelain"); !strings.Contains(status, "?? notes.txt") || !strings.Contains(status, "M BECODE.md") {
		t.Fatalf("checkout disturbed: %q", status)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "restore point: "+branches) || !strings.Contains(joined, "git restore --source="+branches+" --staged --worktree -- .") {
		t.Fatalf("log lines lack the restore point and command:\n%s", joined)
	}
	if strings.Contains(joined, ".bak") {
		t.Fatalf("log still mentions a .bak: %s", joined)
	}
}

func TestRunInitRejectionInGitRepoMakesNoBranch(t *testing.T) {
	r := newTestREPL(t)
	root := r.Agent.Tools.Root
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644)
	gitInRoot(t, root, "git init -q -b main", "git config user.email t@t.local", "git config user.name t", "git add -A", "git commit -qm initial")
	r.Provider = scriptedProvider(func(provider.ChatRequest) string { return "# X\nUses go.mod; see go build ./....\n" })
	r.Agent.Provider = r.Provider
	if _, err := RunInit(context.Background(), r.Agent, InitOptions{Root: root, Approve: func(string) bool { return false }}); err == nil {
		t.Fatal("expected rejection")
	}
	if b := gitInRoot(t, root, "git branch --list 'be-code/pre-init/*'"); b != "" {
		t.Fatalf("branch created for a rejected init: %q", b)
	}
}

func TestRunInitRespectsRejection(t *testing.T) {
	r := newTestREPL(t)
	root := r.Agent.Tools.Root
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644)
	r.Provider = scriptedProvider(func(provider.ChatRequest) string { return "# X\nUses go.mod; see go build ./....\n" })
	r.Agent.Provider = r.Provider
	_, err := RunInit(context.Background(), r.Agent, InitOptions{Root: root, Approve: func(string) bool { return false }})
	if err == nil || !strings.Contains(err.Error(), "rejected") {
		t.Fatalf("err %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "BECODE.md")); statErr == nil {
		t.Fatal("file written despite rejection")
	}
}

func TestNeedsInitHint(t *testing.T) {
	root := t.TempDir()
	if NeedsInitHint(root) {
		t.Fatal("empty workspace must not hint")
	}
	os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644)
	if !NeedsInitHint(root) {
		t.Fatal("go project without notes must hint")
	}
	os.WriteFile(filepath.Join(root, "CLAUDE.md"), []byte("x"), 0o644)
	if NeedsInitHint(root) {
		t.Fatal("CLAUDE.md counts as notes")
	}
}
