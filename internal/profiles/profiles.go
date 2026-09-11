// Package profiles adapts BE-Code to specific local model families. Local
// models differ wildly in tool-call reliability, reasoning-tag emission,
// and temperature sensitivity; a profile encodes what works so users don't
// tune per model by hand. Explicit config always overrides a profile.
package profiles

import "strings"

// Profile is the tuning set for a model family.
type Profile struct {
	Family string
	// Compat suggests the tool-call mode: "auto", "always" (embedded
	// prompts only — for families with broken native tool support), or
	// "never".
	Compat string
	// Temperature suggestion for coding work (<0 means no opinion).
	Temperature float64
	// StripThink removes <think>/<thought> reasoning blocks from output
	// (emitted by reasoning-tuned families; they pollute transcripts and
	// burn context if fed back).
	StripThink bool
	// Notes is a one-liner shown by `be-code doctor`.
	Notes string
}

var defaultProfile = Profile{Family: "generic", Compat: "auto", Temperature: -1}

// table is ordered: first substring match on the lowercase model name wins.
var table = []struct {
	match string
	p     Profile
}{
	{"qwen3", Profile{Family: "qwen3", Compat: "auto", Temperature: 0.2, StripThink: true,
		Notes: "reasoning tags stripped; native tool calls usually fine via Ollama"}},
	{"qwen", Profile{Family: "qwen", Compat: "auto", Temperature: 0.2,
		Notes: "solid native tool calling"}},
	{"deepseek-r1", Profile{Family: "deepseek-r1", Compat: "always", Temperature: 0.3, StripThink: true,
		Notes: "reasoning model: embedded tool calls + think-stripping"}},
	{"deepseek", Profile{Family: "deepseek", Compat: "auto", Temperature: 0.2,
		Notes: "coder variants are strong at diffs"}},
	{"gemma", Profile{Family: "gemma", Compat: "always", Temperature: 0.3,
		Notes: "no native tool-call training; embedded format required (see BE-MCPql)"}},
	{"llama", Profile{Family: "llama", Compat: "auto", Temperature: 0.3,
		Notes: "3.1+ handle native tool calls"}},
	{"mistral", Profile{Family: "mistral", Compat: "auto", Temperature: 0.25,
		Notes: "native tool calls supported"}},
	{"codestral", Profile{Family: "codestral", Compat: "auto", Temperature: 0.2,
		Notes: "code-tuned mistral"}},
	{"phi", Profile{Family: "phi", Compat: "always", Temperature: 0.3,
		Notes: "small; embedded calls more reliable"}},
	{"granite", Profile{Family: "granite", Compat: "auto", Temperature: 0.2, Notes: ""}},
	{"starcoder", Profile{Family: "starcoder", Compat: "always", Temperature: 0.2,
		Notes: "completion-oriented; embedded calls"}},
	{"codellama", Profile{Family: "codellama", Compat: "always", Temperature: 0.2,
		Notes: "pre-tool-era; embedded calls"}},
	{"gpt-oss", Profile{Family: "gpt-oss", Compat: "auto", Temperature: 0.3, StripThink: true,
		Notes: "reasoning output stripped"}},
}

// Detect returns the profile for a model name (e.g. "qwen3:8b",
// "deepseek-r1:14b-qwen-distill-q4_K_M"). Among matching rows the longest
// match wins, so "deepseek-r1:...-qwen-distill" resolves to deepseek-r1,
// not qwen, and "codellama" beats "llama".
func Detect(model string) Profile {
	m := strings.ToLower(model)
	best := defaultProfile
	bestLen := 0
	for _, row := range table {
		if strings.Contains(m, row.match) && len(row.match) > bestLen {
			best, bestLen = row.p, len(row.match)
		}
	}
	return best
}
