// Package setup implements first-run onboarding: probing local inference
// backends and building an initial config interactively.
package setup

import (
	"context"
	"sync"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// Candidate is a probeable backend.
type Candidate struct {
	Name string
	Cfg  config.ProviderConfig
}

// Found is a reachable backend with its models.
type Found struct {
	Candidate
	Models []provider.ModelInfo
}

// DefaultCandidates covers the usual local stack plus the BE AI Engine.
func DefaultCandidates() []Candidate {
	return []Candidate{
		{"ollama", config.ProviderConfig{Type: "ollama", BaseURL: "http://localhost:11434/v1"}},
		{"llamacpp", config.ProviderConfig{Type: "openai", BaseURL: "http://localhost:8080/v1"}},
		{"lmstudio", config.ProviderConfig{Type: "openai", BaseURL: "http://localhost:1234/v1"}},
		{"vllm", config.ProviderConfig{Type: "openai", BaseURL: "http://localhost:8000/v1"}},
		{"be-ai-engine", config.ProviderConfig{Type: "openai", BaseURL: "http://localhost:9800/v1", APIKeyEnv: "BE_AI_ENGINE_KEY"}},
	}
}

// Probe checks all candidates concurrently and returns the reachable ones
// (order preserved) within the timeout.
func Probe(ctx context.Context, cands []Candidate, timeout time.Duration) []Found {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	results := make([]*Found, len(cands))
	var wg sync.WaitGroup
	for i, c := range cands {
		wg.Add(1)
		go func(i int, c Candidate) {
			defer wg.Done()
			var p provider.Provider
			if c.Cfg.Type == "ollama" {
				p = provider.NewOllama(c.Name, c.Cfg.BaseURL, c.Cfg.APIKey())
			} else {
				p = provider.NewOpenAICompat(c.Name, c.Cfg.BaseURL, c.Cfg.APIKey())
			}
			models, err := p.ListModels(ctx)
			if err != nil {
				return
			}
			results[i] = &Found{Candidate: c, Models: models}
		}(i, c)
	}
	wg.Wait()
	var found []Found
	for _, r := range results {
		if r != nil {
			found = append(found, *r)
		}
	}
	return found
}

// BuildConfig assembles a config from the wizard's selections. All probed
// candidates are kept as named providers (reachable or not) so switching
// later is one flag; the chosen backend/model become the defaults.
func BuildConfig(chosenProvider, chosenModel string) *config.Config {
	cfg := config.Default()
	cfg.Providers = map[string]config.ProviderConfig{}
	for _, c := range DefaultCandidates() {
		cfg.Providers[c.Name] = c.Cfg
	}
	if chosenProvider != "" {
		cfg.DefaultProvider = chosenProvider
	}
	if chosenModel != "" {
		cfg.Model = chosenModel
	}
	return cfg
}
