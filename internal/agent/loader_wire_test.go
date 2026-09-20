package agent

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/loader"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// S2. R1 handed the wire back to the current model when a superseded
// resolution returned — but only one that returned a window. One whose
// /api/ps read failed came back as (0, nil) and left through the early
// return above the stale check, after the loader had already rewritten the
// endpoint's options for *its* model: no window, and its own passthrough
// map. So a slow, failing /model big behind a fast /model small left small
// with no num_ctx at all and big's top_k on every request.
//
// This one runs the real loader over a real Ollama provider (against
// httptest, never a network): what is being pinned is what the two of them
// leave on the wire between them, and a fake loader would only pin this
// file's reading of the real one.
func TestASupersededFailingResolutionHandsTheWireBack(t *testing.T) {
	slow := make(chan struct{})
	var release sync.Once
	unpark := func() { release.Do(func() { close(slow) }) }
	var psCalls, failed int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/ps" {
			http.NotFound(w, r)
			return
		}
		// The first residency read is big's: it parks, and then fails.
		// Every later one answers at once — nothing is resident, so a
		// configured window needs nobody's consent.
		if atomic.AddInt32(&psCalls, 1) == 1 {
			<-slow
			http.Error(w, "bad gateway", http.StatusBadGateway)
			atomic.StoreInt32(&failed, 1)
			return
		}
		w.Write([]byte(`{"models":[]}`))
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(unpark) // registered second, so it runs first: Close waits on the parked handler

	o := provider.NewOllama("t", srv.URL, "")
	ag, _ := newTestAgent(t, o, func(c *config.Config) {
		c.ContextTokens = 0
		c.Models = map[string]config.ModelConfig{
			"big":   {ContextWindow: 65536, Options: map[string]any{"top_k": 7}},
			"small": {ContextWindow: 8192, Options: map[string]any{"top_k": 40}},
		}
	})
	ag.SetLoader(loader.New(o, ag.Cfg, nil, func(string) {}))

	ag.SetModel("big") // parks in its residency read
	waitFor(t, "the slow resolution to reach the server", func() bool { return atomic.LoadInt32(&psCalls) == 1 })
	ag.SetModel("small") // overtakes it and resolves at once
	waitFor(t, "the fast resolution to land", func() bool { return o.Options().NumCtx == 8192 })

	unpark() // big's read finally fails, long after it stopped being the model
	waitFor(t, "the superseded resolution's read to fail", func() bool { return atomic.LoadInt32(&failed) == 1 })

	// The loader rewrites the options for big on its way out, and the agent
	// then has to put small's back: give that a moment, then hold it to it.
	settled := func() bool {
		got := o.Options()
		return got.NumCtx == 8192 && fmt.Sprint(got.Extra["top_k"]) == "40"
	}
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline) && !settled(); {
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	got := o.Options()
	if got.NumCtx != 8192 {
		t.Errorf("num_ctx %d on the wire for model %q, want 8192; a superseded resolution that failed took the current model's window away", got.NumCtx, ag.Model)
	}
	if k := fmt.Sprint(got.Extra["top_k"]); k != "40" {
		t.Errorf("top_k %s on the wire for model %q, want 40; the overtaken model's passthrough options are riding on the current model's requests", k, ag.Model)
	}
}
