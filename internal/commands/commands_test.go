package commands

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAndExpand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ws := t.TempDir()
	dir := filepath.Join(ws, ".becode", "commands")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "review.md"),
		[]byte("Review the following for security issues:\n\n$ARGS"), 0o644)
	os.WriteFile(filepath.Join(dir, "tidy.md"),
		[]byte("Run gofmt and go vet, then fix anything they flag."), 0o644)

	cmds := Load(ws)
	if len(cmds) != 2 {
		t.Fatalf("loaded %d commands", len(cmds))
	}
	got := cmds["review"].Expand("internal/auth/jwt.go")
	if got != "Review the following for security issues:\n\ninternal/auth/jwt.go" {
		t.Fatalf("expand: %q", got)
	}
	// No $ARGS: args appended.
	got = cmds["tidy"].Expand("focus on cmd/")
	if got != "Run gofmt and go vet, then fix anything they flag.\n\nfocus on cmd/" {
		t.Fatalf("expand: %q", got)
	}
	// No args, no marker: template as-is.
	if cmds["tidy"].Expand("") != "Run gofmt and go vet, then fix anything they flag." {
		t.Fatal("bare expand changed template")
	}
}

func TestWorkspaceShadowsGlobal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	gdir := filepath.Join(home, ".be-code", "commands")
	os.MkdirAll(gdir, 0o755)
	os.WriteFile(filepath.Join(gdir, "deploy.md"), []byte("global deploy"), 0o644)

	ws := t.TempDir()
	wdir := filepath.Join(ws, ".becode", "commands")
	os.MkdirAll(wdir, 0o755)
	os.WriteFile(filepath.Join(wdir, "deploy.md"), []byte("workspace deploy"), 0o644)

	cmds := Load(ws)
	if cmds["deploy"].Template != "workspace deploy" || cmds["deploy"].Source != "workspace" {
		t.Fatalf("shadowing broken: %+v", cmds["deploy"])
	}
}
