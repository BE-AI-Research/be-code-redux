package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// fakeLoader records what the agent asked of it and answers instantly (or
// after delay, for the test that proves /model does not wait on it). Every
// field is behind mu: resolveModel runs on a goroutine of its own, so a
// test that read these directly would be the very race it is checking for.
type fakeLoader struct {
	mu        sync.Mutex
	window    int
	delay     time.Duration
	applied   []string
	evicted   []string
	changed   []int
	keepAlive time.Duration
}

func (f *fakeLoader) Apply(ctx context.Context, model string) (int, error) {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, model)
	return f.window, nil
}

func (f *fakeLoader) OnEvicted(_ context.Context, model string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evicted = append(f.evicted, model)
}

func (f *fakeLoader) OnWindowChanged(_ string, w int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changed = append(f.changed, w)
}

// keepAlive is what the agent asks for when refreshing residency; 0 means
// nothing configured, so the agent's own default stands.
func (f *fakeLoader) KeepAlive(string) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.keepAlive
}

func (f *fakeLoader) appliedModels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.applied...)
}

func (f *fakeLoader) evictedModels() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.evicted...)
}

func (f *fakeLoader) changedWindows() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.changed...)
}

// summarizingProvider answers every request with a one-line summary, which
// is all Compact needs to succeed.
func summarizingProvider() *funcProvider {
	return &funcProvider{fn: func(provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "the task so far, in one line"}, nil
	}}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// noticeSink collects notices from whichever goroutine emits them.
type noticeSink struct {
	mu   sync.Mutex
	msgs []string
}

func (n *noticeSink) add(m string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.msgs = append(n.msgs, m)
}

func (n *noticeSink) has(sub string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	for _, m := range n.msgs {
		if strings.Contains(m, sub) {
			return true
		}
	}
	return false
}

func (n *noticeSink) all() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.msgs...)
}

// over reads History.Over() with the turn lock held, which is how the
// production compaction path serialises against a request. A test that read
// it bare would race the very compaction it is waiting for.
func over(ag *Agent) bool {
	ag.turnMu.Lock()
	defer ag.turnMu.Unlock()
	return ag.History.Tokens() > ag.History.Limit()
}

// fillHistory pads the transcript past tokens' worth of text, so a smaller
// window really is smaller than what the conversation occupies.
func fillHistory(t *testing.T, ag *Agent, tokens int) {
	t.Helper()
	chunk := strings.Repeat("context that the next model cannot hold. ", 40)
	for ag.History.Tokens() < tokens {
		ag.History.Messages = append(ag.History.Messages,
			provider.Message{Role: provider.RoleUser, Content: chunk},
			provider.Message{Role: provider.RoleAssistant, Content: chunk})
	}
}

// TestSetModelDoesNotBlockOnTheLoader: a loader that takes a second must
// not make /model take a second. SetModel is called from a Bubble Tea
// Update and from the REPL's own goroutine; either one parked for the
// length of a model reload is a frozen UI.
func TestSetModelDoesNotBlockOnTheLoader(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	ag.SetLoader(&fakeLoader{window: 8192, delay: time.Second})
	start := time.Now()
	ag.SetModel("other-model")
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("SetModel blocked for %s", d)
	}
	if ag.Model != "other-model" {
		t.Fatalf("the switch itself must be synchronous; model is %q", ag.Model)
	}
}

// TestWindowFollowsTheModel: switching models re-derives the budget and the
// reserve from the new model's window, which 0.10.0 never did.
func TestWindowFollowsTheModel(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 0 })
	ag.ApplyWindow(32768)
	l := &fakeLoader{window: 8192}
	ag.SetLoader(l)
	ag.SetModel("small-model")
	waitFor(t, "the new window", func() bool { return ag.Window() == 8192 })
	budget, reserve, _ := ag.History.Scalars()
	if budget > 8192 {
		t.Fatalf("budget %d exceeds the new window", budget)
	}
	if reserve == 0 {
		t.Fatal("reserve was not re-derived")
	}
	if got := l.appliedModels(); len(got) != 1 || got[0] != "small-model" {
		t.Fatalf("the loader was asked for %v", got)
	}
}

// TestASmallerWindowCompactsOnce: rather than letting the next request
// truncate silently on the server.
func TestASmallerWindowCompactsOnce(t *testing.T) {
	ag, _ := newTestAgent(t, summarizingProvider(), func(c *config.Config) { c.ContextTokens = 0 })
	ag.ApplyWindow(32768)
	fillHistory(t, ag, 20000)
	var notices noticeSink
	ag.Events.OnNotice = notices.add
	ag.SetLoader(&fakeLoader{window: 8192})
	ag.SetModel("small-model")
	waitFor(t, "the compaction notice", func() bool { return notices.has("compacting once") })
	waitFor(t, "the conversation to fit", func() bool { return !over(ag) })
	if !notices.has("smaller than this conversation") {
		t.Fatalf("no notice about the compaction: %v", notices.all())
	}
}

// TestAStaleModelResolutionIsDiscarded: two switches in quick succession
// must not let the first model's window land on the second model. The
// loader runs off the caller's goroutine, so the answers can arrive in any
// order; only the newest switch owns the session's window.
func TestAStaleModelResolutionIsDiscarded(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 0 })
	slow := &fakeLoader{window: 4096, delay: 300 * time.Millisecond}
	ag.SetLoader(slow)
	ag.SetModel("slow-model")
	ag.SetLoader(&fakeLoader{window: 32768})
	ag.SetModel("fast-model")
	waitFor(t, "the newest window", func() bool { return ag.Window() == 32768 })
	waitFor(t, "the stale resolution to finish", func() bool { return len(slow.appliedModels()) == 1 })
	time.Sleep(50 * time.Millisecond)
	if ag.Window() != 32768 {
		t.Fatalf("a stale resolution clobbered the current model's window: %d", ag.Window())
	}
}

// TestEvictionReloadsAtOurWindow: the backend-status trip routes through the
// loader, so the reload that is coming anyway uses our parameters. Nothing
// was holding the model, so this needs no consent.
func TestEvictionReloadsAtOurWindow(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	l := &fakeLoader{window: 32768}
	ag.SetLoader(l)
	ag.Provider = &statusProvider{funcProvider: &funcProvider{}, window: 0, loaded: false}
	ag.checkBackend(context.Background())
	if got := l.evictedModels(); len(got) != 1 || got[0] != ag.Model {
		t.Fatalf("the loader was not told about the eviction: %v", got)
	}
}

// TestAnotherClientsWindowChangeIsNotFought: we adapt to their window; we
// never reload back. A reload war between two clients on a shared server is
// the worst outcome available.
func TestAnotherClientsWindowChangeIsNotFought(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 0 })
	l := &fakeLoader{window: 32768}
	ag.SetLoader(l)
	ag.ApplyWindow(32768)
	ag.Provider = &statusProvider{funcProvider: &funcProvider{}, window: 4096, loaded: true}
	ag.checkBackend(context.Background())
	if ag.Window() != 4096 {
		t.Fatalf("did not adapt: window %d", ag.Window())
	}
	if got := l.changedWindows(); len(got) != 1 || got[0] != 4096 {
		t.Fatalf("loader not told: %v", got)
	}
	if got := l.appliedModels(); len(got) != 0 {
		t.Fatalf("we reloaded back at our own window; that is a reload war: %v", got)
	}
}

// TestModelResolutionDoesNotRewriteHistoryUnderARunningRequest: the one
// thing resolveModel does that a request also does is rewrite the
// transcript. A compaction fired from the switch's goroutine while the tool
// loop is between calls would corrupt it, so the switch takes the turn lock
// and, failing, leaves the work to the loop's own maybeCompact — without
// even measuring the conversation, since measuring it means reading what
// the request is rewriting.
func TestModelResolutionDoesNotRewriteHistoryUnderARunningRequest(t *testing.T) {
	ag, _ := newTestAgent(t, summarizingProvider(), func(c *config.Config) { c.ContextTokens = 0 })
	ag.ApplyWindow(32768)
	fillHistory(t, ag, 20000)
	before := len(ag.History.Messages)
	var notices noticeSink
	ag.Events.OnNotice = notices.add
	ag.turnMu.Lock() // stand in for a request in flight
	ag.SetLoader(&fakeLoader{window: 8192})
	ag.SetModel("small-model")
	waitFor(t, "the smaller-window notice", func() bool { return notices.has("compacts if it needs to") })
	if notices.has("compacting once") {
		t.Fatalf("it compacted under a running request: %v", notices.all())
	}
	if len(ag.History.Messages) != before {
		t.Fatal("history was rewritten while a request held the turn")
	}
	ag.turnMu.Unlock()
}

// TestNoticeReportsWhetherAnyUIHeardIt: cmd builds the loader before a UI
// exists, so it needs to know whether a notice reached a transcript or must
// go to stderr instead.
func TestNoticeReportsWhetherAnyUIHeardIt(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	if ag.Notice("nobody is listening") {
		t.Fatal("an unwired agent must report that the notice went nowhere")
	}
	var notices noticeSink
	ag.Events.OnNotice = notices.add
	if !ag.Notice("somebody is listening") {
		t.Fatal("a wired agent must report delivery")
	}
	if !notices.has("somebody is listening") {
		t.Fatalf("notice not delivered: %v", notices.all())
	}
}

// TestResolveModelAsksThroughWhoeverIsWired is the startup half of the
// wiring: the loader is handed to the agent before any UI exists, and
// re-running it once an approver is wired is what turns a question that was
// refused into one that is asked.
func TestResolveModelAsksThroughWhoeverIsWired(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 0 })
	asked := make(chan string, 1)
	ag.SetLoader(askingLoader{ag: ag, asked: asked, window: 8192})
	// Nobody wired yet: the registry's approver is the test's blanket yes,
	// so this simply proves the path runs at all.
	ag.ResolveModel()
	select {
	case a := <-asked:
		if a != "model_reload" {
			t.Fatalf("asked %q", a)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("ResolveModel never reached the approval seam")
	}
	waitFor(t, "the consented window", func() bool { return ag.Window() == 8192 })
}

// askingLoader is a loader that needs consent, like the real one does when
// a model is resident at another window.
type askingLoader struct {
	ag     *Agent
	asked  chan string
	window int
}

func (l askingLoader) Apply(_ context.Context, _ string) (int, error) {
	if l.ag.Tools.Approve == nil {
		return 0, fmt.Errorf("nobody to ask")
	}
	l.asked <- "model_reload"
	if !l.ag.Tools.Approve("model_reload", "model m is loaded with a 4096-token window") {
		return 0, nil
	}
	return l.window, nil
}
func (askingLoader) OnEvicted(context.Context, string) {}
func (askingLoader) OnWindowChanged(string, int)       {}
func (askingLoader) KeepAlive(string) time.Duration    { return 0 }

// TestAProviderSwitchGetsItsOwnLoader: a loader speaks for one backend and
// carries that server's consent record. Keeping it across /provider would
// put one machine's num_ctx on the wire for another, and ask the user about
// a server they have just left.
func TestAProviderSwitchGetsItsOwnLoader(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 0 })
	old := &fakeLoader{window: 4096}
	ag.SetLoader(old)

	fresh := &fakeLoader{window: 32768}
	var built int
	LoaderFactory = func(*config.Config, provider.Provider) ModelLoader { built++; return fresh }
	t.Cleanup(func() { LoaderFactory = nil })

	ag.SetProvider(&scriptedProvider{})
	if built != 1 {
		t.Fatalf("the switch did not build a loader for the new backend (%d)", built)
	}
	ag.SetModel("m")
	waitFor(t, "the new backend's window", func() bool { return ag.Window() == 32768 })
	if got := old.appliedModels(); len(got) != 0 {
		t.Fatalf("the old server's loader was still used: %v", got)
	}
}

// And with nothing able to build one, the switch simply stops resolving
// rather than resolving against the wrong machine.
func TestAProviderSwitchWithNoFactoryDropsTheLoader(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	old := &fakeLoader{window: 4096}
	ag.SetLoader(old)
	LoaderFactory = nil

	ag.SetProvider(&scriptedProvider{})
	ag.SetModel("m")
	time.Sleep(50 * time.Millisecond)
	if got := old.appliedModels(); len(got) != 0 {
		t.Fatalf("the old server's loader was still used: %v", got)
	}
}

// TestConcurrentModelSwitchesAndBackendChecks is the -race proof for the
// state this seam made concurrent. It models the real contract, not an
// imaginary one: model switches come from exactly one goroutine (a Bubble
// Tea Update, or the REPL loop), each of them spawns a resolution that
// lands a window from a goroutine of its own, the tool loop adapts to a
// third party's reload through ApplyWindow, and a UI reads the numbers it
// draws throughout.
func TestConcurrentModelSwitchesAndBackendChecks(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 0 })
	ag.SetLoader(&fakeLoader{window: 16384})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the one UI goroutine that switches models
		defer wg.Done()
		for n := 0; n < 200; n++ {
			ag.SetModel(fmt.Sprintf("model-%d", n))
		}
	}()
	wg.Add(1)
	go func() { // the tool loop adapting to another client's reload
		defer wg.Done()
		for n := 0; n < 200; n++ {
			ag.ApplyWindow(8192 + (n%2)*8192)
		}
	}()
	wg.Add(1)
	go func() { // what a UI reads while all of that is happening
		defer wg.Done()
		for n := 0; n < 400; n++ {
			_ = ag.Window()
			_, _, _ = ag.History.Scalars()
			_ = ag.Tools.MaxOutput()
		}
	}()
	wg.Wait()
	waitFor(t, "every resolution to land", func() bool { return ag.Window() > 0 })
}

// TestBudgetScalarsAreReadUnderTheLock: ApplyWindow writes Budget and
// Reserve, and since a model switch resolves its window on a goroutine of
// its own it writes them from there — before it ever reaches the turn lock.
// Limit, Target and Over are read meanwhile by the tool loop's budgeting
// notices and by both UIs' context wheels, so they cannot read the scalars
// bare. Fails under -race before Limit took the lock.
func TestBudgetScalarsAreReadUnderTheLock(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.ContextTokens = 0 })
	fillHistory(t, ag, 4000)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the resolution goroutine landing a switch's window
		defer wg.Done()
		for n := 0; n < 300; n++ {
			ag.ApplyWindow(8192 + (n%2)*8192)
		}
	}()
	wg.Add(1)
	go func() { // what the loop's own budgeting and a UI's wheel read
		defer wg.Done()
		for n := 0; n < 300; n++ {
			_ = ag.History.Limit()
			_ = ag.History.Target()
			_ = ag.History.Over()
		}
	}()
	wg.Wait()
}

// TestUserHistoryRewritesTakeTheTurnLock: /clear and /compact were safe
// while the tool loop was the only other writer of the transcript and a UI
// only touched it when not running. The resolution goroutine broke that —
// it can compact while the UI believes itself idle — so both now go through
// the agent, under the same lock a request holds.
func TestUserHistoryRewritesTakeTheTurnLock(t *testing.T) {
	ag, _ := newTestAgent(t, summarizingProvider(), nil)
	fillHistory(t, ag, 2000)
	before := len(ag.History.Messages)

	ag.turnMu.Lock() // stand in for a request, or for the post-switch compaction
	cleared := make(chan struct{})
	go func() { ag.ClearHistory(); close(cleared) }()
	select {
	case <-cleared:
		t.Fatal("/clear rewrote the transcript while something else held the turn")
	case <-time.After(50 * time.Millisecond):
	}
	if len(ag.History.Messages) != before {
		t.Fatal("the transcript was cleared out from under the lock holder")
	}
	ag.turnMu.Unlock()
	select {
	case <-cleared:
	case <-time.After(2 * time.Second):
		t.Fatal("/clear never completed after the lock was released")
	}
	if len(ag.History.Messages) != 0 {
		t.Fatalf("history not cleared: %d messages", len(ag.History.Messages))
	}
}

// And the same for /compact, which is the other rewrite a user asks for.
func TestCompactNowTakesTheTurnLock(t *testing.T) {
	ag, _ := newTestAgent(t, summarizingProvider(), nil)
	fillHistory(t, ag, 2000)
	ag.turnMu.Lock()
	done := make(chan error, 1)
	go func() { done <- ag.CompactNow(context.Background()) }()
	select {
	case <-done:
		t.Fatal("/compact rewrote the transcript while something else held the turn")
	case <-time.After(50 * time.Millisecond):
	}
	ag.turnMu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("compaction failed: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("/compact never completed after the lock was released")
	}
}

// TestCompactSnapshotsTheModelItIsSummarizingFor: Compact runs on the
// resolution's goroutine after a model switch, and a second switch landing
// mid-compaction would otherwise be read half-applied — a summary addressed
// to one model and stripped as if it came from another. Fails under -race
// before the snapshot.
func TestCompactSnapshotsTheModelItIsSummarizingFor(t *testing.T) {
	ag, _ := newTestAgent(t, summarizingProvider(), nil)
	fillHistory(t, ag, 2000)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { // the one UI goroutine that switches models
		defer wg.Done()
		for n := 0; ; n++ {
			select {
			case <-stop:
				return
			default:
			}
			ag.SetModel(fmt.Sprintf("model-%d", n))
		}
	}()
	// Refilling goes through the turn lock and never measures the history:
	// Tokens reads the system prompt, which the switching goroutine is
	// rewriting, and that read is the test's own race, not the code's.
	chunk := strings.Repeat("more transcript. ", 100)
	refill := func() {
		ag.turnMu.Lock()
		for i := 0; i < 20; i++ {
			ag.History.Messages = append(ag.History.Messages,
				provider.Message{Role: provider.RoleUser, Content: chunk},
				provider.Message{Role: provider.RoleAssistant, Content: chunk})
		}
		ag.turnMu.Unlock()
	}
	for n := 0; n < 20; n++ {
		refill()
		if err := ag.CompactNow(context.Background()); err != nil {
			t.Fatalf("compaction failed: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}

// TestKeepAliveComesFromTheLoader: keep_alive is resolved per model (the
// models entry, then the provider block, then the top level), and the
// residency refresh has to use the same answer — reading cfg.KeepAlive here
// quietly undid a per-model setting.
func TestKeepAliveComesFromTheLoader(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.KeepAlive = "30m" })
	sp := &statusProvider{funcProvider: &funcProvider{}, window: 8192, loaded: true}
	ag.Provider = sp
	ag.SetLoader(&fakeLoader{window: 8192, keepAlive: 90 * time.Second})

	ag.refreshKeepAlive()
	waitFor(t, "the keep-alive refresh", func() bool { return sp.keepAlives() == 1 })
	sp.mu.Lock()
	got := sp.kept[0]
	sp.mu.Unlock()
	if got != 90*time.Second {
		t.Fatalf("refreshed for %s; the loader said 90s and config said 30m", got)
	}
}

// With no loader, or nothing configured for the model, the old behaviour
// stands: a session must not lose its residency refresh to this change.
func TestKeepAliveFallsBackToConfig(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { c.KeepAlive = "10m" })
	sp := &statusProvider{funcProvider: &funcProvider{}, window: 8192, loaded: true}
	ag.Provider = sp
	ag.SetLoader(&fakeLoader{window: 8192}) // keepAlive 0: nothing configured

	ag.refreshKeepAlive()
	waitFor(t, "the keep-alive refresh", func() bool { return sp.keepAlives() == 1 })
	sp.mu.Lock()
	got := sp.kept[0]
	sp.mu.Unlock()
	if got != 10*time.Minute {
		t.Fatalf("refreshed for %s; config said 10m", got)
	}
}
