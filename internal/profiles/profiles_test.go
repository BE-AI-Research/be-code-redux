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
	// Test vendor/model names that should resolve to online families
	vendorCases := map[string]string{
		"openai/gpt-4.1":            "gpt",
		"openai/o3-mini":            "o3",
		"openai/o4-mini":            "o4",
		"anthropic/claude-sonnet-5": "claude",
		"google/gemini-3.5-flash":   "gemini",
		"deepseek/deepseek-chat":    "deepseek-chat",
		"deepseek/deepseek-v3":      "deepseek-v3",
		"deepseek/deepseek-r1":      "deepseek-r1",
		"hf.co/unsloth/Qwen3.8-27B": "qwen3",
	}
	for id, fam := range vendorCases {
		if p := Detect(id); p.Family != fam {
			t.Errorf("vendor %s: got %s, want %s", id, p.Family, fam)
		}
	}

	// Test plain model names (simulating a user typing them directly)
	plainCases := map[string]string{
		"o4-mini":           "o4",
		"gpt-4.1":           "gpt",
		"claude-sonnet-5":   "claude",
		"gemini-2.5-pro":    "gemini",
		"deepseek-v3":       "deepseek-v3",
		"deepseek-chat":     "deepseek-chat",
		"deepseek-r1:14b":   "deepseek-r1",
		"deepseek-coder-v2": "deepseek",
	}
	for name, expectedFamily := range plainCases {
		if p := Detect(name); p.Family != expectedFamily {
			t.Errorf("plain %s: got %s, want %s", name, p.Family, expectedFamily)
		}
	}

	// Verify online families use native tool calls
	if Detect("anthropic/claude-sonnet-5").Compat != "never" {
		t.Error("online families use native tool calls")
	}
}

func TestLocalNamesNotCapturedByOnlineRows(t *testing.T) {
	// Test that anchored online families don't capture local model distills
	cases := map[string]string{
		// Original local models must keep their families
		"qwen3:8b":                                             "qwen3",
		"phi3:mini":                                            "phi",
		"llama3.3:70b":                                         "llama",
		"granite3.1-dense":                                     "granite",
		"gpt-oss:20b":                                          "gpt-oss",
		"deepseek-r1:14b":                                      "deepseek-r1",
		"deepseek-r1:14b-qwen-distill-q4_K_M":                  "deepseek-r1",
		"deepseek-coder-v2":                                    "deepseek",
		// Distilled models combining local and online names (must keep their base families)
		"hf.co/unsloth/Qwen3-4B-Claude-4.5-Opus-Distill-GGUF": "qwen3",
		"hf.co/x/Qwen3-8B-Gemini-2.5-Flash-Distill":           "qwen3",
		// Models that look like online names but aren't
		"olmo3:7b": "generic",
		"gpt4all":  "generic",
	}
	for name, expectedFamily := range cases {
		if p := Detect(name); p.Family != expectedFamily {
			t.Errorf("%s: got %s, want %s", name, p.Family, expectedFamily)
		}
	}
}
