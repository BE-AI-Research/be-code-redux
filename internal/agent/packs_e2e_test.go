package agent

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/packs"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// The whole chain, with the pack as shipped: discover it on the search path,
// detect that this workspace is an Unreal project, activate it, and find its
// rules in the prompt and its build in the checks.
func TestDiscoverToActivationWithTheRealPack(t *testing.T) {
	home := t.TempDir()
	src := "../packs/testdata/ue5"
	dst := filepath.Join(home, "packs", "ue5")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := osexec.Command("cp", "-r", src, dst).CombinedOutput(); err != nil {
		t.Fatalf("copy: %v %s", err, out)
	}

	ag, ws := newTestAgent(t, &scriptedProvider{responses: []provider.ChatResponse{{Content: "ok"}}},
		func(c *config.Config) { c.RepoMap = false })
	// An Unreal project is a .uproject at the root.
	if err := os.WriteFile(filepath.Join(ws, "Shooter.uproject"), []byte(`{"EngineAssociation":"5.4"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	found, warns := packs.Discover(filepath.Join(home, "packs"), ws)
	if len(warns) != 0 {
		t.Fatalf("warnings: %v", warns)
	}
	if len(found) != 1 || found[0].Name != "ue5" {
		t.Fatalf("discovery found %d packs", len(found))
	}
	if !found[0].Matches(ws) {
		t.Fatal("the ue5 pack did not detect a .uproject workspace")
	}
	ag.EnablePacks(found)

	sys := ag.composeSystem("")
	for _, want := range []string{
		"Pack: ue5 0.1.0",
		"ue5/cpp-reflection",
		"ue5/build-system",
		"TObjectPtr",  // from core.md
		"follow them", // the pack frame: rules, not facts
	} {
		if !strings.Contains(sys, want) {
			t.Errorf("system prompt missing %q", want)
		}
	}
	proj := ag.verifyProject()
	var build *string
	for i := range proj.Checks {
		if proj.Checks[i].Name == "build" {
			d := proj.Checks[i].Display()
			build = &d
			if proj.Checks[i].Parser != "ubt" {
				t.Errorf("parser = %q", proj.Checks[i].Parser)
			}
		}
	}
	if build == nil {
		t.Fatalf("the pack's build check is not in verification: %+v", proj.Checks)
	}
	if !strings.Contains(*build, "ue-build") || !strings.Contains(*build, ws) {
		t.Errorf("check = %q", *build)
	}
	t.Logf("kind=%s checks=%d build=%s", proj.Kind, len(proj.Checks), *build)
}
