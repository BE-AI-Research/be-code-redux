package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/gitctx"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// noopLedger stands in for the store when the tools are registered without
// one (tests): the task tool still exists and still answers, it simply
// remembers nothing.
type noopLedger struct{}

func (noopLedger) SetPlan(string, []string)                 {}
func (noopLedger) SetStep(int, string) error                { return nil }
func (noopLedger) AddNote(string, string, bool, bool) error { return nil }

// registerEngineTools adds task and the git lookups per cfg.Engine.
func registerEngineTools(cfg *config.Config, reg *tools.Registry, ledger tools.TaskLedger, baseline tools.BaselineFunc) {
	if !cfg.Engine.Enabled {
		return
	}
	if ledger == nil {
		ledger = noopLedger{}
	}
	reg.AddTool(tools.NewTask(ledger))
	for _, t := range tools.NewGitTools(reg, baseline, cfg.Engine.Tools == "minimal") {
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
		return
	}
	ag.SetEngine(st)
	// The porcelain text at task start is re-read lazily on the first
	// changes call; the engine stores only its hash. A documented
	// approximation: files dirtied during the task but before that first
	// call are listed as pre-existing. The stat itself is always exact.
	var dirtyText string
	baseline := func() (string, string) {
		b := st.Ledger().Baseline
		if b.Head == "" {
			return "", ""
		}
		if dirtyText == "" && b.Dirty != "" {
			dirtyText = gitctx.Porcelain(context.Background(), reg.Root)
		}
		return b.Head, dirtyText
	}
	registerEngineTools(cfg, reg, st, baseline)
	ag.RefreshSystem()
}
