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

// A pack's commands join a person's own, and a person's own always win: a
// pack arrives with somebody's repository or a dotdir install, and neither
// may redefine a command the person wrote themselves.
func TestMergeKeepsAPersonsOwnCommands(t *testing.T) {
	own := map[string]Command{
		"deploy": {Name: "deploy", Template: "my own deploy", Source: "workspace"},
	}
	got := Merge(own, map[string]string{
		"deploy":   "the pack's deploy",
		"ue-actor": "Create an Actor called $ARGS.",
	})
	if got["deploy"].Template != "my own deploy" {
		t.Errorf("a pack overrode a person's command: %q", got["deploy"].Template)
	}
	if got["deploy"].Source != "workspace" {
		t.Errorf("source changed: %q", got["deploy"].Source)
	}
	c, ok := got["ue-actor"]
	if !ok {
		t.Fatal("the pack's own command was not offered")
	}
	if c.Source != "pack" {
		t.Errorf("source = %q, want pack so /commands can say where it came from", c.Source)
	}
	if c.Expand("MyActor") != "Create an Actor called MyActor." {
		t.Errorf("a pack command does not expand $ARGS: %q", c.Expand("MyActor"))
	}
}

// No packs, nothing changes — including the nil map a session with no
// commands at all starts from.
func TestMergeWithNoPackCommandsIsInert(t *testing.T) {
	own := map[string]Command{"x": {Name: "x", Template: "t", Source: "global"}}
	if got := Merge(own, nil); len(got) != 1 || got["x"].Template != "t" {
		t.Errorf("got %+v", got)
	}
	if got := Merge(nil, nil); len(got) != 0 {
		t.Errorf("got %+v, want empty", got)
	}
	if got := Merge(nil, map[string]string{"a": "b"}); len(got) != 1 {
		t.Errorf("got %+v", got)
	}
}
