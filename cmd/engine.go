package cmd

import (
	"fmt"
	"os"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// noopLedger stands in for the store when the tools are registered without
// one (a store that would not open, and tests): the task tool still exists
// and still answers, it simply remembers nothing.
type noopLedger struct{}

func (noopLedger) SetPlan(string, []string)                 {}
func (noopLedger) SetStep(int, string) error                { return nil }
func (noopLedger) AddNote(string, string, bool, bool) error { return nil }

// registerEngineTools adds task and the git lookups per cfg.Engine. An
// engine.tools value that is neither full nor minimal warns once and is
// treated as full, the same way an unusable ide.review is.
func registerEngineTools(cfg *config.Config, reg *tools.Registry, ledger tools.TaskLedger, baseline tools.BaselineFunc) {
	if !cfg.Engine.Enabled {
		return
	}
	minimal := cfg.Engine.Tools == "minimal"
	if !minimal && cfg.Engine.Tools != "" && cfg.Engine.Tools != "full" {
		fmt.Fprintf(os.Stderr, "warn: engine.tools %q is not one of full|minimal; using full\n", cfg.Engine.Tools)
	}
	if ledger == nil {
		ledger = noopLedger{}
	}
	reg.AddTool(tools.NewTask(ledger))
	for _, t := range tools.NewGitTools(reg, baseline, minimal) {
		reg.AddTool(t)
	}
}

// attachEngine opens the workspace's working memory once the session id is
// known and wires the tools. A store that cannot be opened is a warning,
// not a failure: the session runs as it did without one.
func attachEngine(cfg *config.Config, reg *tools.Registry, ag *agent.Agent, resumed bool) {
	if !cfg.Engine.Enabled || ag.Session == nil {
		return
	}
	st, err := engine.Open(reg.Root, ag.Session.ID, resumed, cfg.Engine.NotesCap)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: engine: %v; continuing without working memory\n", err)
		// The store is what remembers; the tools are what the model can
		// call. Losing the first must not silently lose the second, or a
		// prompt that still advertises task and the git lookups would be
		// describing tools that are not registered.
		registerEngineTools(cfg, reg, nil, nil)
		ag.RefreshSystem()
		return
	}
	ag.SetEngine(st)
	registerEngineTools(cfg, reg, st, baselineFunc(st))
	ag.RefreshSystem()
}

// baselineFunc reports where the current task began, read straight from the
// ledger every time. It keeps no state of its own: RunFull records the
// porcelain text as each task starts, and anything cached here would pair a
// later task's head with the first task's dirty list.
func baselineFunc(st *engine.Store) tools.BaselineFunc {
	return func() (string, string) {
		b := st.Ledger().Baseline
		return b.Head, b.Dirty
	}
}
