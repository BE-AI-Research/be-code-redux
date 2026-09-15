package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/brown-enterprises/be-code/internal/provider"
)

// History manages the conversation under a token budget. Local models have
// small effective contexts (often 8k-32k), and quality degrades well before
// the hard limit, so BE-Code trims aggressively: old tool results are
// collapsed first, then whole old turns are dropped behind a stub.
type History struct {
	// mu guards the three scalars below — Budget, Reserve and
	// CharsPerToken — and nothing else. They are written by the agent
	// goroutine (Calibrate after every request, Agent.ApplyWindow when the
	// backend's window moves) and read by whatever goroutine starts a
	// consultation: /consult comes off a UI goroutine, and consultAgent
	// copies all three into the scratch history. Readers on the agent's own
	// goroutine (est, Tokens, Limit, trim) stay lock-free: they cannot race
	// with a writer that is themselves, and a lock in est would be taken
	// once per message per turn for nothing. Scalars is the one seam a
	// foreign goroutine reads them through.
	mu sync.Mutex

	System   provider.Message
	Messages []provider.Message
	Budget   int // total prompt tokens the backend can take (its context window)
	Extra    int // per-request overhead not in Messages: the tools schema
	Reserve  int // generation headroom kept free below Budget

	// CharsPerToken is the estimation ratio. It starts at a conservative
	// default and is recalibrated from server-reported prompt_tokens, since
	// code and tool output tokenize far denser than prose.
	CharsPerToken float64
	calibrated    bool

	// TargetFraction is how far below Limit compression aims (hysteresis):
	// trigger at Limit, compress to Limit*TargetFraction, so a run gets real
	// runway between compactions instead of one every couple of tool calls.
	TargetFraction float64
}

// Token estimation: chars/CharsPerToken. Real tokenizers land anywhere from
// ~2 chars/token (paths, symbol dumps) to ~4.5 (English prose), so the
// default leans conservative and Calibrate corrects it from live usage.
const (
	defaultCharsPerToken = 3.0
	minCharsPerToken     = 1.5
	maxCharsPerToken     = 5.0
)

// estimateTokens is the uncalibrated estimate used where no History is at hand.
func estimateTokens(s string) int { return int(float64(len(s))/defaultCharsPerToken) + 1 }

func (h *History) est(s string) int {
	r := h.CharsPerToken
	if r <= 0 {
		r = defaultCharsPerToken
	}
	return int(float64(len(s))/r) + 1
}

// MessageTokens estimates one message with the history's calibrated ratio.
func (h *History) MessageTokens(m provider.Message) int {
	n := h.est(m.Content) + 4
	for _, tc := range m.ToolCalls {
		n += h.est(tc.Name) + h.est(tc.Arguments)
	}
	return n
}

// messageTokens is the uncalibrated package-level estimate.
func messageTokens(m provider.Message) int {
	return (&History{}).MessageTokens(m)
}

// NewHistory creates a history with the given system prompt and budget.
func NewHistory(system string, budget int) *History {
	if budget <= 0 {
		budget = 16384
	}
	return &History{
		System:         provider.Message{Role: provider.RoleSystem, Content: system},
		Budget:         budget,
		CharsPerToken:  defaultCharsPerToken,
		TargetFraction: 0.5,
	}
}

// Target is the token count compression aims for once triggered.
func (h *History) Target() int {
	f := h.TargetFraction
	if f <= 0 || f >= 1 {
		f = 0.5
	}
	return int(float64(h.Limit()) * f)
}

// Limit is the budget available to System+Messages+Extra after reserving
// generation headroom.
func (h *History) Limit() int {
	l := h.Budget - h.Reserve
	if l < 512 {
		l = 512 // never trim into nothing; the model must see something
	}
	return l
}

// Over reports whether the next prompt would exceed the usable limit.
func (h *History) Over() bool { return h.Tokens() > h.Limit() }

// Calibrate adjusts CharsPerToken so the estimate of the last prompt matches
// the token count the server reported for it. Smoothed and clamped so one
// odd report cannot swing the budget.
func (h *History) Calibrate(reportedPromptTokens int) {
	if reportedPromptTokens <= 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	chars := 0
	for _, m := range append([]provider.Message{h.System}, h.Messages...) {
		chars += len(m.Content)
		for _, tc := range m.ToolCalls {
			chars += len(tc.Name) + len(tc.Arguments)
		}
	}
	if chars < 256 {
		return
	}
	observed := float64(chars) / float64(reportedPromptTokens)
	cur := h.CharsPerToken
	if cur <= 0 {
		cur = defaultCharsPerToken
	}
	// First report is authoritative (the default was a guess); later ones
	// are smoothed.
	next := observed
	if h.calibrated {
		next = cur*0.5 + observed*0.5
	}
	h.calibrated = true
	if next < minCharsPerToken {
		next = minCharsPerToken
	}
	if next > maxCharsPerToken {
		next = maxCharsPerToken
	}
	h.CharsPerToken = next
}

// Scalars is Budget, Reserve and CharsPerToken read together under mu, for
// a goroutine that is not the agent's own (consultAgent, building a
// co-worker's scratch history while the primary may be recalibrating).
func (h *History) Scalars() (budget, reserve int, charsPerToken float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.Budget, h.Reserve, h.CharsPerToken
}

// Add appends a message.
func (h *History) Add(m provider.Message) { h.Messages = append(h.Messages, m) }

// Tokens returns the estimated size of the full prompt.
func (h *History) Tokens() int {
	n := h.MessageTokens(h.System) + h.Extra
	for _, m := range h.Messages {
		n += h.MessageTokens(m)
	}
	return n
}

const collapsedStub = "[old tool result removed to save context]"

// Prompt returns the message list to send, trimming to budget if needed.
func (h *History) Prompt() []provider.Message {
	h.trim()
	out := make([]provider.Message, 0, len(h.Messages)+1)
	out = append(out, h.System)
	out = append(out, h.Messages...)
	return out
}

// trim reduces history size in two passes. Pass 1: collapse tool-result
// bodies outside the most recent 8 messages. Pass 2: drop oldest turns
// entirely (keeping the first user message, which carries the task).
func (h *History) trim() {
	if !h.Over() {
		return
	}
	if h.CollapseOldToolResults() {
		return
	}
	// Pass 2: drop oldest messages after the first user message, keeping
	// tool-call/result pairing intact by dropping from the front.
	for h.Over() && len(h.Messages) > 6 {
		firstUser := -1
		for i, m := range h.Messages {
			if m.Role == provider.RoleUser {
				firstUser = i
				break
			}
		}
		dropIdx := firstUser + 1
		if dropIdx >= len(h.Messages)-4 {
			break // nothing safe left to drop
		}
		h.Messages = append(h.Messages[:dropIdx], h.Messages[dropIdx+1:]...)
		// Insert a one-time marker so the model knows history is elided.
		if dropIdx < len(h.Messages) {
			m := &h.Messages[dropIdx]
			if isToolResult(*m) && m.Content != collapsedStub {
				m.Content = collapsedStub
			}
		}
	}
	h.RepairOrphans()
}

// CollapseOldToolResults compresses old tool traffic until the history is
// at or below Target: first tool results older than the last 8 messages,
// then all but the most recent one, shrinking big tool-call arguments
// (file bodies passed to write_file/edit_file) along the way. Reports
// whether the target was reached. Cheap and lossless for the conversation
// itself, so it always runs before model-written compaction.
func (h *History) CollapseOldToolResults() bool {
	for _, keep := range []int{8, 1} {
		keepFrom := len(h.Messages) - keep
		for i := range h.Messages {
			if i >= keepFrom {
				break
			}
			m := &h.Messages[i]
			if isToolResult(*m) && len(m.Content) > len(collapsedStub) {
				m.Content = collapsedStub
			}
			shrinkToolCallArgs(m)
			if h.Tokens() <= h.Target() {
				return true
			}
		}
	}
	return h.Tokens() <= h.Target()
}

// CollapseToolResults stubs the bodies of all tool results except the
// newest keepLast, and shrinks the arguments of every tool call older than
// the newest exchange, regardless of budget. Returns how many results were
// collapsed.
func (h *History) CollapseToolResults(keepLast int) int {
	n := 0
	seen := 0
	lastCall := -1
	for i := len(h.Messages) - 1; i >= 0; i-- {
		if h.Messages[i].Role == provider.RoleAssistant && len(h.Messages[i].ToolCalls) > 0 {
			lastCall = i
			break
		}
	}
	for i := len(h.Messages) - 1; i >= 0; i-- {
		m := &h.Messages[i]
		if m.Role == provider.RoleAssistant && i != lastCall {
			shrinkToolCallArgs(m)
		}
		if !isToolResult(*m) {
			continue
		}
		seen++
		if seen <= keepLast {
			continue
		}
		if len(m.Content) > len(collapsedStub) {
			m.Content = collapsedStub
			n++
		}
	}
	return n
}

// shrinkArgsThreshold is the argument size above which bodies are omitted.
const shrinkArgsThreshold = 512

// shrinkToolCallArgs replaces large string fields in a tool call's
// arguments (file contents, edit bodies) with a short marker, keeping small
// fields such as the path so the transcript still says what was touched.
func shrinkToolCallArgs(m *provider.Message) {
	for i := range m.ToolCalls {
		tc := &m.ToolCalls[i]
		if len(tc.Arguments) <= shrinkArgsThreshold {
			continue
		}
		var args map[string]any
		if json.Unmarshal([]byte(tc.Arguments), &args) != nil {
			tc.Arguments = fmt.Sprintf("{\"_omitted\":\"%d bytes of arguments removed to save context\"}", len(tc.Arguments))
			continue
		}
		changed := false
		for k, v := range args {
			if str, ok := v.(string); ok && len(str) > 200 {
				args[k] = fmt.Sprintf("[omitted %d bytes to save context; re-read the file if needed]", len(str))
				changed = true
			}
		}
		if changed {
			if b, err := json.Marshal(args); err == nil {
				tc.Arguments = string(b)
			}
		}
	}
}

// isToolResult matches both native tool messages and compat-mode results
// (user messages carrying <tool_result> blocks).
func isToolResult(m provider.Message) bool {
	if m.Role == provider.RoleTool {
		return true
	}
	return m.Role == provider.RoleUser && strings.HasPrefix(m.Content, "<tool_result")
}

// RepairOrphans converts tool results whose originating call was dropped
// into user-visible notes — providers reject unpaired tool messages.
func (h *History) RepairOrphans() {
	for i := range h.Messages {
		if h.Messages[i].Role != provider.RoleTool {
			continue
		}
		if !h.hasMatchingCall(i) {
			h.Messages[i] = provider.Message{
				Role:    provider.RoleUser,
				Content: fmt.Sprintf("[context note: earlier tool result elided: %.120s]", h.Messages[i].Content),
			}
		}
	}
}

func (h *History) hasMatchingCall(toolIdx int) bool {
	id := h.Messages[toolIdx].ToolCallID
	for i := toolIdx - 1; i >= 0; i-- {
		m := h.Messages[i]
		if m.Role == provider.RoleAssistant {
			for _, tc := range m.ToolCalls {
				if tc.ID == id {
					return true
				}
			}
		}
	}
	return false
}
