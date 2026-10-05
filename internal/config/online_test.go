package config

import (
	"strings"
	"testing"
)

func TestProviderIsOnline(t *testing.T) {
	cases := map[string]struct {
		pc   ProviderConfig
		want bool
	}{
		"local ollama":      {ProviderConfig{BaseURL: "http://192.168.1.150:11434/v1"}, false},
		"remote unflagged":  {ProviderConfig{BaseURL: "https://openrouter.ai/api/v1"}, true},
		"local but flagged": {ProviderConfig{BaseURL: "http://localhost:8080/v1", Online: true}, true},
	}
	for name, c := range cases {
		if got := ProviderIsOnline(c.pc); got != c.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestOnlineWarnings(t *testing.T) {
	c := Default()
	c.Providers["or"] = ProviderConfig{Type: "openai", BaseURL: "https://openrouter.ai/api/v1"}
	c.Providers["flagged"] = ProviderConfig{Type: "openai", BaseURL: "https://api.openai.com/v1", Online: true}
	ws := c.OnlineWarnings()
	if len(ws) != 1 || !strings.Contains(ws[0], `"or"`) || !strings.Contains(ws[0], "treating it as online") {
		t.Fatalf("%v", ws)
	}
}

func TestOnlineConfigDefaults(t *testing.T) {
	c := Default()
	if c.MaxSpendUSD != 0 || c.LocalHelper.Provider != "" {
		t.Fatalf("defaults must be off: %+v %+v", c.MaxSpendUSD, c.LocalHelper)
	}
}
