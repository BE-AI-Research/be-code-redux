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

func (noopLedger) Plan(string, []string) string                  { return "" }
func (noopLedger) Add(string, string) (string, error)            { return "", nil }
func (noopLedger) SetStatusText(string, string, string) error    { return nil }
func (noopLedger) Note(string, string, string, bool, bool) error { return nil }
func (noopLedger) ShowText(string) string                        { return "" }

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
	st, err := engine.Open(reg.Root, ag.Session.ID, resumed,
		engine.Limits{NotesCap: cfg.Engine.NotesCap, ItemCap: cfg.Engine.ItemCap, NodeCap: cfg.Engine.NodeCap})
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
	// The tools reach the store through the agent's fence, never directly: a
	// store that panics inside a task call is detached with one notice, like
	// one that panics anywhere else, instead of taking the tool loop down.
	registerEngineTools(cfg, reg, ag.TaskLedger(), baselineFunc(ag))
	ag.RefreshSystem()
}

// baselineFunc reports where the current task began, read straight from the
// store every time. It keeps no state of its own: RunFull records the
// porcelain text as each task starts, and anything cached here would pair a
// later task's head with the first task's dirty list.
func baselineFunc(ag *agent.Agent) tools.BaselineFunc {
	return ag.EngineBaseline
}
