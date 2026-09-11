package provider

import (
	"fmt"

	"github.com/brown-enterprises/be-code/internal/config"
)

// FromConfig instantiates the named provider (or the default) from config.
func FromConfig(cfg *config.Config, name string) (Provider, error) {
	if name == "" {
		name = cfg.DefaultProvider
	}
	pc, ok := cfg.Providers[name]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q (configured: %s)", name, names(cfg))
	}
	switch pc.Type {
	case "ollama":
		return NewOllama(name, pc.BaseURL, pc.APIKey()), nil
	case "openai", "":
		return NewOpenAICompat(name, pc.BaseURL, pc.APIKey()), nil
	default:
		return nil, fmt.Errorf("provider %q has unknown type %q", name, pc.Type)
	}
}

// ResolveModel picks the model: explicit flag > provider default > global.
func ResolveModel(cfg *config.Config, providerName, flagModel string) string {
	if flagModel != "" {
		return flagModel
	}
	if providerName == "" {
		providerName = cfg.DefaultProvider
	}
	if pc, ok := cfg.Providers[providerName]; ok && pc.DefaultModel != "" {
		return pc.DefaultModel
	}
	return cfg.Model
}

func names(cfg *config.Config) string {
	s := ""
	for n := range cfg.Providers {
		if s != "" {
			s += ", "
		}
		s += n
	}
	return s
}
