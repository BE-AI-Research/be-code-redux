package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brown-enterprises/be-code/internal/provider"
)

// Backend resilience: a local inference server is shared and fallible.
// Another client can evict our model, reload it with a different window,
// or make it queue for a minute; the server can restart. None of that
// should cost the user their task. The tool-loop state lives in History,
// so a failed call is simply retried in place; evictions and window
// changes are detected before each call; silence is reported instead of
// looking like a hang; and the model's residency is refreshed after each
// request so idle expiry does not force a reload plus full prompt
// re-processing.

// maxBackendRetries bounds in-place retries of one model call.
const maxBackendRetries = 5

// chatWithRetry runs chatFiltered, retrying transient backend failures with
// exponential backoff (retryBase, doubling, capped at 30s).
func (a *Agent) chatWithRetry(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= maxBackendRetries; attempt++ {
		resp, err := a.chatFiltered(ctx, req)
		a.noteNativeFallback()
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if ctx.Err() != nil || !isRetryableBackendError(err) || attempt == maxBackendRetries {
			break
		}
		delay := a.retryBase << uint(attempt)
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}
		a.transient("backend error (%v); retrying in %s (attempt %d/%d, task state preserved)",
			compactErr(err), delay.Round(time.Millisecond), attempt+1, maxBackendRetries)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if isRetryableBackendError(lastErr) && ctx.Err() == nil {
		return nil, fmt.Errorf("backend unavailable after %d retries: %w", maxBackendRetries, lastErr)
	}
	return nil, lastErr
}

// isRetryableBackendError distinguishes "the server is busy/restarting/
// swapping models" from "this request is wrong".
func isRetryableBackendError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	if strings.Contains(s, "context canceled") {
		return false
	}
	for _, k := range []string{
		"connection refused", "connection reset", "broken pipe", "eof",
		"no such host", "timeout", "timed out", "http 5", "http 429",
		"loading", "unavailable", "busy", "try again", "overloaded",
	} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func compactErr(err error) string {
	s := err.Error()
	if i := strings.Index(s, "\n"); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}

// noteNativeFallback tells the user, once, when a backend has given up its
// native endpoint for an OpenAI-compatible one. It is a quieter session,
// not a broken one — but it is also a session that can no longer set the
// model's context window, so it must not look identical to a healthy one.
func (a *Agent) noteNativeFallback() {
	nf, ok := a.Provider.(provider.NativeFallbacker)
	if !ok || a.nativeFallbackNotified || !nf.NativeFallback() {
		return
	}
	a.nativeFallbackNotified = true
	a.notice("backend does not serve its native chat endpoint; using the OpenAI-compatible path for this session (the context window cannot be set from here)")
}

// checkBackend asks a status-capable provider whether the model is still
// resident and with which window, then adapts the budget and tells the
// user what happened.
func (a *Agent) checkBackend(ctx context.Context) {
	bs, ok := a.Provider.(provider.BackendStatus)
	if !ok {
		return
	}
	sctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	window, loaded, err := bs.Status(sctx, a.Model)
	if err != nil {
		return // unreachable backend: the chat call will report it
	}
	// Both arms below hand the trip to the loader, because the loader is
	// the only place a model's parameters are decided (spec §10.1). What
	// it does with each is different, and the difference is who else is
	// affected:
	//
	//   evicted — nothing is holding the model, so the next request
	//   reloads it whatever we do. The reload may as well carry our
	//   num_ctx, and nobody has to be asked for it.
	//
	//   window changed — another client reloaded the model at their size.
	//   We adapt. Reloading it back would be a reload war on a shared
	//   server, which is the worst outcome available, so the loader
	//   insists only where standing consent already exists.
	l := a.modelLoader()
	if !loaded {
		if !a.unloadedNotified {
			a.unloadedNotified = true
			a.transient("model %s is not loaded on the backend (evicted by another model or idle expiry); the next reply includes reload and prompt re-processing time", a.Model)
		}
		if l != nil {
			l.OnEvicted(ctx, a.Model)
		}
	} else {
		a.unloadedNotified = false
	}
	if window > 0 && window != a.Window() {
		old := a.Window()
		a.ApplyWindow(window)
		budget, _, _ := a.History.Scalars()
		a.transient("backend context window changed %d → %d; budget now %d tokens (limit %d)", old, window, budget, a.History.Limit())
		if l != nil {
			l.OnWindowChanged(a.Model, window)
		}
	}
}

// refreshKeepAlive extends the model's residency after a request (async;
// a no-op for providers without keep-alive or when keep_alive is "0").
func (a *Agent) refreshKeepAlive() {
	ka, ok := a.Provider.(provider.KeepAliver)
	if !ok {
		return
	}
	model := a.Model
	// The loader resolves keep_alive per model (models entry, then the
	// provider block, then the top-level setting), and it is the same
	// number the requests themselves carry. Reading cfg.KeepAlive here
	// instead would refresh residency on a schedule a per-model setting had
	// already overridden.
	var d time.Duration
	if l := a.modelLoader(); l != nil {
		d = l.KeepAlive(model)
	}
	if d <= 0 {
		var err error
		d, err = time.ParseDuration(strings.TrimSpace(a.Cfg.KeepAlive))
		if a.Cfg.KeepAlive == "" {
			d = 30 * time.Minute
		} else if err != nil || d <= 0 {
			return // "0" means do not keep it resident; nothing to refresh
		}
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		_ = ka.KeepAlive(ctx, model, d)
	}()
}

// watchForStall wraps the stream callbacks so a silent backend (model
// reloading, request queued behind another client) is reported after
// stallAfter and again at 3x, instead of looking like a hang.
func (a *Agent) watchForStall(onDelta, onReasoning provider.StreamFunc) (provider.StreamFunc, provider.StreamFunc, func()) {
	if a.stallAfter <= 0 {
		return onDelta, onReasoning, func() {}
	}
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	touch := func() { last.Store(time.Now().UnixNano()) }
	wrap := func(f provider.StreamFunc) provider.StreamFunc {
		return func(s string) {
			touch()
			if f != nil {
				f(s)
			}
		}
	}
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		tick := time.NewTicker(a.stallAfter / 4)
		defer tick.Stop()
		stage := 0
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				idle := time.Since(time.Unix(0, last.Load()))
				switch {
				case stage == 0 && idle >= a.stallAfter:
					stage = 1
					a.transient("waiting for backend: no tokens for %s (prompt re-processing after a context change, model loading, or queued behind another client)", idle.Round(time.Second))
				case stage == 1 && idle >= stallSecondStage(a.stallAfter):
					stage = 2
					a.transient("still waiting for backend after %s; Ctrl-C/Esc cancels, the task state is saved", idle.Round(time.Second))
				}
			}
		}
	}()
	stop := func() { close(done); wg.Wait() }
	// Always wrap so the watcher sees activity even when the UI has no
	// delta consumer (e.g. headless --json).
	return wrap(onDelta), wrap(onReasoning), stop
}
