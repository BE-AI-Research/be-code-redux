package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/packs"
)

// loadPackWithWhen builds a pack whose one check declares when.changed.
func loadPackWithWhen(t *testing.T, name string, changed []string) *packs.Pack {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(dir, "chunks"), 0o755); err != nil {
		t.Fatal(err)
	}
	when := ""
	if changed != nil {
		when = `,"when":{"changed":["` + strings.Join(changed, `","`) + `"]}`
	}
	body := `{"schema":1,"name":"` + name + `","version":"1.0.0","description":"a test pack",
		"core":{"file":"core.md","budget_tokens":1500},
		"chunks":{"things":{"file":"chunks/things.md","summary":"how things work"}},
		"verify":[{"name":"packbuild","cmd":["mybuild"],"parser":"gcc"` + when + `}]}`
	for rel, s := range map[string]string{
		"pack.json":        body,
		"core.md":          "rules",
		"chunks/things.md": "chunk",
	} {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p, _, err := packs.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func checkNames(ag *Agent, changed []string) []string {
	var out []string
	for _, c := range ag.verifyProjectFor(changed).Checks {
		out = append(out, c.Name)
	}
	return out
}

func has(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// when.changed was parsed, documented in the format spec and declared by both
// shipped packs — and never consulted, so every pack check ran on every turn
// that touched any file. For the ue5 pack that is a full UnrealBuildTool build
// (30-minute timeout) because somebody fixed a typo in a README.
func TestPackCheckIsSkippedWhenNothingItWatchesChanged(t *testing.T) {
	ag := packAgent(t)
	ag.EnablePacks([]*packs.Pack{loadPackWithWhen(t, "p", []string{"src/**", "*.go"})})

	if names := checkNames(ag, []string{"README.md", "docs/x.md"}); has(names, "packbuild") {
		t.Errorf("ran a watched check for unrelated edits: %v", names)
	}
	if names := checkNames(ag, []string{"src/app/main.ts"}); !has(names, "packbuild") {
		t.Errorf("skipped the check when a watched file changed: %v", names)
	}
	if names := checkNames(ag, []string{"main.go"}); !has(names, "packbuild") {
		t.Errorf("a top-level glob did not match: %v", names)
	}
}

// A check with no when.changed is unconditional, as the spec says.
func TestPackCheckWithoutWhenAlwaysRuns(t *testing.T) {
	ag := packAgent(t)
	ag.EnablePacks([]*packs.Pack{loadPackWithWhen(t, "p", nil)})

	if names := checkNames(ag, []string{"README.md"}); !has(names, "packbuild") {
		t.Errorf("an unconditional check was skipped: %v", names)
	}
}

// Fail open. When the changed-file list is unavailable — no checkpointer, or
// a checkpoint that could not be read — running the check is the safe answer:
// skipping verification because we could not tell what changed would quietly
// stop verifying at all, which is the one outcome worse than a slow build.
func TestPackCheckRunsWhenTheChangedListIsUnknown(t *testing.T) {
	ag := packAgent(t)
	ag.EnablePacks([]*packs.Pack{loadPackWithWhen(t, "p", []string{"src/**"})})

	if names := checkNames(ag, nil); !has(names, "packbuild") {
		t.Errorf("skipped a check with no changed-file list to judge by: %v", names)
	}
}

// The project's own detected checks are never filtered: when.changed is a
// pack's field, and a pack must not be able to suppress the harness's checks.
func TestProjectChecksAreNeverFilteredByPackGlobs(t *testing.T) {
	ag := packAgent(t)
	if err := os.WriteFile(filepath.Join(ag.Tools.Root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag.EnablePacks([]*packs.Pack{loadPackWithWhen(t, "p", []string{"nothing/**"})})

	names := checkNames(ag, []string{"README.md"})
	if len(names) == 0 {
		t.Fatal("no checks at all; the fixture detected nothing to compare")
	}
	if has(names, "packbuild") {
		t.Errorf("the pack check should have been skipped: %v", names)
	}
}

// /verify, the REPL's /verify and be-code verify all called verify.Detect
// directly, so an explicit verification reported "no verification checks
// apply (unrecognized project type)" on an Unreal workspace while the
// automatic repair loop was running a full UBT build on the same files. They
// also passed no gate, so neither shell_deny nor shell_after_web applied.
// VerifyNow is the one path, and it is the agent's own.
func TestVerifyNowIncludesPackChecks(t *testing.T) {
	ag := packAgent(t)
	ag.EnablePacks([]*packs.Pack{loadPackWithWhen(t, "p", nil)})

	proj := ag.VerifyProjectNow()
	var found bool
	for _, c := range proj.Checks {
		if c.Name == "packbuild" {
			found = true
		}
	}
	if !found {
		t.Fatalf("an explicit verification left the pack's checks out: %+v", proj.Checks)
	}
	if proj.Kind == "none" || proj.Kind == "" {
		t.Errorf("a workspace with pack checks is not an unrecognised project: kind=%q", proj.Kind)
	}
}
