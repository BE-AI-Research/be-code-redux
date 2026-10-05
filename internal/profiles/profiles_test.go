package profiles

import "testing"

func TestDetect(t *testing.T) {
	cases := []struct {
		model      string
		family     string
		compat     string
		stripThink bool
	}{
		{"qwen3:8b", "qwen3", "auto", true},
		{"qwen2.5-coder:7b", "qwen", "auto", false},
		{"deepseek-r1:14b-qwen-distill-q4_K_M", "deepseek-r1", "always", true},
		{"deepseek-coder-v2:16b", "deepseek", "auto", false},
		{"gemma3:12b", "gemma", "always", false},
		{"llama3.1:8b", "llama", "auto", false},
		{"codellama:13b", "codellama", "always", false},
		{"totally-unknown-model", "generic", "auto", false},
	}
	for _, c := range cases {
		p := Detect(c.model)
		if p.Family != c.family || p.Compat != c.compat || p.StripThink != c.stripThink {
			t.Errorf("%s -> %+v, want family=%s compat=%s strip=%v",
				c.model, p, c.family, c.compat, c.stripThink)
		}
	}
}

func TestOnlineFamiliesFromVendorNames(t *testing.T) {
	cases := map[string]string{
		"openai/gpt-4.1":               "gpt",
		"anthropic/claude-sonnet-5":    "claude",
		"google/gemini-3.5-flash":      "gemini",
		"openai/o4-mini":               "o4",
		"deepseek/deepseek-chat":       "deepseek-chat",
		"deepseek/deepseek-r1":         "deepseek-r1",
		"hf.co/unsloth/Qwen3.8-27B":    "qwen3",
	}
	for id, fam := range cases {
		if p := Detect(id); p.Family != fam {
			t.Errorf("%s: got %s, want %s", id, p.Family, fam)
		}
	}
	if Detect("anthropic/claude-sonnet-5").Compat != "never" {
		t.Error("online families use native tool calls")
	}
}

func TestLocalNamesNotCapturedByOnlineRows(t *testing.T) {
	cases := map[string]string{
		"qwen3:8b":           "qwen3",
		"phi3:mini":          "phi",
		"llama3.3:70b":       "llama",
		"granite3.1-dense":   "granite",
		"gpt-oss:20b":        "gpt-oss",
		"deepseek-r1:14b":    "deepseek-r1",
		"deepseek-coder-v2":  "deepseek",
	}
	for name, expectedFamily := range cases {
		if p := Detect(name); p.Family != expectedFamily {
			t.Errorf("%s: got %s, want %s", name, p.Family, expectedFamily)
		}
	}
}
