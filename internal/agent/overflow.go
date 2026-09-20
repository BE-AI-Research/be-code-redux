package agent

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// A server that refuses a prompt larger than the model's loaded window. Older
// Ollama truncated such a prompt silently; 0.34 answers HTTP 400 with
// llama.cpp's exceed_context_size_error, and an OpenAI-compatible server says
// context_length_exceeded. Left alone it is the worst kind of failure: every
// retry sends the same prompt into the same window, so the session is dead
// until somebody unloads the model by hand, and what it prints is the server's
// escaped JSON.

var (
	overflowNCtx   = regexp.MustCompile(`n_ctx\\*"\s*:\s*(\d+)`)
	overflowPrompt = regexp.MustCompile(`n_prompt_tokens\\*"\s*:\s*(\d+)`)
)

// contextOverflow reports whether err is that refusal, with the prompt size
// and the window the server named (0 where it named none).
func contextOverflow(err error) (promptTokens, window int, ok bool) {
	if err == nil {
		return 0, 0, false
	}
	s := err.Error()
	low := strings.ToLower(s)
	switch {
	case strings.Contains(low, "exceed_context_size_error"),
		strings.Contains(low, "exceeds the available context size"),
		strings.Contains(low, "context_length_exceeded"),
		strings.Contains(low, "maximum context length"):
	default:
		return 0, 0, false
	}
	if m := overflowPrompt.FindStringSubmatch(s); m != nil {
		promptTokens, _ = strconv.Atoi(m[1])
	}
	if m := overflowNCtx.FindStringSubmatch(s); m != nil {
		window, _ = strconv.Atoi(m[1])
	}
	return promptTokens, window, true
}

// recoverFromOverflow is the one attempt a run makes after such a refusal,
// and reports whether the request is worth sending again.
//
// The server has just said what window the model really has, which makes it
// better informed than we are. So the loader is asked again first: it may hold
// a consent that never reached the wire, or find the model free to load at the
// configured size, and either way its answer goes on the wire for the retry.
// Only when the window cannot grow is the prompt made smaller instead.
func (a *Agent) recoverFromOverflow(ctx context.Context, promptTokens, serverWindow int) bool {
	held := a.Window()
	if serverWindow <= 0 {
		serverWindow = held
	}
	w := 0
	if l := a.modelLoader(); l != nil {
		w, _ = l.Apply(ctx, a.Model)
	}
	if ctx.Err() != nil {
		return false
	}
	if w > 0 {
		a.ApplyResolvedWindow(w)
	} else if serverWindow > 0 {
		a.ApplyWindow(serverWindow)
		w = serverWindow
	}
	if w > serverWindow {
		a.notice("the backend refused a %d-token prompt: %s is loaded with a %d-token window. Retrying at %d, which reloads it",
			promptTokens, a.Model, serverWindow, w)
		return true
	}
	// The window stays. The server's count is ground truth for the prompt it
	// refused, so the estimate learns from it before anything is measured.
	if promptTokens > a.History.Extra {
		a.History.Calibrate(promptTokens - a.History.Extra)
	}
	before := a.History.Tokens()
	a.maybeCompact(ctx)
	after := a.History.Tokens()
	if after < before && (w <= 0 || after < w) {
		a.notice("the backend refused a %d-token prompt: %s is loaded with a %d-token window. Compacted to about %d tokens; retrying",
			promptTokens, a.Model, serverWindow, after)
		return true
	}
	return false
}

// overflowError is what the user reads when nothing could be done. It says
// what does not fit and what would fix it, and never the server's raw reply.
func (a *Agent) overflowError(promptTokens, serverWindow int) error {
	if serverWindow <= 0 {
		serverWindow = a.Window()
	}
	if promptTokens <= 0 {
		promptTokens = a.History.Tokens()
	}
	return fmt.Errorf("the request (%d tokens) does not fit the %d-token window %s is loaded with; about %d of those tokens are the system prompt and tool schemas, which compaction cannot remove. "+
		"To fix it: unload the model on the server so it reloads at the configured window (`ollama stop %s`), "+
		"or set reload_on_mismatch to \"always\" if that server is yours to reshape, or /clear to drop this conversation",
		promptTokens, serverWindow, a.Model, a.History.Floor(), a.Model)
}
