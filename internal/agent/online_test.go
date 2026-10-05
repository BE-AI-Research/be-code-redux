package agent

import (
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
)

func TestOnlineWindowPrecedence(t *testing.T) {
	got := OnlineWindow(provider.ModelInfo{ContextLength: 200000}, provider.Preset{Window: 128000}, 32000)
	if got != 200000 {
		t.Fatalf("listing should win, got %d", got)
	}
}

func TestOnlineWindowFallsBackWhenListingLacksIt(t *testing.T) {
	if got := OnlineWindow(provider.ModelInfo{}, provider.Preset{Window: 128000}, 32000); got != 128000 {
		t.Fatalf("preset: %d", got)
	}
	if got := OnlineWindow(provider.ModelInfo{}, provider.Preset{}, 32000); got != 32000 {
		t.Fatalf("config: %d", got)
	}
	if got := OnlineWindow(provider.ModelInfo{}, provider.Preset{}, 0); got != 0 {
		t.Fatalf("none: %d", got)
	}
}

func TestPricingForLongestSubstring(t *testing.T) {
	pre := provider.Preset{
		PromptPrice:     map[string]float64{"gpt-4": 1, "gpt-4o-mini": 2},
		CompletionPrice: map[string]float64{"gpt-4": 3, "gpt-4o-mini": 4},
	}
	p := PricingFor("OpenAI/GPT-4o-mini-2024", provider.ModelInfo{}, pre)
	if !p.Known || p.Prompt != 2 || p.Completion != 4 {
		t.Fatalf("%+v", p)
	}
	if PricingFor("llama", provider.ModelInfo{}, pre).Known {
		t.Fatal("unknown model should not be priced")
	}
	l := PricingFor("x", provider.ModelInfo{PromptPrice: 5e-6, CompletionPrice: 1e-5}, pre)
	if !l.Known || l.Prompt != 5e-6 {
		t.Fatalf("%+v", l)
	}
}
