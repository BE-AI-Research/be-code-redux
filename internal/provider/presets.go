package provider

import "github.com/brown-enterprises/be-code/internal/config"

// Preset is a known online provider: an OpenAI-compatible endpoint and the
// environment variable its key comes from. Window and the price maps are
// fallbacks only; where the provider's /models listing carries them, the
// listing wins. Prices are per token, keyed by a substring of the model ID.
type Preset struct {
	Name, BaseURL, KeyEnv, Docs  string
	Window                       int
	PromptPrice, CompletionPrice map[string]float64
}

var presets = []Preset{
	{Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1", KeyEnv: "OPENROUTER_API_KEY", Docs: "https://openrouter.ai/docs"},
	{Name: "openai", BaseURL: "https://api.openai.com/v1", KeyEnv: "OPENAI_API_KEY", Docs: "https://platform.openai.com/docs"},
	{Name: "groq", BaseURL: "https://api.groq.com/openai/v1", KeyEnv: "GROQ_API_KEY", Docs: "https://console.groq.com/docs"},
	{Name: "deepseek", BaseURL: "https://api.deepseek.com/v1", KeyEnv: "DEEPSEEK_API_KEY", Docs: "https://api-docs.deepseek.com"},
	{Name: "mistral", BaseURL: "https://api.mistral.ai/v1", KeyEnv: "MISTRAL_API_KEY", Docs: "https://docs.mistral.ai"},
	{Name: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", KeyEnv: "GEMINI_API_KEY", Docs: "https://ai.google.dev/gemini-api/docs/openai"},
	{Name: "anthropic", BaseURL: "https://api.anthropic.com/v1", KeyEnv: "ANTHROPIC_API_KEY", Docs: "https://docs.anthropic.com"},
}

// Presets returns the known online providers, OpenRouter first.
func Presets() []Preset {
	return append([]Preset(nil), presets...)
}

// PresetByName finds a preset by its name.
func PresetByName(name string) (Preset, bool) {
	for _, p := range presets {
		if p.Name == name {
			return p, true
		}
	}
	return Preset{}, false
}

// ProviderConfig is the config block a preset stands for.
func (p Preset) ProviderConfig() config.ProviderConfig {
	return config.ProviderConfig{Type: "openai", BaseURL: p.BaseURL, APIKeyEnv: p.KeyEnv, Online: true}
}
