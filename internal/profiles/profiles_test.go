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
