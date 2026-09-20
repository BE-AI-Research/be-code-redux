package agent

import (
	"context"
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
		resp, err := pf.Prefill(ctx, req)
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
	return a.requestFor("", false), true // no tail: it rides on a message nobody has typed yet
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

// requestFor builds the chat request from the history as it stands. withTail
// adds the volatile block to the copy that is sent — never to the history.
func (a *Agent) requestFor(effort string, withTail bool) provider.ChatRequest {
	msgs := a.History.Prompt()
	tail := ""
	if withTail {
		tail = a.volatileTail()
	}
	if tail != "" && len(msgs) > 0 {
		msgs = append([]provider.Message(nil), msgs...)
		if last := &msgs[len(msgs)-1]; last.Role == provider.RoleUser || last.Role == provider.RoleTool {
			last.Content = strings.TrimRight(last.Content, "\n") + tail
		} else {
			msgs = append(msgs, provider.Message{Role: provider.RoleUser, Content: strings.TrimLeft(tail, "\n")})
		}
	}
	req := provider.ChatRequest{
		Model:           a.Model,
		Messages:        msgs,
		Temperature:     a.temperature(),
		MaxTokens:       a.Cfg.MaxTokens,
		ReasoningEffort: a.effortFor(effort),
	}
	// Extra is everything sent that the history does not hold: the tools
	// schema and the tail. Both are part of the floor no compaction removes.
	a.History.Extra = 0
	if tail != "" {
		a.History.Extra = a.History.est(tail)
	}
	if !a.compat {
		req.Tools = a.Tools.Specs()
		a.History.Extra += a.specsTokens()
	}
	return req
}

// The prompt layout (config prompt_layout: "cached", the default, or
// "classic").
//
// A server's prompt cache is a prefix cache: it survives from one request to
// the next exactly as far as the two prompts are identical. Working memory
// used to sit in the middle of the system prompt and changes after nearly
// every tool call, so every request differed from the last one a few thousand
// tokens in, and the server re-read the entire conversation behind it each
// turn — measured on a real five-read task at 225 of 296 seconds, nine cache
// misses in nine requests.
//
// In the cached layout the system prompt holds only what is stable for the
// length of a request, and what changes — the git summary and Working memory
// — is appended to the last message of the copy that is sent. The history
// itself never carries it, so last turn's copy of that message is what the
// server cached, and the next request re-reads only what is new.

func (a *Agent) cachedLayout() bool {
	return a.Cfg == nil || !strings.EqualFold(strings.TrimSpace(a.Cfg.PromptLayout), "classic")
}

// tailHeader separates the volatile block from the message it rides on.
const tailHeader = "\n\n---\nHarness state, refreshed on every request (not part of the message above):\n\n"

// volatileTail is the git summary and the Working memory block, or "" in the
// classic layout, where composeSystem carries them.
func (a *Agent) volatileTail() string {
	if !a.cachedLayout() {
		return ""
	}
	if a.systemOverride != "" {
		return "" // a scratch agent: no Working memory, and git stays in its system prompt
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
