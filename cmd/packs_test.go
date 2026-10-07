package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// be-code packs validate is what a pack author runs in CI, so a broken pack
// has to exit non-zero: a validator that only prints is a validator nobody
// notices failing.
func TestPacksValidateReportsProblemsAndFails(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "p")
	if err := os.MkdirAll(filepath.Join(dir, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel string, mode os.FileMode, body string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("pack.json", 0o644, `{"schema":1,"name":"p","version":"1.0.0",
		"core":{"file":"core.md","budget_tokens":10},
		"chunks":{"big":{"file":"chunks/big.md","summary":"s"}},
		"verify":[{"name":"build","cmd":["{pack}/tools/build","--x"],"parser":"gcc"}]}`)
	write("core.md", 0o644, strings.Repeat("core rules. ", 50)) // over a 10-token budget
	if err := os.MkdirAll(filepath.Join(dir, "chunks"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("chunks/big.md", 0o644, strings.Repeat("x", minToolOutputCap+1))
	write("tools/build", 0o644, "#!/bin/sh\n") // deliberately not executable

	err := packsValidateCmd.RunE(packsValidateCmd, []string{dir})
	if err == nil {
		t.Fatal("a pack with problems must exit non-zero")
	}
	if !strings.Contains(err.Error(), "3 problem") {
		t.Errorf("want all three problems counted, got %v", err)
	}

	// Fixed, it passes: the executable bit, a smaller chunk, a real budget.
	if err := os.Chmod(filepath.Join(dir, "tools", "build"), 0o755); err != nil {
		t.Fatal(err)
	}
	write("chunks/big.md", 0o644, "small enough")
	write("pack.json", 0o644, `{"schema":1,"name":"p","version":"1.0.0",
		"core":{"file":"core.md","budget_tokens":1500},
		"chunks":{"big":{"file":"chunks/big.md","summary":"s"}},
		"verify":[{"name":"build","cmd":["{pack}/tools/build","--x"],"parser":"gcc"}]}`)
	if err := packsValidateCmd.RunE(packsValidateCmd, []string{dir}); err != nil {
		t.Fatalf("a sound pack must validate: %v", err)
	}
}

// A manifest the loader refuses is an error, not a list of problems.
func TestPacksValidateRejectsABadManifest(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "p")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack.json"), []byte(`{"schema":1,"name":"p","version":"1",
		"verify":[{"name":"b","cmd":["b"],"parser":"nope"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	err := packsValidateCmd.RunE(packsValidateCmd, []string{dir})
	if err == nil || !strings.Contains(err.Error(), "parser") {
		t.Fatalf("want the unknown parser named, got %v", err)
	}
}
