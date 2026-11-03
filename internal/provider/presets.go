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
	// KeysURL is the provider's own page for creating an API key. The
	// quick sign-in list opens it in the browser before the masked key
	// field: none of these providers issues a key to a third-party app
	// through a browser login, so the page is the honest shortcut.
	KeysURL string
}

var presets = []Preset{
	{Name: "openrouter", BaseURL: "https://openrouter.ai/api/v1", KeyEnv: "OPENROUTER_API_KEY", Docs: "https://openrouter.ai/docs"},
	{Name: "openai", BaseURL: "https://api.openai.com/v1", KeyEnv: "OPENAI_API_KEY", Docs: "https://platform.openai.com/docs", KeysURL: "https://platform.openai.com/api-keys"},
	{Name: "groq", BaseURL: "https://api.groq.com/openai/v1", KeyEnv: "GROQ_API_KEY", Docs: "https://console.groq.com/docs"},
	{Name: "deepseek", BaseURL: "https://api.deepseek.com/v1", KeyEnv: "DEEPSEEK_API_KEY", Docs: "https://api-docs.deepseek.com"},
	{Name: "mistral", BaseURL: "https://api.mistral.ai/v1", KeyEnv: "MISTRAL_API_KEY", Docs: "https://docs.mistral.ai"},
	{Name: "gemini", BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai", KeyEnv: "GEMINI_API_KEY", Docs: "https://ai.google.dev/gemini-api/docs/openai", KeysURL: "https://aistudio.google.com/apikey"},
	{Name: "anthropic", BaseURL: "https://api.anthropic.com/v1", KeyEnv: "ANTHROPIC_API_KEY", Docs: "https://docs.anthropic.com", KeysURL: "https://console.anthropic.com/settings/keys"},
}

// quickSignIn is the order of the "quick sign-in" rows both UIs show first:
// OpenRouter (a real browser sign-in) and the three providers people ask
// for by name, each of which opens its keys page instead.
var quickSignIn = []string{"openrouter", "openai", "anthropic", "gemini"}

// QuickSignIn returns the quick sign-in presets in display order.
func QuickSignIn() []Preset {
	out := make([]Preset, 0, len(quickSignIn))
	for _, name := range quickSignIn {
		if p, ok := PresetByName(name); ok {
			out = append(out, p)
		}
	}
	return out
}

// OtherPresets returns the presets that are not quick sign-in rows.
func OtherPresets() []Preset {
	var out []Preset
	for _, p := range presets {
		quick := false
		for _, q := range quickSignIn {
			if p.Name == q {
				quick = true
				break
			}
		}
		if !quick {
			out = append(out, p)
		}
	}
	return out
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
