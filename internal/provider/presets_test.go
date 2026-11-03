package provider

import "testing"

func TestPresetsVerbatim(t *testing.T) {
	want := map[string][2]string{
		"openrouter": {"https://openrouter.ai/api/v1", "OPENROUTER_API_KEY"},
		"openai":     {"https://api.openai.com/v1", "OPENAI_API_KEY"},
		"groq":       {"https://api.groq.com/openai/v1", "GROQ_API_KEY"},
		"deepseek":   {"https://api.deepseek.com/v1", "DEEPSEEK_API_KEY"},
		"mistral":    {"https://api.mistral.ai/v1", "MISTRAL_API_KEY"},
		"gemini":     {"https://generativelanguage.googleapis.com/v1beta/openai", "GEMINI_API_KEY"},
		"anthropic":  {"https://api.anthropic.com/v1", "ANTHROPIC_API_KEY"},
	}
	if len(Presets()) != len(want) || Presets()[0].Name != "openrouter" {
		t.Fatalf("openrouter must lead: %+v", Presets())
	}
	for n, w := range want {
		p, ok := PresetByName(n)
		pc := p.ProviderConfig()
		if !ok || p.BaseURL != w[0] || p.KeyEnv != w[1] || pc.Type != "openai" || !pc.Online || pc.APIKeyEnv != w[1] || pc.BaseURL != w[0] {
			t.Errorf("%s: %+v %+v", n, p, pc)
		}
	}
	if _, ok := PresetByName("nope"); ok {
		t.Error("unknown preset found")
	}
}

func TestQuickSignInPresetsNameTheirKeysPage(t *testing.T) {
	want := map[string]string{
		"openai":    "https://platform.openai.com/api-keys",
		"anthropic": "https://console.anthropic.com/settings/keys",
		"gemini":    "https://aistudio.google.com/apikey",
	}
	for name, url := range want {
		p, ok := PresetByName(name)
		if !ok || p.KeysURL != url {
			t.Errorf("%s: KeysURL %q, want %q", name, p.KeysURL, url)
		}
	}
	if p, _ := PresetByName("openrouter"); p.KeysURL != "" {
		t.Errorf("openrouter signs in with the browser; it has no keys page: %q", p.KeysURL)
	}
	if got := QuickSignIn(); len(got) != 4 || got[0].Name != "openrouter" || got[1].Name != "openai" || got[2].Name != "anthropic" || got[3].Name != "gemini" {
		t.Errorf("QuickSignIn order: %v", got)
	}
}
