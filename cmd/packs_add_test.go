package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The CLI's own seam: a non-TTY stdin with no -y must refuse rather than hang
// or silently proceed, the same rule `be-code init` follows.
func TestPacksAddRefusesNonInteractiveWithoutYes(t *testing.T) {
	ws := t.TempDir()
	src := t.TempDir()
	writePackFixture(t, filepath.Join(src, "packs", "assist"))

	// stdinIsTTY is a var precisely so a test can stub it (cmd/root.go:113);
	// under `go test` it reports true, which would take the interactive path
	// and decline at EOF — the right outcome by luck, for the wrong reason,
	// and with the wrong message.
	saved := stdinIsTTY
	stdinIsTTY = func() bool { return false }
	defer func() { stdinIsTTY = saved }()

	out, err := runPacksAdd(t, []string{"assist", "--source", src, "-C", ws})
	if err == nil {
		t.Fatalf("a non-interactive add succeeded without -y:\n%s", out)
	}
	if !strings.Contains(out+err.Error(), "-y") {
		t.Errorf("the refusal should say how to proceed: %s / %v", out, err)
	}
	if _, serr := os.Stat(filepath.Join(ws, ".becode", "packs", "assist")); !os.IsNotExist(serr) {
		t.Error("a refused add installed something")
	}
}

func TestPacksAddInstallsWithYes(t *testing.T) {
	ws := t.TempDir()
	src := t.TempDir()
	writePackFixture(t, filepath.Join(src, "packs", "assist"))

	out, err := runPacksAdd(t, []string{"assist", "--source", src, "-C", ws, "-y"})
	if err != nil {
		t.Fatalf("add failed: %v\n%s", err, out)
	}
	if _, serr := os.Stat(filepath.Join(ws, ".becode", "packs", "assist", "core.md")); serr != nil {
		t.Errorf("the pack is not installed: %v", serr)
	}
	if !strings.Contains(out, "assist") {
		t.Errorf("the output should name the pack: %s", out)
	}
}

func TestPacksAddWithNoNameListsWhatIsOnOffer(t *testing.T) {
	ws := t.TempDir()
	src := t.TempDir()
	writePackFixture(t, filepath.Join(src, "packs", "assist"))
	writePackFixture(t, filepath.Join(src, "packs", "ue5"))

	out, err := runPacksAdd(t, []string{"--source", src, "-C", ws, "-y"})
	if err == nil {
		t.Fatal("add with no pack name succeeded; it must ask which one")
	}
	for _, want := range []string{"assist", "ue5"} {
		if !strings.Contains(out+err.Error(), want) {
			t.Errorf("the output should offer %q: %s / %v", want, out, err)
		}
	}
}

func writePackFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(dir)
	files := map[string]string{
		"pack.json": `{"schema":1,"name":"` + name + `","version":"0.1.0",
  "description":"a pack for tests","core":{"file":"core.md","budget_tokens":1500}}`,
		"core.md": "Rules.\n",
	}
	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// runPacksAdd drives the subcommand the way cmd/packs_test.go drives
// packsValidateCmd: RunE directly, with this package's flag vars set as the
// test's own setup. Calling Execute() on a subcommand delegates to the root
// command and ignores SetArgs, so the arguments never arrived.
func runPacksAdd(t *testing.T, args []string) (string, error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // never touch the real ~/.be-code
	var buf strings.Builder
	cmd := packsAddCmd
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	savedYes, savedDir, savedSrc := flagYes, flagDir, flagPackSource
	defer func() {
		flagYes, flagDir, flagPackSource = savedYes, savedDir, savedSrc
		_ = cmd.Flags().Set("force", "false")
	}()

	// Translate the argv the plan wrote into the flag vars the command reads.
	var positional []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-y":
			flagYes = true
		case "-C":
			i++
			flagDir = args[i]
		case "--source":
			i++
			flagPackSource = args[i]
		case "--force":
			if err := cmd.Flags().Set("force", "true"); err != nil {
				t.Fatal(err)
			}
		default:
			positional = append(positional, args[i])
		}
	}
	err := cmd.RunE(cmd, positional)
	return buf.String(), err
}
