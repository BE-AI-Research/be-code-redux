package packs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeManifestPack(t *testing.T, body string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pack")
	if err := os.MkdirAll(filepath.Join(dir, "chunks"), 0o755); err != nil {
		t.Fatal(err)
	}
	for rel, s := range map[string]string{"pack.json": body, "core.md": "rules", "chunks/a.md": "a", "chunks/b.md": "b"} {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The v1.0 schema types requires.parsers/hooks/packs/tools and mcp_servers as
// arrays. The format's own conformance pack uses them, so a loader that
// claims the closed contract must load them rather than call the JSON
// invalid.
func TestLoadAcceptsTheSchemaShapedRequiresAndServers(t *testing.T) {
	dir := writeManifestPack(t, `{"schema":1,"name":"pack","version":"1.0.0","description":"d",
		"core":{"file":"core.md","budget_tokens":100},
		"requires":{"format":"1.0","be_code":">=1.3.0","parsers":["gcc"],"hooks":["pre_exec"],"tools":["shellcheck"]},
		"mcp_servers":[{"name":"zeta","command":["zeta","--serve"]},{"name":"alpha","command":["alpha"],"optional":true}]}`)
	p, _, err := Load(dir)
	if err != nil {
		t.Fatalf("a schema-valid pack must load: %v", err)
	}
	if got := strings.Join(p.DeclaresUnstartedServers(), ","); got != "alpha,zeta" {
		t.Errorf("server names from the array form = %q", got)
	}
}

// The name-keyed object the shipped packs use keeps working.
func TestLoadStillAcceptsObjectShapedServers(t *testing.T) {
	dir := writeManifestPack(t, `{"schema":1,"name":"pack","version":"1.0.0","description":"d",
		"core":{"file":"core.md","budget_tokens":100},
		"mcp_servers":{"zeta":{"command":["zeta"]},"alpha":{"command":["alpha"]}}}`)
	p, _, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(p.DeclaresUnstartedServers(), ","); got != "alpha,zeta" {
		t.Errorf("server names from the object form = %q", got)
	}
}

// A type error is reported by the validator, which names the field, not by
// the typed parse, which calls the whole file invalid JSON.
func TestTypeErrorsNameTheField(t *testing.T) {
	dir := writeManifestPack(t, `{"schema":1,"name":"pack","version":"1.0.0","description":"d",
		"core":{"file":"core.md","budget_tokens":"lots"}}`)
	_, _, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), "budget_tokens") || strings.Contains(err.Error(), "not valid JSON") {
		t.Fatalf("want an error naming budget_tokens, got %v", err)
	}
}

// An error inside the fourth check says verify[3], like its sibling messages.
func TestIndexedErrorsKeepTheIndex(t *testing.T) {
	err := ValidateManifest([]byte(`{"schema":1,"name":"pack","version":"1.0.0","description":"d",
		"core":{"file":"core.md","budget_tokens":100},
		"verify":[{"name":"a","cmd":["a"],"parser":"gcc"},{"name":"b","cmd":["b"],"parser":"gcc"},
		          {"name":"c","cmd":["c"],"parser":"gcc"},{"name":"d","cmd":["d"],"parser":"gcc","when":{"changed":["src/**",42]}}]}`))
	if err == nil || !strings.Contains(err.Error(), "verify[3].when.changed[1]") {
		t.Fatalf("want verify[3].when.changed[1] named, got %v", err)
	}
	err = ValidateManifest([]byte(`{"schema":1,"name":"pack","version":"1.0.0","description":"d",
		"core":{"file":"core.md","budget_tokens":100},
		"verify":[{"name":"a","cmd":["a"],"parser":"gcc"},{"name":"b","cmd":[],"parser":"gcc"}]}`))
	if err == nil || !strings.Contains(err.Error(), "verify[1].cmd") {
		t.Fatalf("want verify[1].cmd named, got %v", err)
	}
}

// The schema's 200 is characters; a non-ASCII description is not shorter
// than it looks.
func TestTextLimitsCountCharactersNotBytes(t *testing.T) {
	cjk := strings.Repeat("語", 150) // 450 bytes, 150 characters
	err := ValidateManifest([]byte(`{"schema":1,"name":"pack","version":"1.0.0","description":"` + cjk + `",
		"core":{"file":"core.md","budget_tokens":100},
		"chunks":{"a":{"file":"chunks/a.md","summary":"` + cjk + `","triggers":["t"]}}}`))
	if err != nil {
		t.Fatalf("150 characters must pass: %v", err)
	}
	err = ValidateManifest([]byte(`{"schema":1,"name":"pack","version":"1.0.0","description":"` + strings.Repeat("x", 201) + `",
		"core":{"file":"core.md","budget_tokens":100}}`))
	if err == nil || !strings.Contains(err.Error(), "201 characters") {
		t.Fatalf("201 characters must fail and say so: %v", err)
	}
}

// Which chunk is reported first does not depend on map order.
func TestChunkErrorsAreReportedInNameOrder(t *testing.T) {
	body := `{"schema":1,"name":"pack","version":"1.0.0","description":"d",
		"core":{"file":"core.md","budget_tokens":100},
		"chunks":{"zeta":{"file":"chunks/a.md"},"alpha":{"file":"chunks/b.md"}}}`
	for i := 0; i < 20; i++ {
		err := ValidateManifest([]byte(body))
		if err == nil || !strings.Contains(err.Error(), `chunk "alpha"`) {
			t.Fatalf("run %d: want chunk \"alpha\" reported first, got %v", i, err)
		}
	}
}
