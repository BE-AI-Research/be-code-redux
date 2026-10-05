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
// Among matches, longest substring wins, so "deepseek-r1:..." matches "deepseek-r1"
// (11 chars) not "deepseek" (8 chars). Online vendor models (e.g. "openai/gpt-4")
// use Compat "never" for native tool calls; local models vary.
var table = []struct {
	match    string
	anchored bool // if true, match must be at start or after '/' to avoid false positives
	p        Profile
}{
	{"qwen3", false, Profile{Family: "qwen3", Compat: "auto", Temperature: 0.2, StripThink: true,
		Notes: "reasoning tags stripped; native tool calls usually fine via Ollama"}},
	{"qwen", false, Profile{Family: "qwen", Compat: "auto", Temperature: 0.2,
		Notes: "solid native tool calling"}},
	// Online vendor models (OpenRouter, Together, etc.) are anchored to avoid
	// capturing distilled models like "Qwen3-Claude-Distill" or "olmo3".
	{"deepseek-v3", true, Profile{Family: "deepseek-v3", Compat: "never", Temperature: 0.2}},
	{"deepseek-chat", true, Profile{Family: "deepseek-chat", Compat: "never", Temperature: 0.2}},
	{"o3", true, Profile{Family: "o3", Compat: "never", Temperature: -1,
		Notes: "reasoning model; temperature ignored"}},
	{"o4", true, Profile{Family: "o4", Compat: "never", Temperature: -1}},
	{"gpt-", true, Profile{Family: "gpt", Compat: "never", Temperature: 0.2}},
	{"claude", true, Profile{Family: "claude", Compat: "never", Temperature: 0.2}},
	{"gemini", true, Profile{Family: "gemini", Compat: "never", Temperature: 0.2}},
	// Local models
	{"deepseek-r1", false, Profile{Family: "deepseek-r1", Compat: "always", Temperature: 0.3, StripThink: true,
		Notes: "reasoning model: embedded tool calls + think-stripping"}},
	{"deepseek", false, Profile{Family: "deepseek", Compat: "auto", Temperature: 0.2,
		Notes: "coder variants are strong at diffs"}},
	{"gemma", false, Profile{Family: "gemma", Compat: "always", Temperature: 0.3,
		Notes: "no native tool-call training; embedded format required (see BE-MCPql)"}},
	{"llama", false, Profile{Family: "llama", Compat: "auto", Temperature: 0.3,
		Notes: "3.1+ handle native tool calls"}},
	{"mistral", false, Profile{Family: "mistral", Compat: "auto", Temperature: 0.25,
		Notes: "native tool calls supported"}},
	{"codestral", false, Profile{Family: "codestral", Compat: "auto", Temperature: 0.2,
		Notes: "code-tuned mistral"}},
	{"phi", false, Profile{Family: "phi", Compat: "always", Temperature: 0.3,
		Notes: "small; embedded calls more reliable"}},
	{"granite", false, Profile{Family: "granite", Compat: "auto", Temperature: 0.2, Notes: ""}},
	{"starcoder", false, Profile{Family: "starcoder", Compat: "always", Temperature: 0.2,
		Notes: "completion-oriented; embedded calls"}},
	{"codellama", false, Profile{Family: "codellama", Compat: "always", Temperature: 0.2,
		Notes: "pre-tool-era; embedded calls"}},
	{"gpt-oss", false, Profile{Family: "gpt-oss", Compat: "auto", Temperature: 0.3, StripThink: true,
		Notes: "reasoning output stripped"}},
}

// Detect returns the profile for a model name (e.g. "qwen3:8b",
// "deepseek-r1:14b-qwen-distill-q4_K_M"). Among matching rows the longest
// match wins, so "deepseek-r1:...-qwen-distill" resolves to deepseek-r1,
// not qwen, and "codellama" beats "llama". Anchored rows (online vendors)
// match only at the start of the name or after a '/' to avoid false positives
// like matching "claude" in "Qwen3-Claude-Distill".
func Detect(model string) Profile {
	m := strings.ToLower(model)
	best := defaultProfile
	bestLen := 0
	for _, row := range table {
		// Check if the match string appears in the model name
		var found bool
		idx := 0
		for {
			nextIdx := strings.Index(m[idx:], row.match)
			if nextIdx == -1 {
				break
			}
			actualIdx := idx + nextIdx
			// For anchored rows, check if match is at start or after '/'
			if row.anchored {
				if actualIdx == 0 || (actualIdx > 0 && m[actualIdx-1] == '/') {
					found = true
					break
				}
			} else {
				found = true
				break
			}
			idx = actualIdx + 1
		}
		if found && len(row.match) > bestLen {
			best, bestLen = row.p, len(row.match)
		}
	}
	return best
}
