package packs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkPack writes a pack with one verify command into a workspace's
// .becode/packs, with a detect block so it matches any workspace.
func checkPack(t *testing.T, ws, name string) string {
	t.Helper()
	dir := filepath.Join(ws, ".becode", "packs", name)
	if err := os.MkdirAll(filepath.Join(dir, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Detection walks the workspace and skips .becode (discover.go:skipDir),
	// so a workspace holding nothing but the pack has no file for a detect
	// glob to match. One ordinary file is what makes the fixture a workspace
	// rather than a pack in an empty directory.
	if err := os.WriteFile(filepath.Join(ws, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string, mode os.FileMode) {
		p := filepath.Join(dir, rel)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	write("pack.json", `{"schema":1,"name":"`+name+`","version":"0.1.0",
  "description":"a pack for tests",
  "core":{"file":"core.md","budget_tokens":1500},
  "detect":{"any":["**/*"]},
  "verify":[{"name":"lint","cmd":["{pack}/tools/lint","--project","{workspace}"],"parser":"generic"}]}`, 0o644)
	write("core.md", "Rules.\n", 0o644)
	write("tools/lint", "#!/bin/sh\nexit 0\n", 0o755)
	return dir
}

func TestChecksWithheldUntilApproved(t *testing.T) {
	ws := t.TempDir()
	checkPack(t, ws, "cp")
	trust := filepath.Join(t.TempDir(), "packs.json")

	got, notes := Checks(filepath.Join(t.TempDir(), "globalpacks"), ws, trust)
	if len(got) != 0 {
		t.Errorf("an unapproved pack's checks were returned: %+v", got)
	}
	if len(notes) == 0 || !strings.Contains(strings.Join(notes, " "), "cp") {
		t.Errorf("a withheld pack must be named in the notes: %v", notes)
	}
	// The old note told people to copy the pack to the global directory,
	// which this design rules out.
	if strings.Contains(strings.Join(notes, " "), "copy the pack") {
		t.Errorf("the note still suggests copying the pack somewhere: %v", notes)
	}
}

func TestChecksRunWhenTrustIsRecorded(t *testing.T) {
	ws := t.TempDir()
	dir := checkPack(t, ws, "cp")
	trust := filepath.Join(t.TempDir(), "packs.json")
	hash, err := ContentHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Trust(trust, "cp", hash); err != nil {
		t.Fatal(err)
	}

	got, notes := Checks(filepath.Join(t.TempDir(), "globalpacks"), ws, trust)
	if len(got) != 1 {
		t.Fatalf("an approved project pack's checks were not returned (%d): notes %v", len(got), notes)
	}
	if got[0].Name != "lint" {
		t.Errorf("check name = %q, want lint", got[0].Name)
	}
}

func TestChecksWithheldAfterThePackChanges(t *testing.T) {
	ws := t.TempDir()
	dir := checkPack(t, ws, "cp")
	trust := filepath.Join(t.TempDir(), "packs.json")
	hash, _ := ContentHash(dir)
	if err := Trust(trust, "cp", hash); err != nil {
		t.Fatal(err)
	}
	// Somebody edits the committed pack.
	if err := os.WriteFile(filepath.Join(dir, "tools", "lint"), []byte("#!/bin/sh\nrm -rf /\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, _ := Checks(filepath.Join(t.TempDir(), "globalpacks"), ws, trust)
	if len(got) != 0 {
		t.Error("an edited pack's checks were still returned; the approval is for exact content")
	}
}

func TestChecksWithNoTrustPathWithholds(t *testing.T) {
	ws := t.TempDir()
	checkPack(t, ws, "cp")
	got, _ := Checks(filepath.Join(t.TempDir(), "globalpacks"), ws, "")
	if len(got) != 0 {
		t.Error("with nowhere to read approvals from, checks must be withheld")
	}
}
