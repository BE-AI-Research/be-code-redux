package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/profiles"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// HelperFactory builds the local helper (spec §3): its provider, model and
// the window its loader resolved (0 when unknown). Injected from cmd for the
// same reason as ReviewerFactory — this package must not import the
// provider registry. nil means no helper can be built.
var HelperFactory func(ctx context.Context, cfg *config.Config) (provider.Provider, string, int, error)

// helperUnavailableNote is said once each time the helper goes down.
const helperUnavailableNote = "local helper unavailable; housekeeping uses the online model"

// helperDownFor is how long a helper that failed to build or to answer is
// left alone before the next chore tries it again.
const helperDownFor = 60 * time.Second

// errChoreTooBig is a chore whose prompt cannot be made to fit the helper's
// window. Compact reads it as a failed summary and continues from the task
// record; it never falls back to the online model, which would send the
// whole oversized transcript out instead.
var errChoreTooBig = errors.New("the prompt does not fit the local helper's window")

// helperState is the lazily built helper, shared by an agent and the scratch
// agents it spawns (plan mode) so the helper is built once per session.
// mu is held across the build, so concurrent chores wait for one build
// instead of starting two.
type helperState struct {
	mu        sync.Mutex
	built     bool
	p         provider.Provider
	model     string
	window    int
	downUntil time.Time
	noticed   bool // the unavailable note was said since the helper last answered
}

func (a *Agent) helperSt() *helperState {
	a.helperInit.Do(func() {
		if a.helper == nil {
			a.helper = &helperState{}
		}
	})
	return a.helper
}

// helperConfigured reports whether chores go to a local helper at all: only
// while the main model is online, with a helper model named and a factory
// to build it.
func (a *Agent) helperConfigured() bool {
	if a.Cfg == nil || a.Cfg.LocalHelper.Model == "" || HelperFactory == nil {
		return false
	}
	_, online := a.Online()
	return online
}

// choreTarget is where one housekeeping request goes.
type choreTarget struct {
	a        *Agent
	p        provider.Provider
	model    string
	window   int
	helper   bool
	thinking bool // the model's profile strips reasoning blocks
}

// choreModel picks the model for a housekeeping chore: the local helper
// while the main model is online and the helper is configured and not
// down, else the primary.
func (a *Agent) choreModel(ctx context.Context) (p provider.Provider, model string, window int, helper bool) {
	t := a.choreTarget(ctx)
	return t.p, t.model, t.window, t.helper
}

func (a *Agent) primaryTarget() choreTarget {
	a.modelMu.Lock()
	t := choreTarget{a: a, p: a.Provider, model: a.Model, thinking: a.Profile.StripThink}
	a.modelMu.Unlock()
	t.window = a.Window()
	return t
}

func (a *Agent) choreTarget(ctx context.Context) choreTarget {
	if !a.helperConfigured() {
		return a.primaryTarget()
	}
	h := a.helperSt()
	h.mu.Lock()
	if timeNow().Before(h.downUntil) {
		h.mu.Unlock()
		return a.primaryTarget()
	}
	if !h.built {
		p, model, window, err := buildHelper(ctx, a.Cfg)
		if err != nil {
			h.mu.Unlock()
			if ctx.Err() == nil {
				a.helperDown(err)
			}
			return a.primaryTarget()
		}
		if model == "" {
			model = a.Cfg.LocalHelper.Model
		}
		h.p, h.model, h.window, h.built = p, model, window, true
	}
	t := choreTarget{a: a, p: h.p, model: h.model, window: h.window, helper: true,
		thinking: profiles.Detect(h.model).StripThink}
	h.mu.Unlock()
	return t
}

// buildHelper runs the factory behind a panic fence: the helper's provider
// is a second wire format, and a panic in its build must not take the
// chore (often compaction, mid-run) down with it.
func buildHelper(ctx context.Context, cfg *config.Config) (p provider.Provider, model string, window int, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("local helper build panicked: %v", r)
		}
	}()
	p, model, window, err = HelperFactory(ctx, cfg)
	if err == nil && p == nil {
		err = errors.New("no local helper provider")
	}
	return p, model, window, err
}

// helperDown marks the helper down for helperDownFor and says so once.
func (a *Agent) helperDown(err error) {
	h := a.helperSt()
	h.mu.Lock()
	h.downUntil = timeNow().Add(helperDownFor)
	say := !h.noticed
	h.noticed = true
	h.mu.Unlock()
	fmt.Fprintf(os.Stderr, "local helper: %v\n", err)
	if say {
		a.notice("%s", helperUnavailableNote)
	}
}

func (a *Agent) helperAnswered() {
	h := a.helperSt()
	h.mu.Lock()
	h.noticed = false
	h.mu.Unlock()
}

// replyTokens is the chore's max_tokens: the primary's own harnessReplyTokens
// unchanged, or for the helper the same rule against the helper's provider
// and window. The session's max_tokens belongs to the primary and is not
// applied to the helper.
func (t choreTarget) replyTokens(limit int, prompt []provider.Message) int {
	if !t.helper {
		return t.a.harnessReplyTokens(limit, prompt)
	}
	n := t.capFor(limit)
	if t.window > 0 {
		if room := t.window - helperPromptTokens(prompt); room < n {
			n = room
		}
	}
	if n < minReplyTokens {
		n = minReplyTokens
	}
	return n
}

// capFor is the helper's reply cap before the window is consulted: limit,
// or the thinking headroom when its provider cannot switch reasoning off.
func (t choreTarget) capFor(limit int) int {
	n := limit
	if t.thinking {
		tc, ok := t.p.(provider.ThinkController)
		if !ok || !tc.HonoursNoThink() {
			r := t.window / 3
			if r < 4096 {
				r = 4096
			}
			if r > 16384 {
				r = 16384
			}
			if r > n {
				n = r
			}
		}
	}
	return n
}

// promptRoom is how many prompt tokens the helper's window leaves beside a
// reply of limit; -1 when the window is unknown (no trimming possible).
func (t choreTarget) promptRoom(limit int) int {
	if !t.helper || t.window <= 0 {
		return -1
	}
	return t.window - t.capFor(limit)
}

// helperPromptTokens is the harness's conservative estimate of a prompt, the
// one harnessReplyTokens uses for an uncalibrated model.
func helperPromptTokens(msgs []provider.Message) int {
	used := 0
	for _, m := range msgs {
		used += int(float64(len(m.Content))/defaultCharsPerToken) + 4
	}
	return used
}

// choreOpts tunes one chore: await waits for the primary's window to land
// before a request is built for it (the chores that did so before still do),
// urgent takes the helper's lane at primary priority (the review blocks the
// request the person is watching).
type choreOpts struct {
	await, urgent bool
}

// choreChat sends one housekeeping request: build makes it for the chosen
// target. A helper that fails to answer is marked down, said once, and the
// chore is retried once on the primary. A build error is returned as is.
func (a *Agent) choreChat(ctx context.Context, o choreOpts, build func(t choreTarget) (provider.ChatRequest, error)) (*provider.ChatResponse, choreTarget, error) {
	t := a.choreTarget(ctx)
	resp, err := a.choreSend(ctx, t, o, build)
	if err == nil || !t.helper || ctx.Err() != nil || errors.Is(err, errChoreTooBig) {
		return resp, t, err
	}
	a.helperDown(err)
	t = a.primaryTarget()
	resp, err = a.choreSend(ctx, t, o, build)
	return resp, t, err
}

func (a *Agent) choreSend(ctx context.Context, t choreTarget, o choreOpts, build func(t choreTarget) (provider.ChatRequest, error)) (*provider.ChatResponse, error) {
	if !t.helper && o.await {
		// Never send with no window on the wire; the model is read after
		// the wait, as these chores always did.
		a.awaitWindow(ctx)
		t = a.primaryTarget()
	}
	req, err := build(t)
	if err != nil {
		return nil, err
	}
	return a.choreDo(ctx, t, o, req)
}

// choreDo sends req to t: the primary in its own lane, unchanged from
// before the helper existed, or the helper in its server's lane.
func (a *Agent) choreDo(ctx context.Context, t choreTarget, o choreOpts, req provider.ChatRequest) (*provider.ChatResponse, error) {
	if !t.helper {
		return a.inLane(ctx, func() (*provider.ChatResponse, error) { return t.p.Chat(ctx, req, nil) })
	}
	resp, err := inLaneWith(ctx, a.laneFor(a.Cfg.LocalHelper.Provider, o.urgent), func() (resp *provider.ChatResponse, err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("local helper panicked: %v", r)
			}
		}()
		return t.p.Chat(ctx, req, nil)
	})
	if err != nil {
		return nil, err
	}
	a.helperAnswered()
	a.addHelperUsage(req, resp)
	return resp, nil
}

// addHelperUsage records a helper reply's tokens under the Helper* fields
// only: a local model costs nothing, and it never passes through the
// primary's spend computation.
func (a *Agent) addHelperUsage(req provider.ChatRequest, resp *provider.ChatResponse) {
	p, c := resp.Usage.PromptTokens, resp.Usage.CompletionTokens
	if p == 0 && c == 0 {
		p = helperPromptTokens(req.Messages)
		c = int(float64(len(resp.Content)) / defaultCharsPerToken)
	}
	a.addStats(Stats{HelperPromptTokens: p, HelperCompletionTokens: c})
}

// strip cleans a chore's reply the way the target's profile asks.
func (t choreTarget) strip(s string) string {
	if t.thinking {
		return StripThink(s)
	}
	return s
}
