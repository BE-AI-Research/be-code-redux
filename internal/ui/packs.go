package ui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/packsrc"
)

// PacksReport is what /packs prints: every pack found on the search path,
// whether it is active and why. One function for both UIs so the two cannot
// drift apart.
func PacksReport(rows []agent.PackRow) string {
	if len(rows) == 0 {
		return "no packs found (install one in ~/.be-code/packs/<name>/)"
	}
	var b strings.Builder
	b.WriteString("packs:\n")
	for _, r := range rows {
		mark := " "
		if r.Active {
			mark = "*"
		}
		fmt.Fprintf(&b, "  %s %-12s %-8s %-8s %s", mark, r.Name, r.Version, r.Source, r.Why)
		if r.CoreBudget > 0 {
			fmt.Fprintf(&b, " · core ~%d/%d tokens", r.CoreTokens, r.CoreBudget)
		}
		b.WriteString("\n")
	}
	b.WriteString("  (* active · /packs on|off|default <name> · /packs reload)")
	return b.String()
}

// PacksCommand applies /packs on|off|default|reload and returns the line to
// show. Both UIs call it, so the behaviour and the wording are shared.
//
// Any of these rewrites the system prompt, which costs the next request a
// full cold read of the prompt cache — said out loud, because an unexplained
// slow turn after a command looks like a bug.
// IsPacksBlocking reports whether a /packs subcommand can raise the pack
// trust approval. Everything that re-runs activation can: a project pack that
// has not been approved yet is asked about the moment it becomes a candidate.
// A bare listing cannot, so it stays inline where it is cheapest.
//
// This exists for the same reason IsScheduleBlocking does, and the cost of
// getting it wrong is worse: Session.Ask relocks the session mutex that
// Update already holds, so an inline question froze every attached terminal
// with the alt screen up and Ctrl-C unable to reach the program.
func IsPacksBlocking(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch strings.ToLower(args[0]) {
	case "reload", "on", "off", "default", "clear", "auto", "add", "update", "remove":
		return true
	}
	return false
}

func PacksCommand(ag *agent.Agent, fields []string) string {
	if len(fields) < 2 {
		return PacksReport(ag.PackRows())
	}
	verb := strings.ToLower(fields[1])
	if verb == "reload" {
		ag.LoadPacks()
		return PacksReport(ag.PackRows()) + "\n  reloaded; the next request re-reads the whole prompt"
	}
	if len(fields) < 3 {
		return packsUsage()
	}
	name := fields[2]
	var err error
	switch verb {
	case "on":
		err = ag.ForcePack(name, true, false)
	case "off":
		err = ag.ForcePack(name, false, false)
	case "default", "clear", "auto":
		err = ag.ForcePack(name, false, true)
	case "add":
		return packsAdd(ag, fields[2:])
	case "update", "remove":
		return packsManage(ag, verb, name)
	default:
		return packsUsage()
	}
	if err != nil {
		return err.Error()
	}
	return PacksReport(ag.PackRows()) + "\n  changed; the next request re-reads the whole prompt"
}

func packsUsage() string {
	return "usage: /packs · /packs add <name>[@<ref>] · /packs update <name> · /packs remove <name> · " +
		"/packs on <name> · /packs off <name> · /packs default <name> · /packs reload"
}

// packOptionsFor is the agent's own workspace, trust file and approval seam.
// The approver is Tools.Approve, so an install asks through whichever UI is
// running — and a nil one is a refusal, which packsrc enforces.
func packOptionsFor(ag *agent.Agent) (packsrc.Options, error) {
	base, err := config.Dir()
	if err != nil {
		return packsrc.Options{}, err
	}
	root := ag.Tools.Root
	return packsrc.Options{
		Workspace: root,
		TrustPath: filepath.Join(base, "engine", engine.Key(root), "packs.json"),
		Approve:   ag.Tools.Approve,
	}, nil
}

func packsAdd(ag *agent.Agent, args []string) string {
	if len(args) == 0 {
		return packsUsage()
	}
	name, ref, _ := strings.Cut(args[0], "@")
	spec := ""
	if len(args) > 1 {
		spec = args[1]
	}
	if spec == "" && ag.Cfg != nil {
		spec = ag.Cfg.Packs.Source
	}
	s, err := packsrc.Resolve(spec, ref)
	if err != nil {
		return err.Error()
	}
	o, err := packOptionsFor(ag)
	if err != nil {
		return err.Error()
	}
	res, err := packsrc.Install(context.Background(), s, name, o)
	if err != nil {
		return err.Error()
	}
	ag.LoadPacks()
	return fmt.Sprintf("installed %s %s into %s\n  commit it so the project carries its own pack setup",
		res.Pack, res.Version, res.Dir)
}

func packsManage(ag *agent.Agent, verb, name string) string {
	o, err := packOptionsFor(ag)
	if err != nil {
		return err.Error()
	}
	if verb == "remove" {
		if err := packsrc.Remove(name, o); err != nil {
			return err.Error()
		}
		ag.LoadPacks()
		return "removed " + name
	}
	res, err := packsrc.Update(context.Background(), name, o)
	if err != nil {
		return err.Error()
	}
	ag.LoadPacks()
	return fmt.Sprintf("updated %s %s", res.Pack, res.Version)
}
