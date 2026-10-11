package ui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// Every subcommand that can raise a question must be blocking: Session.Ask
// relocks the mutex Update holds, and an inline question froze every attached
// terminal with the alt screen up.
func TestInstallingSubcommandsAreBlocking(t *testing.T) {
	for _, verb := range []string{"add", "update", "remove"} {
		if !IsPacksBlocking([]string{verb}) {
			t.Errorf("/packs %s must be blocking: it can raise pack_install", verb)
		}
	}
	if IsPacksBlocking(nil) {
		t.Error("a bare /packs listing must stay inline")
	}
}

func TestPacksUsageNamesTheNewVerbs(t *testing.T) {
	// The usage string is what a person sees after a typo, so it has to list
	// everything that exists.
	usage := packsUsage()
	for _, want := range []string{"add", "update", "remove", "on", "off", "default", "reload"} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage does not mention %q: %s", want, usage)
		}
	}
}

func TestSlashTableMentionsTheNewVerbs(t *testing.T) {
	var row string
	for _, c := range SlashCommandTable {
		if c.Name == "/packs" {
			row = c.Desc // SlashCommandInfo{Name, Desc, Args} — internal/ui/common.go:56
		}
	}
	if row == "" {
		t.Fatal("no /packs row in SlashCommandTable")
	}
	for _, want := range []string{"add", "remove"} {
		if !strings.Contains(row, want) {
			t.Errorf("the /packs row does not mention %q: %s", want, row)
		}
	}
}

// The menu row "Install a skill pack" submits `/packs add` with no name. It
// must not print a usage string: the spec's intended outcome is that a person
// can install a pack "without knowing where packs live on disk or what a pack
// directory has to contain", and a usage line teaches syntax to somebody who
// came to the menu precisely to avoid it.
func TestPacksAddWithNoNameOffersWhatTheSourceHas(t *testing.T) {
	src := t.TempDir()
	for _, n := range []string{"assist", "ue5"} {
		dir := filepath.Join(src, "packs", n)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := `{"schema":1,"name":"` + n + `","version":"0.1.0","description":"a pack for tests",
  "core":{"file":"core.md","budget_tokens":1500}}`
		if err := os.WriteFile(filepath.Join(dir, "pack.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "core.md"), []byte("Rules.\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := config.Default()
	cfg.Packs.Source = src
	reg, rerr := tools.NewRegistry(t.TempDir(), nil)
	if rerr != nil {
		t.Fatal(rerr)
	}
	ag := agent.New(cfg, nullProvider{}, "m", reg, "")

	out := PacksCommand(context.Background(), ag, []string{"/packs", "add"})
	if strings.HasPrefix(out, "usage:") {
		t.Fatalf("the menu's own row printed a usage string:\n%s", out)
	}
	for _, want := range []string{"assist", "ue5"} {
		if !strings.Contains(out, want) {
			t.Errorf("the output should offer %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "/packs add") {
		t.Errorf("the output should say how to pick one:\n%s", out)
	}
}

// A hung clone must be cancellable: the install runs under the session's own
// context, not context.Background(), so Esc or the session ending reaches it.
func TestPacksAddHonoursACancelledContext(t *testing.T) {
	cfg := config.Default()
	reg, rerr := tools.NewRegistry(t.TempDir(), func(string, string) bool { return true })
	if rerr != nil {
		t.Fatal(rerr)
	}
	ag := agent.New(cfg, nullProvider{}, "m", reg, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out := PacksCommand(ctx, ag, []string{"/packs", "add", "assist", "https://example.invalid/packs.git"})
	if !strings.Contains(out, "context canceled") {
		t.Errorf("a cancelled context did not stop the install: %q", out)
	}
}

// `/packs add <name> <source>` has always worked; now it is said somewhere.
func TestPacksUsageDocumentsThePositionalSource(t *testing.T) {
	if !strings.Contains(packsUsage(), "[source]") {
		t.Errorf("usage does not mention the positional source: %s", packsUsage())
	}
	for _, c := range SlashCommandTable {
		if c.Name == "/packs" && !strings.Contains(c.Desc, "[source]") {
			t.Errorf("the /packs row does not mention the positional source: %s", c.Desc)
		}
	}
}

// The same line from /packs remove in a session.
func TestPacksRemoveHintsAtGitInARepository(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, ".becode", "packs", "assist")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack.json"), []byte(`{"schema":1,"name":"assist","version":"0.1.0","core":{"file":"core.md","budget_tokens":1500}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"add", "-A"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "pack"}} {
		c := exec.Command("git", args...)
		c.Dir = ws
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	t.Setenv("HOME", t.TempDir())
	reg, rerr := tools.NewRegistry(ws, nil)
	if rerr != nil {
		t.Fatal(rerr)
	}
	ag := agent.New(config.Default(), nullProvider{}, "m", reg, "")
	out := PacksCommand(context.Background(), ag, []string{"/packs", "remove", "assist"})
	if !strings.Contains(out, "git checkout -- .becode/packs/assist") {
		t.Errorf("no hint at how a committed pack comes back: %q", out)
	}
}
