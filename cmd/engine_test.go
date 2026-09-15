package cmd

import (
	"path/filepath"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// TestEngineToolsFollowTheConfig: engine.tools picks how much of the git
// side the model gets, and engine.enabled removes all of it.
func TestEngineToolsFollowTheConfig(t *testing.T) {
	reg, _ := tools.NewRegistry(t.TempDir(), func(string, string) bool { return true })
	cfg := config.Default()
	registerEngineTools(cfg, reg, nil, func() (string, string) { return "", "" })
	names := map[string]bool{}
	for _, n := range reg.Names() {
		names[n] = true
	}
	for _, want := range []string{"task", "lookup", "history", "show", "changes"} {
		if !names[want] {
			t.Fatalf("full: %s missing (%v)", want, reg.Names())
		}
	}
	reg2, _ := tools.NewRegistry(t.TempDir(), func(string, string) bool { return true })
	cfg.Engine.Tools = "minimal"
	registerEngineTools(cfg, reg2, nil, nil)
	names = map[string]bool{}
	for _, n := range reg2.Names() {
		names[n] = true
	}
	if !names["task"] || !names["lookup"] || names["history"] {
		t.Fatalf("minimal: %v", reg2.Names())
	}
	reg3, _ := tools.NewRegistry(t.TempDir(), func(string, string) bool { return true })
	cfg.Engine.Enabled = false
	registerEngineTools(cfg, reg3, nil, nil)
	if len(reg3.Names()) != len(reg.Names())-5 {
		t.Fatalf("disabled registered tools: %v", reg3.Names())
	}
}

// TestBaselineFollowsTheLedger: every task records its own baseline, and the
// closure the changes tool holds must report the current one — a cached
// dirty list would describe the task before this one.
func TestBaselineFollowsTheLedger(t *testing.T) {
	st, err := engine.OpenAt(filepath.Join(t.TempDir(), "e"), t.TempDir(), "s", false, 4096)
	if err != nil {
		t.Fatal(err)
	}
	baseline := baselineFunc(st)
	st.SetBaseline(engine.Baseline{Head: "1111111111111111111111111111111111111111", Dirty: " M a.txt\n"})
	head1, dirty1 := baseline()
	st.SetBaseline(engine.Baseline{Head: "2222222222222222222222222222222222222222", Dirty: " M b.txt\n"})
	head2, dirty2 := baseline()
	if head1 == head2 || dirty1 == dirty2 {
		t.Fatalf("baseline is stale: %q/%q then %q/%q", head1, dirty1, head2, dirty2)
	}
	if dirty2 != " M b.txt\n" {
		t.Fatalf("second baseline: %q", dirty2)
	}
}
