package agent

import (
	"context"
	"hash/fnv"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/brown-enterprises/be-code/internal/gitctx"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// Background prompt processing (config prompt_prefill, on by default).
//
// A server that has just loaded or reloaded a model has an empty prompt
// cache, and so has one meeting a resumed session for the first time: the
// next request reads the whole prompt before its first token — measured at
// 21s for 9.7k tokens and 43s for 19k on the owner's box. The harness knows
// what that prompt will be before the user has finished typing, so it sends
// it ahead with generation turned off (provider.Prefiller). The real request
// then pays for its own new text only (0.3s in the same probe).
//
// One at a time, never beside a real request: run cancels it before taking
// the turn lock, and a cancelled prefill keeps whatever the server had read.

// prefillTimeout bounds one prefill. A prompt that takes longer than this to
// read is not one to be reading speculatively on a shared server.
const prefillTimeout = 3 * time.Minute

type prefillState struct {
	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// StartPrefill warms the server's prompt cache with the prompt the next turn
// will send. It returns at once; it does nothing when the provider cannot
// prefill, when it is turned off, or when a request is running (that request
// is warming the cache itself).
func (a *Agent) StartPrefill() {
	if a.Cfg == nil || !a.Cfg.PromptPrefill {
		return
	}
	pf, ok := a.Provider.(provider.Prefiller)
	if !ok {
		return
	}
	a.stopPrefill()
	if !a.turnMu.TryLock() {
		return
	}
	req, ok := a.nextPromptLocked()
	a.turnMu.Unlock()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), prefillTimeout)
	done := make(chan struct{})
	a.prefill.mu.Lock()
	a.prefill.cancel, a.prefill.done = cancel, done
	a.prefill.mu.Unlock()
	go func() {
		defer close(done)
		defer cancel()
		defer func() { _ = recover() }() // advisory work: never the session's problem
		start := time.Now()
		a.transient("warming the server's prompt cache (about %d tokens)", a.History.Tokens())
		// In the lane like every other request to this server. The comment
		// above says a real request never shares the server with a prefill,
		// and stopPrefill keeps that true — but a *sub-agent* on the same
		// server is not a real request of ours, and interleaving with one
		// makes the warming pointless (the cache it warmed is the cache the
		// sub-agent's own prompt then evicts). At background priority: this
		// one blocks nobody, so it must not go ahead of real work.
		resp, err := inLaneWith(ctx, a.prefillLane(), func() (*provider.ChatResponse, error) { return pf.Prefill(ctx, req) })
		if err != nil || resp == nil {
			return // cancelled by a real request, or a server that would not: neither is news
		}
		a.transient("prompt cache warm: %d tokens read in %s", resp.Usage.PromptTokens, time.Since(start).Round(time.Second))
	}()
}

// stopPrefill cancels a running prefill and waits for it to let go of the
// connection, so a real request never shares the server with it.
func (a *Agent) stopPrefill() {
	a.prefill.mu.Lock()
	cancel, done := a.prefill.cancel, a.prefill.done
	a.prefill.cancel, a.prefill.done = nil, nil
	a.prefill.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// nextPromptLocked composes the request the next turn will send, minus the
// user message nobody has typed yet. The caller holds turnMu. It goes through
// the same steps run does, in the same order, because a prefill that differs
// from the real prompt anywhere before its end warms nothing past that point.
func (a *Agent) nextPromptLocked() (provider.ChatRequest, bool) {
	if a.History == nil {
		return provider.ChatRequest{}, false
	}
	a.refreshSystemForRequest(context.Background())
	// A model switch rewrites the profile and the system prompt under
	// modelMu, from a UI goroutine, whenever the user likes.
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	// The configured level, through the same path a real request takes: the
	// level is part of the prompt, and a prefill at another one warms nothing.
	return a.requestFor(a.Cfg.ReasoningEffort, false), true
}

// recomposeSystem rebuilds the system prompt under modelMu: a model switch
// writes the same field from another goroutine (applySwitch), and this one is
// now reached from the prefill's goroutine as well as the run's.
func (a *Agent) recomposeSystem(gitInfo string) {
	a.modelMu.Lock()
	a.History.System.Content = a.composeSystem(gitInfo)
	a.modelMu.Unlock()
}

// refreshSystemForRequest is the part of a request's preamble that decides
// the system prompt: the repository map if files changed, fitted to the
// window, and the git summary.
func (a *Agent) refreshSystemForRequest(ctx context.Context) {
	if a.repoDirty {
		a.repoDirty = false
		a.RefreshRepoMap()
		a.recomposeSystem("")
	}
	// The window may have been resolved, or changed, since the map was built.
	a.fitRepoMap()
	a.warnHeavyPrompt()
	gctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if gi := gitctx.Summary(gctx, a.Tools.Root); gi != "" {
		a.lastGitInfo = gi
		a.recomposeSystem(gi)
	}
}

// requestFor builds the chat request from the history as it stands. live is
// a request that is really about to be sent: only then is the harness state
// refreshed where the history has been rewritten, and only then is what went
// out remembered for the next comparison. A prefill is not live — it sends the
// history exactly as it is, which is exactly what the next request extends.
func (a *Agent) requestFor(effort string, live bool) provider.ChatRequest {
	msgs := a.History.Prompt() // may trim, which is a rewrite like any other
	if live && a.cachedLayout() {
		if a.rewrittenSinceSent() {
			a.attachState()
			a.effortLowered = false // a rewritten prompt is read afresh anyway: the level may start over
			msgs = a.History.Prompt()
		}
		a.markSent()
	}
	req := provider.ChatRequest{
		Model:           a.Model,
		Messages:        msgs,
		Temperature:     a.temperature(),
		MaxTokens:       a.Cfg.MaxTokens,
		ReasoningEffort: a.steadyEffort(effort, live),
	}
	a.History.Extra = 0
	if !a.compat {
		req.Tools = a.Tools.Specs()
		a.History.Extra = a.specsTokens()
	}
	return req
}

// steadyEffort is effortFor with a memory. The reasoning level is rendered
// into the front of the prompt — measured: the same 13k-token conversation
// read in 0.3s at the level it was cached with and 19s at another, and 29s
// with no level at all — so every change of level costs a full read of the
// conversation. effortFor is stateless and steps down above Target; a prompt
// hovering at that line would flip the level, and pay for it, turn after
// turn. Here a level that has stepped down stays down until the history is
// rewritten, which is when the prompt is read afresh regardless. One change
// per compaction cycle, which the shorter reasoning pays back within a few
// turns. A prefill (live false) reads the memory and never writes it, so it
// warms the prompt the next request will really send.
func (a *Agent) steadyEffort(configured string, live bool) string {
	want := a.effortFor(configured)
	if !a.cachedLayout() {
		return want
	}
	if want != configured && live {
		a.effortLowered = true
	}
	if a.effortLowered {
		return lowerEffort(configured)
	}
	return want
}

// lowerEffort is one level down, as effortFor steps it.
func lowerEffort(configured string) string {
	switch configured {
	case "high":
		return "medium"
	case "medium":
		return "low"
	}
	return configured
}

// The prompt layout (config prompt_layout: "cached", the default, or
// "classic").
//
// The rule the cached layout keeps is stronger than "put what changes last":
// **what has been sent is never changed or taken back.** Measured on the
// owner's server (Ollama 0.34, a Qwen3.8 hybrid model): a request that strictly
// extends the previous one costs only its new text — five reads of one
// conversation took about 6s each, flat, whatever its length. The same five
// reads with a Working memory block that was appended to the last message and
// *removed again* on the next turn took 11s, 18s, 25s, 41s and 39s: removing
// anything sends the server back to an older checkpoint of the model's state,
// and every checkpoint it had also ended in a block that was no longer there.
// Working memory in the system prompt (the classic layout, and every version
// before 0.14) is the same failure from the front: nine cache misses in nine
// requests.
//
// So the state is **attached and kept**. The git summary and the Working
// memory block go on the end of the user's message when a request begins, as
// part of the stored history, and stay there. In between, the conversation is
// its own record — the model's task calls and tool results are all in it, and
// what the harness has to say mid-run (the time footer, a repeat, a step open
// too long) goes on the end of the tool result it is about. A fresh snapshot
// is attached only when the history has just been rewritten anyway — a
// compaction, a collapse, a trim — which is both when the server's cache is
// cold regardless and when Working memory holds what the conversation no
// longer does.

func (a *Agent) cachedLayout() bool {
	return a.Cfg == nil || !strings.EqualFold(strings.TrimSpace(a.Cfg.PromptLayout), "classic")
}

// tailHeader separates the state from the message it is attached to.
const tailHeader = "\n\n---\nHarness state at this point (not part of the message above):\n\n"

// StripHarnessState is a stored message's own text, without an attached
// state. Whatever reads the history back as the user's or a tool's words — a
// replay, a compaction, a handoff, a consultation — reads it through this.
func StripHarnessState(content string) string {
	if i := strings.Index(content, tailHeader); i >= 0 {
		return content[:i]
	}
	return content
}

// volatileTail is the git summary and the Working memory block, or "" in the
// classic layout (composeSystem carries them) and for a scratch agent.
func (a *Agent) volatileTail() string {
	if !a.cachedLayout() || a.systemOverride != "" {
		return ""
	}
	var parts []string
	if a.lastGitInfo != "" {
		parts = append(parts, a.lastGitInfo)
	}
	if wm := a.workingMemory(); wm != "" {
		parts = append(parts, "Working memory:\n"+wm)
	}
	if len(parts) == 0 {
		return ""
	}
	return tailHeader + strings.Join(parts, "\n\n")
}

// attachState puts the current state on the newest user or tool message, as
// part of the history. The caller holds turnMu and calls it only where that
// message has not been sent yet (a new request's own message) or where the
// history has just been rewritten, so nothing the server has cached is
// disturbed. One snapshot to a message: a second one replaces the first.
func (a *Agent) attachState() {
	tail := a.volatileTail()
	if tail == "" {
		return
	}
	msgs := a.History.Messages
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != provider.RoleUser && msgs[i].Role != provider.RoleTool {
			continue
		}
		msgs[i].Content = strings.TrimRight(StripHarnessState(msgs[i].Content), "\n") + tail
		return
	}
}

// markSent remembers the conversation as it is about to go out, one hash a
// message; rewrittenSinceSent reports whether the conversation still begins
// with it. Anything that edits or removes a message that was sent — a
// collapse, a compaction, a trim, a repaired orphan — makes it true, without
// each of them having to say so.
func (a *Agent) markSent() {
	fp := make([]uint64, len(a.History.Messages))
	for i, m := range a.History.Messages {
		fp[i] = messageHash(m)
	}
	a.sentPrint = fp
}

func (a *Agent) rewrittenSinceSent() bool {
	if len(a.sentPrint) == 0 {
		return false
	}
	msgs := a.History.Messages
	if len(msgs) < len(a.sentPrint) {
		return true
	}
	for i, want := range a.sentPrint {
		if messageHash(msgs[i]) != want {
			return true
		}
	}
	return false
}

func messageHash(m provider.Message) uint64 {
	h := fnv.New64a()
	io.WriteString(h, string(m.Role))
	io.WriteString(h, "\x00"+m.Content+"\x00"+m.ToolCallID)
	for _, tc := range m.ToolCalls {
		io.WriteString(h, "\x00"+tc.ID+"\x00"+tc.Name+"\x00"+tc.Arguments)
	}
	return h.Sum64()
}
