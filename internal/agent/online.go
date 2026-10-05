package agent

import (
	"strings"

	"github.com/brown-enterprises/be-code/internal/provider"
)

// Pricing is a model's cost in USD per token. Known is false when neither the
// provider's listing nor a preset table said; Prompt and Completion are then 0.
type Pricing struct {
	Prompt, Completion float64
	Known              bool
}

// SetOnline marks the main model as served by an online provider. The zero
// Pricing means "prices unknown". An empty provider marks it local again.
func (a *Agent) SetOnline(providerName, keyEnv string, p Pricing) {
	a.onlineMu.Lock()
	defer a.onlineMu.Unlock()
	a.onlineName = providerName
	a.pricing = p
	if providerName != "" && keyEnv != "" {
		a.KeyEnv = keyEnv
	}
}

// Online reports the online provider serving the main model, if any.
func (a *Agent) Online() (string, bool) {
	a.onlineMu.Lock()
	defer a.onlineMu.Unlock()
	return a.onlineName, a.onlineName != ""
}

// Pricing is the main model's per-token cost, as learned by SetOnline.
func (a *Agent) Pricing() Pricing {
	a.onlineMu.Lock()
	defer a.onlineMu.Unlock()
	return a.pricing
}

// OnlineWindow is the first positive of the listing's context length, the
// preset's window and the configured one; 0 means none knew, and the caller
// lets the ordinary budget path apply its default and say so.
func OnlineWindow(listed provider.ModelInfo, preset provider.Preset, configured int) int {
	for _, w := range []int{listed.ContextLength, preset.Window, configured} {
		if w > 0 {
			return w
		}
	}
	return 0
}

// PricingFor takes a listing's prices when it carries any, else the preset's
// substring tables (the longest key found in the lowercased model ID wins),
// else Pricing{Known: false}.
func PricingFor(model string, listed provider.ModelInfo, preset provider.Preset) Pricing {
	if listed.PromptPrice > 0 || listed.CompletionPrice > 0 {
		return Pricing{Prompt: listed.PromptPrice, Completion: listed.CompletionPrice, Known: true}
	}
	id := strings.ToLower(model)
	pp, pok := longestKey(id, preset.PromptPrice)
	cp, cok := longestKey(id, preset.CompletionPrice)
	if !pok && !cok {
		return Pricing{}
	}
	return Pricing{Prompt: pp, Completion: cp, Known: true}
}

func longestKey(id string, table map[string]float64) (float64, bool) {
	best, found, v := -1, false, 0.0
	for k, price := range table {
		lk := strings.ToLower(k)
		if strings.Contains(id, lk) && len(lk) > best {
			best, found, v = len(lk), true, price
		}
	}
	return v, found
}
