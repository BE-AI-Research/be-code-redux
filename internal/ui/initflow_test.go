package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
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

func TestRunInitWritesBackupAndRefreshes(t *testing.T) {
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
