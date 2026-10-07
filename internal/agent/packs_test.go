package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/packs"
	"github.com/brown-enterprises/be-code/internal/provider"
)

func loadTestPack(t *testing.T, name, core, detect string, source packs.Source) *packs.Pack {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(filepath.Join(dir, "chunks"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"schema":1,"name":"` + name + `","version":"1.0.0","description":"a test pack",
		"core":{"file":"core.md","budget_tokens":1500},
		"chunks":{"things":{"file":"chunks/things.md","summary":"how things work"}},
		"verify":[{"name":"packbuild","cmd":["mybuild","--project","{workspace}"],"parser":"gcc"}]` + detect + `}`
	write := func(rel, s string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pack.json", body)
	write("core.md", core)
	write("chunks/things.md", "the things chunk")
	p, warns, err := packs.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Fatal(warns)
	}
	p.Source = source
	return p
}

func packAgent(t *testing.T) *Agent {
	ag, _ := newTestAgent(t, &scriptedProvider{responses: []provider.ChatResponse{{Content: "ok"}}},
		func(c *config.Config) { c.RepoMap = false })
	return ag
}

// Pack rules are operating rules, not project facts, and they sit between the
// project's notes and the repository map — stable for the request, so the
// prompt cache never pays for them.
func TestEnablePacksPutsCoreInTheSystemPrompt(t *testing.T) {
	ag := packAgent(t)
	ag.SetProjectNotes("These are the project notes.")
	ag.EnablePacks([]*packs.Pack{loadTestPack(t, "ue5", "Always use TObjectPtr.", "", packs.SourceGlobal)})

	sys := ag.composeSystem("")
	for _, want := range []string{"Always use TObjectPtr.", "ue5/things", "how things work"} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt is missing %q", want)
		}
	}
	notes := strings.Index(sys, "These are the project notes.")
	core := strings.Index(sys, "Always use TObjectPtr.")
	if notes < 0 || core < 0 || core < notes {
		t.Errorf("pack core must come after the project notes (notes=%d core=%d)", notes, core)
	}
	// Framed as rules to follow, unlike the notes, which are facts.
	if !strings.Contains(sys, "Pack: ue5 1.0.0") {
		t.Errorf("pack block is not labelled:\n%s", sys)
	}
}

// The pack tool only exists while a pack is active.
func TestEnablePacksRegistersTheTool(t *testing.T) {
	ag := packAgent(t)
	if _, ok := ag.Tools.Tool("pack"); ok {
		t.Fatal("pack tool registered before any pack was enabled")
	}
	ag.EnablePacks([]*packs.Pack{loadTestPack(t, "ue5", "rules", "", packs.SourceGlobal)})
	if _, ok := ag.Tools.Tool("pack"); !ok {
		t.Fatal("pack tool not registered")
	}
	// And the model is told it exists.
	if !strings.Contains(ag.composeSystem(""), "pack") {
		t.Error("pack tool missing from the prompt's tool list")
	}
}

// A pack's checks run after the project's own (packs spec §8.1).
func TestPackChecksAreAppendedToVerification(t *testing.T) {
	ag := packAgent(t)
	before := len(ag.verifyProject().Checks)
	ag.EnablePacks([]*packs.Pack{loadTestPack(t, "ue5", "rules", "", packs.SourceGlobal)})
	checks := ag.verifyProject().Checks
	if len(checks) != before+1 {
		t.Fatalf("want one more check than %d, got %d", before, len(checks))
	}
	c := checks[len(checks)-1]
	if c.Name != "packbuild" || c.Parser != "gcc" {
		t.Fatalf("got %+v", c)
	}
	if len(c.Argv) == 0 || c.Argv[2] != ag.Tools.Root {
		t.Errorf("{workspace} not substituted: %v", c.Argv)
	}
	if c.Command != "" {
		t.Errorf("a pack check must not be a shell string: %q", c.Command)
	}
}

// Core is capped so a pack cannot quietly eat a small model's window. Over
// budget the pack does not load at all, and is said so once.
func TestOverBudgetCoreIsRefused(t *testing.T) {
	ag := packAgent(t)
	var notes []string
	ag.Events.OnNotice = func(s string) { notes = append(notes, s) }
	// budget_tokens is 1500 in the fixture; at three characters per token
	// this is far past it.
	huge := strings.Repeat("rule. ", 2000)
	ag.EnablePacks([]*packs.Pack{loadTestPack(t, "ue5", huge, "", packs.SourceGlobal)})

	if strings.Contains(ag.composeSystem(""), "rule. rule.") {
		t.Error("over-budget core reached the system prompt")
	}
	if _, ok := ag.Tools.Tool("pack"); ok {
		t.Error("an over-budget pack must not be active at all")
	}
	if !strings.Contains(strings.Join(notes, "\n"), "budget") {
		t.Fatalf("no notice explaining the refusal: %v", notes)
	}
}

// A pack that arrived with somebody's repository is not the person's own
// choice. Until the trust gate exists it does not run.
func TestProjectPackIsHeldBackPendingTrust(t *testing.T) {
	ag := packAgent(t)
	var notes []string
	ag.Events.OnNotice = func(s string) { notes = append(notes, s) }
	ag.EnablePacks([]*packs.Pack{loadTestPack(t, "fromrepo", "do as I say", "", packs.SourceProject)})

	if strings.Contains(ag.composeSystem(""), "do as I say") {
		t.Error("an untrusted project pack reached the system prompt")
	}
	if _, ok := ag.Tools.Tool("pack"); ok {
		t.Error("an untrusted project pack must not be active")
	}
	joined := strings.Join(notes, "\n")
	if !strings.Contains(joined, "fromrepo") {
		t.Fatalf("the held-back pack is not named: %v", notes)
	}
}
