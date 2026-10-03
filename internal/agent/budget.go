package agent

import (
	"fmt"
	"os"

	"github.com/brown-enterprises/be-code/internal/provider"
)

// Reply caps for the harness's own requests. These are summaries and notes,
// not the user's turn, so they never inherit a max_tokens sized for a whole
// window — on the owner's 32k session a compaction summary sat ~17 minutes
// on one reply. A configured max_tokens below the cap still wins.
const (
	// summaryReplyTokens bounds a compaction summary: the prompt asks for
	// under 400 words (~550 tokens) plus a files list, so 2048 leaves the
	// list three times the summary's own room without inviting an essay.
	summaryReplyTokens = 2048
	// notesReplyTokens bounds the handoff briefing and an /init BECODE.md
	// (at most 150 lines, trimmed to 8 KiB ≈ 2.7k tokens on the way in).
	notesReplyTokens = 4096
	// minReplyTokens is the least a capped reply is ever given, even when
	// the prompt leaves less: a truncated summary beats an empty one, and
	// an overflowing prompt is the server's to refuse (overflow.go).
	minReplyTokens = 256
	// lowLimitTokens is where a budget stops being workable: below it the
	// conversation compacts on nearly every call.
	lowLimitTokens = 2048
)

// CapReserve is the reserve an explicit max_tokens gets in a budget of
// budget tokens: max_tokens, but never more than half of it. 0 or less means
// the budget is unknown and nothing is capped.
func CapReserve(maxTokens, budget int) int {
	if budget > 0 && maxTokens > budget/2 {
		return budget / 2
	}
	return maxTokens
}

// effectiveBudget is the budget a window gives: the window, or the
// context_tokens cap when that is smaller — the same rule ApplyWindow uses.
func (a *Agent) effectiveBudget(window int) int {
	if c := a.Cfg.ContextTokens; c > 0 && (window <= 0 || c < window) {
		return c
	}
	return window
}

// ReserveCapNote is what the session (and doctor) says when max_tokens is
// too large to reserve in full.
func ReserveCapNote(maxTokens, window, reserve int) string {
	return fmt.Sprintf("max_tokens %d leaves no room for the conversation in a %d-token window; reserving %d instead. "+
		"Change max_tokens in config (0 lets the server decide) to avoid this.", maxTokens, window, reserve)
}

// noteBudget explains, once per session each, a reserve the max_tokens cap
// lowered and a budget the reserve leaves almost nothing of. The second is
// still reachable after the cap: the profile reserve has floors (4096 for a
// thinking model) that a small window or context_tokens can fall under.
// Called after the scalars are written, never under History.mu.
func (a *Agent) noteBudget(budget, reserve int) {
	if a.quietBudget || budget <= 0 {
		return
	}
	if m := a.Cfg.MaxTokens; m > 0 && reserve < m && a.reserveCapNoted.CompareAndSwap(false, true) {
		a.announce(ReserveCapNote(m, budget, reserve))
	}
	if l := budget - reserve; l < lowLimitTokens && a.lowLimitNoted.CompareAndSwap(false, true) {
		shown := l
		if shown < 0 {
			shown = 0
		}
		a.announce(fmt.Sprintf("context budget %d minus reserve %d leaves %d tokens for the conversation (the harness keeps at least 512); "+
			"expect compaction on nearly every call. Give the model a larger window (context_window, context_tokens) or set a smaller max_tokens.",
			budget, reserve, shown))
	}
}

// announce delivers a notice to the UI, or — during wiring, before one
// exists — to stderr and the queue FlushQueuedNotices empties into the
// transcript, as cmd's startupWarn does.
func (a *Agent) announce(msg string) {
	if a.Notice(msg) {
		return
	}
	fmt.Fprintf(os.Stderr, "warn: %s\n", msg)
	a.QueueNotice(msg)
}

// harnessReplyTokens is the max_tokens for one of the harness's own requests:
// limit, or the configured max_tokens when smaller, and never more than the
// window leaves beside prompt.
func (a *Agent) harnessReplyTokens(limit int, prompt []provider.Message) int {
	n := limit
	if m := a.Cfg.MaxTokens; m > 0 && m < n {
		n = m
	}
	budget, _, cpt := a.History.Scalars()
	window := a.Window()
	if window <= 0 {
		window = budget
	}
	if cpt <= 0 {
		cpt = defaultCharsPerToken
	}
	used := 0
	for _, m := range prompt {
		used += int(float64(len(m.Content))/cpt) + 4
	}
	if room := window - used; room < n {
		n = room
	}
	if n < minReplyTokens {
		n = minReplyTokens
	}
	return n
}
