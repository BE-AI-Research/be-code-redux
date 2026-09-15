package cmd

import (
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
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
