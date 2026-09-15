package cmd

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/store"
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
	// An unknown value warns and is treated as full.
	reg4, _ := tools.NewRegistry(t.TempDir(), func(string, string) bool { return true })
	cfg.Engine.Tools = "some-typo"
	warning := captureStderr(t, func() { registerEngineTools(cfg, reg4, nil, nil) })
	if warning != "warn: engine.tools \"some-typo\" is not one of full|minimal; using full\n" {
		t.Fatalf("warning: %q", warning)
	}
	if len(reg4.Names()) != len(reg.Names()) {
		t.Fatalf("unknown engine.tools did not fall back to full: %v", reg4.Names())
	}

	reg3, _ := tools.NewRegistry(t.TempDir(), func(string, string) bool { return true })
	cfg.Engine.Enabled = false
	registerEngineTools(cfg, reg3, nil, nil)
	if len(reg3.Names()) != len(reg.Names())-5 {
		t.Fatalf("disabled registered tools: %v", reg3.Names())
	}
}

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = orig
	w.Close()
	b, _ := io.ReadAll(r)
	r.Close()
	return string(b)
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

// TestEngineToolsSurviveAStoreThatWillNotOpen: the store is advisory, the
// tools are not. When engine.Open fails the model must still get task (over
// the no-op ledger) and the git lookups, and the system prompt must be
// recomposed so it advertises exactly what is registered.
func TestEngineToolsSurviveAStoreThatWillNotOpen(t *testing.T) {
	home := t.TempDir()
	// ~/.be-code is a regular file, so config.Dir()'s MkdirAll fails and
	// engine.Open can never reach the store directory.
	if err := os.WriteFile(filepath.Join(home, ".be-code"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	ws := t.TempDir()
	reg, err := tools.NewRegistry(ws, func(string, string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	ag := agent.New(cfg, nil, "test-model", reg, "")
	ag.SetSession(&store.Session{ID: "sess-1"})

	attachEngine(cfg, reg, ag, false)

	names := map[string]bool{}
	for _, n := range reg.Names() {
		names[n] = true
	}
	for _, want := range []string{"task", "lookup", "history", "show", "changes"} {
		if !names[want] {
			t.Fatalf("%s missing after a failed engine.Open: %v", want, reg.Names())
		}
	}
	// The task tool still answers, over the no-op ledger.
	r := reg.Dispatch(context.Background(), provider.ToolCall{
		ID: "c1", Name: "task",
		Arguments: `{"action":"plan","task":"do a thing","steps":["one"]}`,
	})
	if r.IsError {
		t.Fatalf("task over noopLedger: %+v", r)
	}
	// RefreshSystem ran: the prompt names the git tools it has, but not the
	// Working memory block it does not have.
	sys := ag.History.System.Content
	if !strings.Contains(sys, "call lookup") || strings.Contains(sys, "Context is limited and does not survive compaction") {
		t.Fatalf("degraded prompt wrong:\n%s", sys)
	}
}
