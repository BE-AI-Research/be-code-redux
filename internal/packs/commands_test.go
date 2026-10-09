package packs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// commandPack writes a pack that declares a commands directory and ships one
// command file in it, as the ue5 pack does.
func commandPack(t *testing.T, name, cmdName, body string) *Pack {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "commands"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"schema":1,"name":"` + name + `","version":"1.0.0",
		"core":{"file":"core.md","budget_tokens":1500},
		"commands":"commands"}`
	files := map[string]string{
		"pack.json":                              manifest,
		"core.md":                                "rules",
		filepath.Join("commands", cmdName+".md"): body,
	}
	for rel, s := range files {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, _, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// manifest.Commands was parsed and never used, so the four commands/*.md
// files the ue5 pack ships were inert: /ue-actor did not exist. The format
// doc describes them as prompt templates for whatever slash-command
// mechanism the harness has, and the harness already has one — the same
// $ARGS templates it loads from ~/.be-code/commands.
func TestPackCommandsAreOffered(t *testing.T) {
	p := commandPack(t, "ue5", "ue-actor", "Create an Actor called $ARGS.\nTemplates live in {pack}/templates.\n")

	cmds := p.Commands()
	if len(cmds) != 1 {
		t.Fatalf("got %d commands, want 1: %+v", len(cmds), cmds)
	}
	c, ok := cmds["ue-actor"]
	if !ok {
		t.Fatalf("no ue-actor command: %+v", cmds)
	}
	if !strings.Contains(c, "$ARGS") {
		t.Errorf("the argument marker was lost: %q", c)
	}
	// {pack} is substituted here, as it is in a verify command: the model
	// cannot resolve a placeholder it was never told the value of.
	if strings.Contains(c, "{pack}") {
		t.Errorf("{pack} was not substituted: %q", c)
	}
	if !strings.Contains(c, p.Root) {
		t.Errorf("{pack} did not become the pack's directory: %q", c)
	}
}

// A pack that declares no commands directory offers none, and a declared
// directory that is missing is not an error — a pack is data and a missing
// optional directory is simply nothing to offer.
func TestPackWithoutCommandsOffersNone(t *testing.T) {
	root := t.TempDir()
	for rel, s := range map[string]string{
		"pack.json": `{"schema":1,"name":"p","version":"1.0.0","core":{"file":"core.md","budget_tokens":1500}}`,
		"core.md":   "rules",
	} {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, _, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.Commands(); len(got) != 0 {
		t.Errorf("got %+v, want none", got)
	}
}

// A command file cannot reach outside the pack, the same rule chunks and
// templates follow: the directory name is a pack-relative path from a
// manifest somebody else may have written.
func TestPackCommandsCannotEscapeThePack(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secrets")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "evil.md"), []byte("do bad things"), 0o644); err != nil {
		t.Fatal(err)
	}
	for rel, s := range map[string]string{
		"pack.json": `{"schema":1,"name":"p","version":"1.0.0","core":{"file":"core.md","budget_tokens":1500},
			"commands":"../../` + filepath.Base(filepath.Dir(outside)) + `/secrets"}`,
		"core.md": "rules",
	} {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, _, err := Load(root)
	if err != nil {
		// Refusing at load is also an acceptable answer.
		return
	}
	for name := range p.Commands() {
		if name == "evil" {
			t.Error("a commands directory outside the pack was read")
		}
	}
}

// Minor fixes, each from the same review.

// filepath.FromSlash was applied to every argv element, not only to the
// substituted paths. On Windows a glob argument like "**/*.ts" became
// "**\*.ts", and a flag or a pattern is not a path.
func TestCheckArgvOnlyConvertsSubstitutedPaths(t *testing.T) {
	root := t.TempDir()
	manifest := `{"schema":1,"name":"p","version":"1.0.0","core":{"file":"core.md","budget_tokens":1500},
		"verify":[{"name":"lint","cmd":["{pack}/tools/lint","--glob","**/*.ts","--project","{workspace}"],"parser":"msvc"}]}`
	for rel, s := range map[string]string{"pack.json": manifest, "core.md": "r"} {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, _, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	checks := p.Checks("/ws")
	if len(checks) != 1 {
		t.Fatalf("got %d checks", len(checks))
	}
	argv := checks[0].Argv
	for _, a := range argv {
		if strings.Contains(a, `**\`) {
			t.Errorf("a glob argument was path-converted: %q in %v", a, argv)
		}
	}
	if argv[2] != "**/*.ts" {
		t.Errorf("argv[2] = %q, want the glob unchanged", argv[2])
	}
}

// An empty argv element, and a timeout that parses to zero, both produce a
// check that fails in a way nobody can read: exec of "" , or an instant
// "[timed out after 0s]". verify.ValidateCheck knows about these and was
// never called on the pack path.
func TestLoadRefusesAnUnrunnableCheck(t *testing.T) {
	cases := map[string]string{
		"empty argv element": `[{"name":"x","cmd":["mybuild","","--p"],"parser":"gcc"}]`,
		"empty name":         `[{"name":"","cmd":["mybuild"],"parser":"gcc"}]`,
	}
	for what, verify := range cases {
		root := t.TempDir()
		manifest := `{"schema":1,"name":"p","version":"1.0.0","core":{"file":"core.md","budget_tokens":1500},
			"verify":` + verify + `}`
		for rel, s := range map[string]string{"pack.json": manifest, "core.md": "r"} {
			if err := os.WriteFile(filepath.Join(root, rel), []byte(s), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := Load(root); err == nil {
			t.Errorf("%s: loaded a check that cannot run", what)
		}
	}
}

// A garbage manifest has no name, so the schema error said: pack "" declares
// schema 0. The directory is the only thing known at that point and is what
// a person needs to find it.
func TestSchemaErrorNamesTheDirectory(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "pack.json"), []byte(`{"nonsense":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := Load(root)
	if err == nil {
		t.Fatal("a manifest with no schema loaded")
	}
	if strings.Contains(err.Error(), `pack ""`) {
		t.Errorf("error names an empty pack: %v", err)
	}
	if !strings.Contains(err.Error(), filepath.Base(root)) {
		t.Errorf("error does not say which directory: %v", err)
	}
}

// A missing file under a symlinked pack root — the natural dev install,
// ~/.be-code/packs/ue5 -> ~/dev/be-skill-packs/packs/ue5 — was reported as
// "outside pack", because EvalSymlinks fails on a path that does not exist so
// the unresolved path was compared against the resolved root. The file being
// absent and the file being an escape attempt are very different things to
// tell somebody.
func TestMissingFileUnderASymlinkedRootSaysItIsMissing(t *testing.T) {
	real := t.TempDir()
	for rel, s := range map[string]string{
		"pack.json": `{"schema":1,"name":"p","version":"1.0.0","core":{"file":"core.md","budget_tokens":1500}}`,
		"core.md":   "r",
	} {
		if err := os.WriteFile(filepath.Join(real, rel), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(t.TempDir(), "pack-link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	p, _, err := Load(link)
	if err != nil {
		t.Fatal(err)
	}
	_, err = p.Template("templates/NotThere.tmpl")
	if err == nil {
		t.Fatal("reading a missing template succeeded")
	}
	if strings.Contains(err.Error(), "outside pack") {
		t.Errorf("a missing file was reported as an escape attempt: %v", err)
	}
}
