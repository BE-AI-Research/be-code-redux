package packs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const manifestJSON = `{
  "schema": 1,
  "name": "ue5",
  "version": "0.1.0",
  "description": "Unreal Engine 5 C++ and build expertise",
  "detect": { "any": ["*.uproject"] },
  "core": { "file": "core.md", "budget_tokens": 1500 },
  "chunks": {
    "cpp-reflection": { "file": "chunks/cpp-reflection.md", "summary": "UCLASS and friends", "triggers": ["UCLASS"] },
    "build-system":   { "file": "chunks/build-system.md",   "summary": "Build.cs and modules" }
  },
  "verify": [
    { "name": "build", "cmd": ["{pack}/tools/ue-build", "--project", "{workspace}"], "parser": "ubt", "timeout": "30m" }
  ],
  "requires": { "be_code": ">=1.3.0" }
}`

func writePack(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "ue5")
	for path, body := range map[string]string{
		"pack.json":                manifestJSON,
		"core.md":                  "Always-on rules.",
		"chunks/cpp-reflection.md": "# reflection\nUCLASS rules.",
		"chunks/build-system.md":   "# build\nBuild.cs rules.",
		"templates/Actor.h.tmpl":   "// {{MODULE_API}} actor header",
		"tools/ue-build":           "#!/bin/sh\n",
	} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestLoadReadsTheManifest(t *testing.T) {
	p, warns, err := Load(writePack(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	if p.Name != "ue5" || p.Version != "0.1.0" {
		t.Fatalf("got %+v", p)
	}
	if p.Core != "Always-on rules." {
		t.Errorf("core = %q", p.Core)
	}
	if len(p.ChunkNames()) != 2 || p.ChunkNames()[0] != "build-system" {
		t.Errorf("chunk names = %v, want them sorted", p.ChunkNames())
	}
}

// The index is what the model is told it can fetch, so every chunk has to
// appear in it with its summary.
func TestIndexListsEveryChunk(t *testing.T) {
	p, _, _ := Load(writePack(t))
	idx := p.Index()
	for _, want := range []string{"ue5/cpp-reflection", "UCLASS and friends", "ue5/build-system", "Build.cs and modules"} {
		if !strings.Contains(idx, want) {
			t.Errorf("index is missing %q:\n%s", want, idx)
		}
	}
}

func TestChunkAndTemplateAreServedFromThePack(t *testing.T) {
	p, _, _ := Load(writePack(t))
	body, err := p.Chunk("cpp-reflection")
	if err != nil || !strings.Contains(body, "UCLASS rules") {
		t.Fatalf("chunk: %q err=%v", body, err)
	}
	if _, err := p.Chunk("nope"); err == nil {
		t.Error("an unknown chunk must be an error, not an empty string")
	}
	tmpl, err := p.Template("templates/Actor.h.tmpl")
	if err != nil || !strings.Contains(tmpl, "MODULE_API") {
		t.Fatalf("template: %q err=%v", tmpl, err)
	}
}

// The template path comes from the model. A pack lives outside the workspace,
// so the file tools' confinement does not cover this read: it has to confine
// to the pack's own directory itself, or "../../.ssh/id_rsa" reads whatever
// the process can.
func TestTemplateCannotEscapeThePack(t *testing.T) {
	root := writePack(t)
	secret := filepath.Join(filepath.Dir(root), "secret.txt")
	if err := os.WriteFile(secret, []byte("private"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, _, _ := Load(root)
	for _, bad := range []string{
		"../secret.txt",
		"templates/../../secret.txt",
		secret,
		"/etc/passwd",
		`..\secret.txt`,
	} {
		if body, err := p.Template(bad); err == nil {
			t.Errorf("Template(%q) escaped the pack and returned %q", bad, body)
		}
	}
	// A pack's own file is still served, including through a tidy path.
	if _, err := p.Template("./templates/Actor.h.tmpl"); err != nil {
		t.Errorf("a path inside the pack was refused: %v", err)
	}
}

// A pack's verify entry is an argument vector with the pack's own directory
// and the workspace substituted in.
func TestChecksSubstitutePlaceholders(t *testing.T) {
	root := writePack(t)
	p, _, _ := Load(root)
	checks := p.Checks("/home/dev/Shooter")
	if len(checks) != 1 {
		t.Fatalf("got %d checks", len(checks))
	}
	c := checks[0]
	if c.Parser != "ubt" || c.Name != "build" {
		t.Errorf("got %+v", c)
	}
	if len(c.Argv) != 3 || c.Argv[0] != filepath.Join(root, "tools/ue-build") || c.Argv[2] != "/home/dev/Shooter" {
		t.Errorf("argv = %v", c.Argv)
	}
	if c.Command != "" {
		t.Errorf("a pack check must never be a shell string: %q", c.Command)
	}
	if c.Timeout.Minutes() != 30 {
		t.Errorf("timeout = %v", c.Timeout)
	}
}

func TestLoadRejectsABadManifest(t *testing.T) {
	for _, tc := range []struct{ name, body, wantIn string }{
		{"unknown schema", `{"schema": 99, "name": "x", "version": "1"}`, "schema"},
		{"bad name", `{"schema": 1, "name": "UE 5!", "version": "1"}`, "name"},
		{"no name", `{"schema": 1, "version": "1"}`, "name"},
		{"unknown parser", `{"schema":1,"name":"x","version":"1","verify":[{"name":"b","cmd":["b"],"parser":"nope"}]}`, "parser"},
		{"shell string instead of argv", `{"schema":1,"name":"x","version":"1","verify":[{"name":"b","cmd":[]}]}`, "cmd"},
		{"not json", `nonsense`, "pack.json"},
	} {
		dir := filepath.Join(t.TempDir(), "p")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "pack.json"), []byte(tc.body), 0o644); err != nil {
			t.Fatal(err)
		}
		_, _, err := Load(dir)
		if err == nil {
			t.Errorf("%s: accepted", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantIn) {
			t.Errorf("%s: message %q does not mention %q", tc.name, err, tc.wantIn)
		}
	}
}

// A missing chunk file is the pack author's mistake, but it must not stop the
// pack loading: the rest of it still works.
func TestMissingChunkFileWarnsAndIsDropped(t *testing.T) {
	root := writePack(t)
	if err := os.Remove(filepath.Join(root, "chunks/build-system.md")); err != nil {
		t.Fatal(err)
	}
	p, warns, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "build-system") {
		t.Fatalf("warnings = %v", warns)
	}
	if len(p.ChunkNames()) != 1 || p.ChunkNames()[0] != "cpp-reflection" {
		t.Errorf("chunks = %v", p.ChunkNames())
	}
}
