package ui

import (
	"fmt"
	"strings"

	"github.com/brown-enterprises/be-code/internal/agent"
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
		return "usage: /packs · /packs on <name> · /packs off <name> · /packs default <name> · /packs reload"
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
	default:
		return "usage: /packs · /packs on <name> · /packs off <name> · /packs default <name> · /packs reload"
	}
	if err != nil {
		return err.Error()
	}
	return PacksReport(ag.PackRows()) + "\n  changed; the next request re-reads the whole prompt"
}
