// internal/ui/coworkeradd.go
package ui

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// coworkerModels lists a configured provider's models; tests replace it.
var coworkerModels = func(ctx context.Context, cfg *config.Config, name string) ([]provider.ModelDetail, error) {
	p, err := provider.FromConfig(cfg, name)
	if err != nil {
		return nil, err
	}
	return provider.ModelDetails(ctx, p)
}

// AddCoworker is the one write behind /coworkers add in both UIs: the
// agent validates and registers the consult tool, then the entry is saved
// so the next launch has it too. The line says what became usable and
// what waits for a relaunch (a sub-agent needs the lanes built at start).
func AddCoworker(cfg *config.Config, ag *agent.Agent, cw config.CoworkerConfig) (string, error) {
	if err := ag.AddCoworker(cw); err != nil {
		return "", err
	}
	cws := ag.Coworkers()
	added := cws[len(cws)-1] // the agent's copy carries the online flag it decided
	cfg.Coworkers = append(cfg.Coworkers, added)
	if err := cfg.Save(); err != nil {
		return "", err
	}
	line := fmt.Sprintf("co-worker %s added (%s/%s); consult it with /consult %s <question>, or the agent will when it needs to", added.Name, added.Provider, added.Model, added.Name)
	if added.Online {
		line += "; it is online, so the first consultation asks before any code is sent"
	}
	if added.SubAgent {
		line += "; it can own task steps from the next launch"
	}
	return line, nil
}

// RemoveCoworker is /coworkers remove <name> in both UIs.
func RemoveCoworker(cfg *config.Config, ag *agent.Agent, name string) (string, error) {
	if err := ag.RemoveCoworker(name); err != nil {
		return "", err
	}
	kept := cfg.Coworkers[:0]
	for _, cw := range cfg.Coworkers {
		if cw.Name != name {
			kept = append(kept, cw)
		}
	}
	cfg.Coworkers = kept
	if err := cfg.Save(); err != nil {
		return "", err
	}
	return "co-worker " + name + " removed", nil
}

// ProviderNames is every configured provider, sorted, for the pickers.
func ProviderNames(cfg *config.Config) []string {
	names := make([]string, 0, len(cfg.Providers))
	for n := range cfg.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// SuggestCoworkerName is the default name offered for a model: its last
// path segment, cut at the first colon, so "anthropic/claude-x:latest" and
// "qwen3:32b" offer "claude-x" and "qwen3".
func SuggestCoworkerName(model string) string {
	s := model
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[:i]
	}
	return s
}

// CoworkerAdd is /coworkers add on numbered prompts: provider, model from
// its listing, name, sub-agent yes/no, then AddCoworker.
func CoworkerAdd(ctx context.Context, cfg *config.Config, ag *agent.Agent, p Prompter) (string, error) {
	cancelled := func() (string, error) { p.Say("cancelled; nothing was added"); return "", nil }
	names := ProviderNames(cfg)
	if len(names) == 0 {
		return "", fmt.Errorf("no providers are configured")
	}
	p.Say("Providers:")
	for i, n := range names {
		desc := cfg.Providers[n].BaseURL
		if config.ProviderIsOnline(cfg.Providers[n]) {
			desc += " (online)"
		}
		p.Say(fmt.Sprintf("  [%d] %-12s %s", i+1, n, desc))
	}
	n, ok := askNumber(ctx, p, "Choose a provider", 1, len(names))
	if !ok {
		return cancelled()
	}
	prov := names[n-1]
	p.Say("listing models on " + prov + "…")
	models, err := coworkerModels(ctx, cfg, prov)
	if err != nil {
		p.Say(err.Error())
		return cancelled()
	}
	model := ""
	if len(models) == 0 {
		p.Say(prov + " lists no models")
		m, ok := p.Ask(ctx, "model name: ")
		if !ok {
			return cancelled()
		}
		model = strings.TrimSpace(m)
	} else {
		show := models
		if len(show) > 20 {
			show = show[:20]
			p.Say(fmt.Sprintf("  (showing first 20 of %d)", len(models)))
		}
		for i, m := range show {
			p.Say(fmt.Sprintf("  [%d] %s", i+1, m.ID))
		}
		n, ok := askNumber(ctx, p, "Choose a model", 1, len(show))
		if !ok {
			return cancelled()
		}
		model = show[n-1].ID
	}
	suggested := SuggestCoworkerName(model)
	name, ok := p.Ask(ctx, fmt.Sprintf("name for this co-worker [%s]: ", suggested))
	if !ok {
		return cancelled()
	}
	if name = strings.TrimSpace(name); name == "" {
		name = suggested
	}
	a, ok := p.Ask(ctx, "may it own task steps as a sub-agent? [y/N] ")
	if !ok {
		return cancelled()
	}
	sub := strings.HasPrefix(strings.ToLower(strings.TrimSpace(a)), "y")
	line, err := AddCoworker(cfg, ag, config.CoworkerConfig{Name: name, Provider: prov, Model: model, SubAgent: sub})
	if err != nil {
		return "", err
	}
	p.Say(line)
	return name, nil
}
