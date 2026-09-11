package agent

import (
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
)

// Compat mode stores tool results as user messages wrapped in
// <tool_result>; trim's collapse pass must treat them like tool messages.
func TestTrimCollapsesEmbeddedToolResults(t *testing.T) {
	h := NewHistory("system", 800)
	big := "<tool_result name=\"read_file\" status=\"ok\">\n" + strings.Repeat("x", 2000) + "\n</tool_result>\n"
	h.Add(provider.Message{Role: provider.RoleUser, Content: "the task"})
	for i := 0; i < 10; i++ {
		h.Add(provider.Message{Role: provider.RoleAssistant, Content: "<tool_call>{\"name\":\"read_file\"}</tool_call>"})
		h.Add(provider.Message{Role: provider.RoleUser, Content: big})
	}
	msgs := h.Prompt()
	collapsed := 0
	for _, m := range msgs {
		if strings.Contains(m.Content, collapsedStub) {
			collapsed++
		}
	}
	if collapsed == 0 {
		t.Fatal("no embedded tool results were collapsed")
	}
	if msgs[1].Content != "the task" {
		t.Fatalf("task message lost: %q", msgs[1].Content)
	}
}

// The chars/4 heuristic undercounts code and tool output by ~2x on real
// tokenizers. When the server reports prompt_tokens, the estimate must
// recalibrate so the budget reflects reality.
func TestHistoryCalibrateFromServerUsage(t *testing.T) {
	h := NewHistory("sys", 10000)
	h.Add(provider.Message{Role: provider.RoleUser, Content: strings.Repeat("path/to/file.go:12: symbol\n", 400)})
	before := h.Tokens()
	// Server says this prompt was twice what we estimated.
	h.Calibrate(before * 2)
	after := h.Tokens()
	if after < before*17/10 {
		t.Fatalf("estimate did not recalibrate: before=%d after=%d", before, after)
	}
	// Calibration is clamped: an absurd report cannot collapse the estimate
	// below chars/maxCharsPerToken.
	h.Calibrate(1)
	if h.CharsPerToken > maxCharsPerToken {
		t.Fatalf("ratio not clamped: %v", h.CharsPerToken)
	}
	if h.Tokens() < before*3/5 {
		t.Fatalf("estimate collapsed: %d (before %d)", h.Tokens(), before)
	}
}

// Every prompt also carries the tools schema and needs generation headroom;
// the trim decision must count both.
func TestHistoryOverheadCountsAgainstBudget(t *testing.T) {
	h := NewHistory("sys", 1000)
	h.Add(provider.Message{Role: provider.RoleUser, Content: strings.Repeat("y", 2000)}) // ~500 tokens
	if h.Over() {
		t.Fatal("under budget without overhead")
	}
	h.Extra = 300   // tools schema
	h.Reserve = 300 // generation headroom
	if !h.Over() {
		t.Fatalf("overhead ignored: tokens=%d budget=%d", h.Tokens(), h.Budget)
	}
	if h.Limit() != 700 {
		t.Fatalf("Limit = %d, want 700", h.Limit())
	}
}
