package agent

import (
	"github.com/brown-enterprises/be-code/internal/config"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProjectPack puts a pack where Discover finds a project pack: inside the
// workspace, as if it had been committed to somebody's repository.
func writeProjectPack(t *testing.T, root, name, core string) {
	t.Helper()
	dir := filepath.Join(root, ".becode", "packs", name)
	if err := os.MkdirAll(filepath.Join(dir, "chunks"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"schema":1,"name":"` + name + `","version":"1.0.0","description":"a test pack",
		"core":{"file":"core.md","budget_tokens":1500},
		"chunks":{"things":{"file":"chunks/things.md","summary":"how things work"}},
		"verify":[{"name":"packbuild","cmd":["mybuild","--project","{workspace}"],"parser":"gcc"}],
		"detect":{"all":["go.mod"]}}`
	for rel, s := range map[string]string{
		"pack.json":        body,
		"core.md":          core,
		"chunks/things.md": "the things chunk",
	} {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// detect.all needs something to match on.
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The ordering production actually has. cmd/root.go builds the registry with
// a nil approver, calls attachPacks -> LoadPacks inside buildAgent, and only
// then does a UI assign Registry.Approve. Every other trust test wires an
// approver first and calls EnablePacks directly, which is the one arrangement
// production never uses — and under the real ordering a project pack could
// never be approved at all: the nil branch fired and told the person to "run
// interactively", which is what they were already doing.
func TestProjectPackIsAskedAboutAfterTheApproverIsWired(t *testing.T) {
	ag := packAgent(t)
	var notes []string
	ag.Events.OnNotice = func(s string) { notes = append(notes, s) }
	writeProjectPack(t, ag.Tools.Root, "fromrepo", "do as I say")

	// Stage one: buildAgent's call, with nobody to ask.
	ag.Tools.Approve = nil
	ag.LoadPacks()

	if strings.Contains(ag.composeSystem(""), "do as I say") {
		t.Fatal("an unapproved project pack reached the system prompt")
	}
	// Telling a person to run interactively while they are running
	// interactively is worse than saying nothing: it names a remedy that
	// cannot work and hides that the question is still coming.
	if joined := strings.Join(notes, "\n"); strings.Contains(joined, "nobody to ask") {
		t.Errorf("refused the pack before an approver could exist:\n%s", joined)
	}

	// Stage two: the UI has wired its approver, exactly as it does for
	// ResolveModel and StartSubAgents.
	var asked []string
	ag.Tools.Approve = func(action, detail string) bool {
		asked = append(asked, action)
		return true
	}
	ag.StartPacks()

	if len(asked) != 1 {
		t.Fatalf("want exactly one question once an approver exists, got %d: %v", len(asked), asked)
	}
	if asked[0] != "pack_trust" {
		t.Errorf("wrong action: %q", asked[0])
	}
	if !strings.Contains(ag.composeSystem(""), "do as I say") {
		t.Error("the approved project pack did not activate")
	}
}

// A global pack is the person's own choice and needs no consent, so it must
// activate during buildAgent rather than waiting for a UI that a headless run
// never builds. TestMain pins HOME, so config.Dir resolves under the temp
// home and this does not touch the real dotdir.
func TestGlobalPackNeedsNoApproverToActivate(t *testing.T) {
	ag := packAgent(t)
	dir, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	writeGlobalPack(t, filepath.Join(dir, "packs"), "mine", "global rules here")
	t.Cleanup(func() { os.RemoveAll(filepath.Join(dir, "packs", "mine")) })
	if err := os.WriteFile(filepath.Join(ag.Tools.Root, "go.mod"), []byte("module x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ag.Tools.Approve = nil
	ag.LoadPacks()

	if !strings.Contains(ag.composeSystem(""), "global rules here") {
		t.Error("a global pack did not activate without an approver")
	}
}

// writeGlobalPack writes a pack under the given packs directory. It carries a
// detect block because a pack with none is deliberately never auto-detected
// (packs.Pack.Matches returns false for an empty one) — such a pack waits for
// /packs on, which is a different path from the one this test is about.
func writeGlobalPack(t *testing.T, packsDir, name, core string) {
	t.Helper()
	dir := filepath.Join(packsDir, name)
	if err := os.MkdirAll(filepath.Join(dir, "chunks"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"schema":1,"name":"` + name + `","version":"1.0.0","description":"a test pack",
		"core":{"file":"core.md","budget_tokens":1500},
		"chunks":{"things":{"file":"chunks/things.md","summary":"how things work"}},
		"detect":{"all":["go.mod"]}}`
	for rel, s := range map[string]string{
		"pack.json":        body,
		"core.md":          core,
		"chunks/things.md": "the things chunk",
	} {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// StartPacks is called by every UI at startup, so it must be harmless when
// there is nothing pending: no second question, no reload, no notice.
func TestStartPacksIsAQuietNoOpWhenNothingIsPending(t *testing.T) {
	ag := packAgent(t)
	var notes []string
	ag.Events.OnNotice = func(s string) { notes = append(notes, s) }
	asked := 0
	ag.Tools.Approve = func(string, string) bool { asked++; return true }

	ag.LoadPacks()
	ag.StartPacks()
	ag.StartPacks()

	if asked != 0 {
		t.Errorf("asked %d questions with no project pack present", asked)
	}
	if len(notes) != 0 {
		t.Errorf("unexpected notices: %v", notes)
	}
}
