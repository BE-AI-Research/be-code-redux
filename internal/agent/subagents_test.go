package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/subagent"
)

// subFixture is a primary agent with a store, one sub-agent co-worker
// "big" on a scripted provider, and channels the tests wait on.
type subFixture struct {
	ag    *Agent
	st    *engine.Store
	dir   string
	sub   *scriptedProvider
	start chan subagent.Dispatch
	asks  chan subagent.Ask
	ends  chan subagent.HandBack
}

func newSubFixture(t *testing.T, sub *scriptedProvider, mut func(*config.Config)) *subFixture {
	t.Helper()
	ag, dir := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) {
		c.Coworkers = []config.CoworkerConfig{{Name: "big", Provider: "ollama", Model: "cw-model-big", SubAgent: true}}
		c.Providers["ollama"] = config.ProviderConfig{Type: "ollama", BaseURL: "http://sub:11434"}
		c.SubAgents = config.SubAgentsConfig{MaxConcurrent: 2, MaxTurns: 6, AskTimeout: 600}
		if mut != nil {
			mut(c)
		}
	})
	f := &subFixture{ag: ag, dir: dir, sub: sub,
		start: make(chan subagent.Dispatch, 4), asks: make(chan subagent.Ask, 4), ends: make(chan subagent.HandBack, 4)}
	f.st = withEngine(t, ag)
	ag.Events.OnSubAgentStart = func(d subagent.Dispatch) { f.start <- d }
	ag.Events.OnSubAgentAsk = func(a subagent.Ask) { f.asks <- a }
	ag.Events.OnSubAgentEnd = func(hb subagent.HandBack) { f.ends <- hb }
	CoworkerFactory = func(context.Context, *config.Config, config.CoworkerConfig) (provider.Provider, int, error) {
		return sub, 0, nil
	}
	t.Cleanup(func() { CoworkerFactory = nil; ag.StopAllSubAgents("test over") })
	if err := os.MkdirAll(filepath.Join(dir, "internal/scan"), 0o755); err != nil {
		t.Fatal(err)
	}
	cws, _ := ag.Cfg.ValidCoworkers()
	ag.EnableSubAgents(cws, "http://primary:11434")
	return f
}

// assign plans a task with one step owned by big and returns the step id.
func (f *subFixture) assign(t *testing.T) string {
	t.Helper()
	root := f.st.Plan("port the scanner", []string{"port internal/scan"})
	id := root + ".1"
	if err := f.st.SetOwner(id, "big", false); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetScope(id, []string{"internal/scan"}); err != nil {
		t.Fatal(err)
	}
	return id
}

func wait[T any](t *testing.T, ch chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func toolCall(name, args string) provider.ChatResponse {
	return provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "c", Name: name, Arguments: args}}}
}

func TestSubAgentOwnsAStepToDone(t *testing.T) {
	sub := &scriptedProvider{responses: []provider.ChatResponse{
		toolCall("write_file", `{"path":"internal/scan/token.go","content":"package scan\n"}`),
		toolCall("task", `{"action":"status","id":"1.1","status":"done"}`),
		{Content: "ported the token loop"},
	}}
	f := newSubFixture(t, sub, nil)
	id := f.assign(t)
	f.ag.ScheduleSubAgents()
	d := wait(t, f.start, "start")
	if d.Node != id || d.Owner != "big" || d.Scope[0] != "internal/scan" || d.MaxTurns != 6 {
		t.Fatalf("dispatch: %+v", d)
	}
	hb := wait(t, f.ends, "hand-back")
	if hb.Status != "done" || hb.Summary != "ported the token loop" || len(hb.Files) != 1 || hb.Files[0] != "internal/scan/token.go" {
		t.Fatalf("hand-back: %+v", hb)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "internal/scan/token.go")); err != nil {
		t.Fatal("the sub-agent's file was not written")
	}
	if f.ag.Pending() != 1 {
		t.Fatalf("queued messages: %d", f.ag.Pending())
	}
	line := f.ag.DrainInbox()[0]
	if !strings.HasPrefix(line, "sub-agent big finished "+id+" (done, ") || !strings.Contains(line, "wrote internal/scan/token.go") || !strings.HasSuffix(line, "ported the token loop") {
		t.Fatalf("hand-back line: %q", line)
	}
	if got := f.st.ShowText(id); !strings.Contains(got, "[x] "+id+".") {
		t.Fatalf("node not closed:\n%s", got)
	}
	if len(f.st.Dispatched()) != 0 {
		t.Fatal("dispatched mark not cleared")
	}
	// The sub-agent's system prompt carried the frame and the dispatch, and
	// its first request went to the co-worker's provider, not the primary's.
	sys := sub.lastReq.Messages[0].Content
	if !strings.HasPrefix(sys, SubAgentFrame) || !strings.Contains(sys, "Your step: "+id+" port internal/scan") || !strings.Contains(sys, "Scope (the only paths you may write): internal/scan") {
		t.Fatalf("sub-agent system prompt:\n%s", sys)
	}
}

func TestSubAgentAskMainRoundTrip(t *testing.T) {
	sub := &scriptedProvider{responses: []provider.ChatResponse{
		toolCall("write_file", `{"path":"cmd/x.go","content":"x"}`),
		toolCall("ask_main", `{"question":"may I edit cmd/x.go?"}`),
		{Content: "done as told"},
	}}
	f := newSubFixture(t, sub, nil)
	id := f.assign(t)
	f.ag.ScheduleSubAgents()
	wait(t, f.start, "start")
	ask := wait(t, f.asks, "ask")
	if ask.Node != id || ask.Owner != "big" || ask.Question != "may I edit cmd/x.go?" {
		t.Fatalf("ask: %+v", ask)
	}
	if f.ag.Pending() != 1 || !strings.HasPrefix(f.ag.DrainInbox()[0], "sub-agent big asks about "+id+": may I edit cmd/x.go?") {
		t.Fatal("the ask was not queued for the main model")
	}
	if err := f.ag.ReplyAsk(id, "no; leave cmd alone"); err != nil {
		t.Fatal(err)
	}
	if err := f.ag.ReplyAsk(id, "again"); err == nil {
		t.Fatal("a second reply with no open ask must fail")
	}
	hb := wait(t, f.ends, "hand-back")
	if hb.Status != "done" {
		t.Fatalf("hand-back: %+v", hb)
	}
	// The refused write and the reply both reached the sub-agent as tool results.
	var sawRefusal, sawReply bool
	for _, m := range sub.lastReq.Messages {
		if strings.Contains(m.Content, "outside your scope (internal/scan)") {
			sawRefusal = true
		}
		if strings.Contains(m.Content, "the main model replied:\n\nno; leave cmd alone") {
			sawReply = true
		}
	}
	if !sawRefusal || !sawReply {
		t.Fatalf("refusal %v reply %v in:\n%+v", sawRefusal, sawReply, sub.lastReq.Messages)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "cmd/x.go")); err == nil {
		t.Fatal("an out-of-scope write landed on disk")
	}
	if got := f.st.ShowText(id); !strings.Contains(got, "may I edit cmd/x.go?") || !strings.Contains(got, "no; leave cmd alone") {
		t.Fatalf("ask and reply not recorded on the node:\n%s", got)
	}
}

func TestSubAgentAskTimesOutToBlocked(t *testing.T) {
	sub := &scriptedProvider{responses: []provider.ChatResponse{
		toolCall("ask_main", `{"question":"anyone there?"}`),
		{Content: "should never be reached"},
	}}
	f := newSubFixture(t, sub, func(c *config.Config) { c.SubAgents.AskTimeout = 1 })
	id := f.assign(t)
	f.ag.ScheduleSubAgents()
	wait(t, f.asks, "ask")
	hb := wait(t, f.ends, "hand-back")
	if hb.Status != "blocked" || hb.Reason != "no answer to: anyone there?" {
		t.Fatalf("hand-back: %+v", hb)
	}
	if got := f.st.ShowText(id); !strings.Contains(got, "[!] "+id+".") {
		t.Fatalf("node not blocked:\n%s", got)
	}
}

func TestSubAgentTurnCapAndPanicBlock(t *testing.T) {
	loop := &scriptedProvider{}
	for i := 0; i < 10; i++ {
		loop.responses = append(loop.responses, toolCall("list_dir", `{"path":"."}`))
	}
	f := newSubFixture(t, loop, func(c *config.Config) { c.SubAgents.MaxTurns = 2 })
	f.assign(t)
	f.ag.ScheduleSubAgents()
	hb := wait(t, f.ends, "hand-back")
	if hb.Status != "blocked" || hb.Reason != "turn cap of 2 reached" {
		t.Fatalf("turn cap: %+v", hb)
	}
	pp := &panicProvider{}
	CoworkerFactory = func(context.Context, *config.Config, config.CoworkerConfig) (provider.Provider, int, error) {
		return pp, 0, nil
	}
	root := f.st.Plan("second", []string{"again"})
	_ = f.st.SetOwner(root+".1", "big", false)
	_ = f.st.SetScope(root+".1", []string{"internal/scan"})
	f.ag.ScheduleSubAgents()
	hb = wait(t, f.ends, "hand-back after panic")
	if hb.Status != "blocked" || !strings.HasPrefix(hb.Reason, "internal error: ") {
		t.Fatalf("panic: %+v", hb)
	}
	if _, err := f.ag.Run(context.Background(), "still alive?"); err != nil {
		t.Fatalf("the primary must survive a sub-agent panic: %v", err)
	}
}

func TestOnlineSubAgentAsksConsentNamingTheScope(t *testing.T) {
	sub := &scriptedProvider{responses: []provider.ChatResponse{{Content: "never sent"}}}
	var detail string
	f := newSubFixture(t, sub, func(c *config.Config) { c.Coworkers[0].Online = true })
	f.ag.Tools.Approve = func(action, d string) bool { detail = action + "\n" + d; return false }
	id := f.assign(t)
	f.ag.ScheduleSubAgents()
	hb := wait(t, f.ends, "hand-back")
	if hb.Status != "blocked" || hb.Reason != "consent refused" {
		t.Fatalf("refusal: %+v", hb)
	}
	if !strings.HasPrefix(detail, "consult\ncoworker: big (ollama/cw-model-big)") || !strings.Contains(detail, "scope: internal/scan") || !strings.Contains(detail, "origin: sub-agent "+id) {
		t.Fatalf("consent detail: %q", detail)
	}
	if sub.i != 0 {
		t.Fatal("a request reached the online co-worker without consent")
	}
	// Refusal is remembered: a second assigned step is blocked without asking.
	detail = ""
	root := f.st.Plan("second", []string{"again"})
	_ = f.st.SetOwner(root+".1", "big", false)
	_ = f.st.SetScope(root+".1", []string{"internal/scan"})
	f.ag.ScheduleSubAgents()
	hb = wait(t, f.ends, "second hand-back")
	if hb.Reason != "consent refused" || detail != "" {
		t.Fatalf("refusal not remembered: %+v %q", hb, detail)
	}
}

// parkingProvider parks every Chat until its context ends.
type parkingProvider struct {
	scriptedProvider
	entered chan struct{}
}

func (p *parkingProvider) Chat(ctx context.Context, req provider.ChatRequest, _ provider.StreamFunc) (*provider.ChatResponse, error) {
	p.lastReq = req
	select {
	case p.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestStopAllInterruptsAndResumeRedispatches(t *testing.T) {
	bp := &parkingProvider{entered: make(chan struct{}, 1)}
	f := newSubFixture(t, &scriptedProvider{}, nil)
	CoworkerFactory = func(context.Context, *config.Config, config.CoworkerConfig) (provider.Provider, int, error) {
		return bp, 0, nil
	}
	id := f.assign(t)
	f.ag.ScheduleSubAgents()
	wait(t, f.start, "start")
	wait(t, bp.entered, "the sub-agent's first request")
	// Esc on the main run: a cancelled Run context does not reach the sub-agent.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = f.ag.Run(ctx, "cancelled at once")
	if len(f.ag.RunningSubAgents()) != 1 {
		t.Fatal("cancelling a main run stopped the sub-agent")
	}
	f.ag.StopAllSubAgents("session ended")
	hb := wait(t, f.ends, "interrupt")
	if hb.Status != "interrupted" {
		t.Fatalf("expected interrupted, got %+v", hb)
	}
	if f.ag.Pending() != 0 {
		t.Fatal("an interruption is not a hand-back")
	}
	shown := f.st.ShowText(id)
	if !strings.Contains(shown, "[ ] "+id+".") || !strings.Contains(shown, "interrupted ") {
		t.Fatalf("node after interrupt:\n%s", shown)
	}
	// Resume: enabling again re-dispatches silently with Interrupted set.
	bp2 := &parkingProvider{entered: make(chan struct{}, 1)}
	CoworkerFactory = func(context.Context, *config.Config, config.CoworkerConfig) (provider.Provider, int, error) {
		return bp2, 0, nil
	}
	cws, _ := f.ag.Cfg.ValidCoworkers()
	f.ag.EnableSubAgents(cws, "http://primary:11434")
	f.ag.StartSubAgents()
	d := wait(t, f.start, "resumed start")
	if !d.Interrupted {
		t.Fatalf("resumed dispatch not marked interrupted: %+v", d)
	}
	wait(t, bp2.entered, "resumed request")
	if !strings.Contains(bp2.lastReq.Messages[0].Content, "This step was interrupted earlier") {
		t.Fatal("the resumed sub-agent was not told")
	}
	f.ag.StopAllSubAgents("test over")
	wait(t, f.ends, "second interrupt")
}

func TestResumeThatCannotRedispatchBlocksAndNotices(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, nil)
	id := f.assign(t)
	if err := f.st.Interrupt(id, time.Now(), 2, []string{"internal/scan/a.go"}); err != nil {
		t.Fatal(err)
	}
	var notices []string
	f.ag.Events.OnNotice = func(m string) { notices = append(notices, m) }
	// The owner is gone from the config.
	f.ag.EnableSubAgents(nil, "http://primary:11434")
	f.ag.StartSubAgents()
	hb := wait(t, f.ends, "hand-back")
	if hb.Status != "blocked" || hb.Reason != "big is not in coworkers" {
		t.Fatalf("hand-back: %+v", hb)
	}
	if f.ag.Pending() != 1 || len(notices) != 1 || !strings.Contains(notices[0], "sub-agent big cannot resume "+id+": big is not in coworkers") {
		t.Fatalf("pending %d notices %q", f.ag.Pending(), notices)
	}
}

func TestAssignOwnerStopsAParkedRunAndStopByName(t *testing.T) {
	sub := &scriptedProvider{responses: []provider.ChatResponse{
		toolCall("ask_main", `{"question":"which tokenizer?"}`),
		{Content: "unreachable"},
	}}
	f := newSubFixture(t, sub, nil)
	id := f.assign(t)
	f.ag.ScheduleSubAgents()
	wait(t, f.asks, "ask")
	if err := f.ag.AssignOwner(id, "", true); err != nil {
		t.Fatal(err)
	}
	hb := wait(t, f.ends, "interrupt")
	if hb.Status != "interrupted" {
		t.Fatalf("%+v", hb)
	}
	if n := f.st.ShowText(id); !strings.Contains(n, "[ ] "+id+". port internal/scan\n") && !strings.Contains(n, "[ ] "+id+". port internal/scan  scope") {
		t.Fatalf("owner not cleared:\n%s", n)
	}
	// Stop by name.
	bp := &parkingProvider{entered: make(chan struct{}, 1)}
	CoworkerFactory = func(context.Context, *config.Config, config.CoworkerConfig) (provider.Provider, int, error) {
		return bp, 0, nil
	}
	_ = f.st.SetOwner(id, "big", true)
	f.ag.ScheduleSubAgents()
	wait(t, bp.entered, "request")
	if err := f.ag.StopSubAgent("nobody"); err == nil {
		t.Fatal("stopping an unknown sub-agent must fail")
	}
	if err := f.ag.StopSubAgent("big"); err != nil {
		t.Fatal(err)
	}
	hb = wait(t, f.ends, "stop")
	if hb.Status != "blocked" || hb.Reason != "stopped by operator" {
		t.Fatalf("%+v", hb)
	}
}

func TestMainWriteInsideARunningScopeGetsAFooter(t *testing.T) {
	bp := &parkingProvider{entered: make(chan struct{}, 1)}
	f := newSubFixture(t, &scriptedProvider{}, nil)
	CoworkerFactory = func(context.Context, *config.Config, config.CoworkerConfig) (provider.Provider, int, error) {
		return bp, 0, nil
	}
	id := f.assign(t)
	f.ag.ScheduleSubAgents()
	wait(t, bp.entered, "request")
	res := f.ag.dispatch(context.Background(), provider.ToolCall{ID: "1", Name: "write_file",
		Arguments: `{"path":"internal/scan/x.go","content":"x"}`})
	if res.IsError || !strings.Contains(res.Content, "note: internal/scan is owned by big ("+id+") until it hands back") {
		t.Fatalf("footer missing: %+v", res)
	}
}

func TestPrimaryModelCallsTakeTheLane(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, nil)
	var mu sync.Mutex
	acquired := 0
	f.ag.laneAcquire = func(ctx context.Context) (func(), error) {
		mu.Lock()
		acquired++
		mu.Unlock()
		return func() {}, nil
	}
	if _, err := f.ag.Run(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if acquired != 1 {
		t.Fatalf("lane taken %d times for one request", acquired)
	}
}

// The scheduler runs on the primary's own goroutine (at the top of run, and
// after every task call), so a store that panics there must detach the
// engine like any other advisory call — not end the session.
func TestSchedulingSurvivesAPanickingStore(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, nil)
	f.assign(t)
	var notices []string
	f.ag.Events.OnNotice = func(m string) { notices = append(notices, m) }
	f.ag.engineFault = func(op string) {
		if strings.HasPrefix(op, "sub-agent ") {
			panic("the store exploded in " + op)
		}
	}
	f.ag.ScheduleSubAgents()
	if len(f.ag.RunningSubAgents()) != 0 {
		t.Fatal("a detached engine must yield no steps and dispatch nothing")
	}
	// The primary keeps working, and the fence said so exactly once.
	if _, err := f.ag.Run(context.Background(), "still alive?"); err != nil {
		t.Fatalf("the primary must survive a panicking store in the scheduler: %v", err)
	}
	// Exactly one detach notice, and it names the scheduler's own read. The
	// op matters: engineFault only fires from inside engineDo, so a Steps()
	// call put back in front of the fence would not panic there at all and
	// the first recovered panic would name a later op instead.
	n := 0
	for _, m := range notices {
		if strings.Contains(m, "continuing without working memory") {
			n++
			if !strings.Contains(m, "engine: sub-agent schedule failed") {
				t.Fatalf("the scheduler's Steps() read is not behind the fence: %q", m)
			}
		}
	}
	if n != 1 {
		t.Fatalf("expected one detach notice, got %d: %q", n, notices)
	}
}

// engineDo raises its detach notice synchronously, and Task 8's UI answers
// a notice by redrawing — which reads /agents and the bottom line. If the
// runner's mutex were held across the store call that panicked, that read
// would self-deadlock on a non-reentrant mutex. This is that exact shape:
// the panic comes from DispatchContext, inside the dispatch path.
func TestANoticeHandlerMayCallBackIntoTheRunner(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, nil)
	f.assign(t)
	var mu sync.Mutex
	var seen []string
	f.ag.Events.OnNotice = func(m string) {
		// What a UI does: read the rows it draws, from the notice itself.
		f.ag.SubAgentStates()
		f.ag.RunningSubAgents()
		mu.Lock()
		seen = append(seen, m)
		mu.Unlock()
	}
	f.ag.engineFault = func(op string) {
		if op == "sub-agent dispatch" {
			panic("the store exploded in " + op)
		}
	}
	done := make(chan struct{})
	go func() { defer close(done); f.ag.ScheduleSubAgents() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("ScheduleSubAgents deadlocked against a notice handler that called back in")
	}
	if len(f.ag.RunningSubAgents()) != 0 {
		t.Fatal("a dispatch whose store call panicked must leave no run")
	}
	// Nothing reserved was left behind, so a later schedule is not wedged.
	f.ag.subs.mu.Lock()
	pending := len(f.ag.subs.pending)
	f.ag.subs.mu.Unlock()
	if pending != 0 {
		t.Fatalf("abandoned candidate still reserved: %d pending", pending)
	}
	mu.Lock()
	n := len(seen)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("expected one detach notice, got %d: %q", n, seen)
	}
	if _, err := f.ag.Run(context.Background(), "still alive?"); err != nil {
		t.Fatalf("the primary must survive it: %v", err)
	}
}

func TestEnableDoesNotDispatchUntilStart(t *testing.T) {
	sub := &scriptedProvider{responses: []provider.ChatResponse{{Content: "done"}}}
	f := newSubFixture(t, sub, nil)
	f.assign(t)
	cws, _ := f.ag.Cfg.ValidCoworkers()
	f.ag.EnableSubAgents(cws, "http://primary:11434")
	select {
	case d := <-f.start:
		t.Fatalf("dispatched before StartSubAgents: %+v", d)
	case <-time.After(200 * time.Millisecond):
	}
	f.ag.StartSubAgents()
	wait(t, f.start, "start")
	wait(t, f.ends, "end")
	if rep := f.ag.SubAgentReport(); len(rep) != 1 || rep[0].Status != "done" {
		t.Fatalf("report: %+v", rep)
	}
}

// WaitSubAgents must not block on a run parked on ask_main: that is exactly
// the shape a headless run's own round-trip resolves (see Task 10), so
// counting it as busy would deadlock the run against itself.
func TestWaitSubAgentsDoesNotCountAnAskAsBusy(t *testing.T) {
	sub := &scriptedProvider{responses: []provider.ChatResponse{
		toolCall("ask_main", `{"question":"which tokenizer?"}`),
		{Content: "done as told"},
	}}
	f := newSubFixture(t, sub, nil)
	id := f.assign(t)
	f.ag.ScheduleSubAgents()
	wait(t, f.start, "start")
	wait(t, f.asks, "ask")
	if f.ag.subAgentsBusy() {
		t.Fatal("a run parked on ask_main must not count as busy")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	waited := make(chan struct{})
	go func() { f.ag.WaitSubAgents(ctx); close(waited) }()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("WaitSubAgents blocked on a run parked on an ask")
	}
	if ctx.Err() != nil {
		t.Fatal("WaitSubAgents returned only because ctx timed out, not because it saw the run as idle")
	}
	// Answering the ask makes the run genuinely busy again until it ends.
	if err := f.ag.ReplyAsk(id, "use the old one"); err != nil {
		t.Fatal(err)
	}
	wait(t, f.ends, "hand-back")
}

func TestSubAgentStatesAndGuidance(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, nil)
	states := f.ag.SubAgentStates()
	if len(states) != 1 || states[0].Name != "big" || !states[0].SubAgent || states[0].State != "idle" {
		t.Fatalf("states: %+v", states)
	}
	root := f.st.Plan("t", []string{"a"})
	_ = f.st.SetOwner(root+".1", "big", false)
	states = f.ag.SubAgentStates()
	if len(states) != 2 || states[1].Node != root+".1" || states[1].State != "waiting: no scope" {
		t.Fatalf("waiting row: %+v", states)
	}
	if !strings.Contains(f.ag.History.System.Content, subAgentGuidance) {
		t.Fatal("guidance missing with sub-agents enabled")
	}
	plain, _ := newTestAgent(t, &scriptedProvider{}, nil)
	if strings.Contains(plain.History.System.Content, "Sub-agents:") {
		t.Fatal("guidance present without sub-agents")
	}
}

// SubAgentStates is reached from RunningSubAgents, which
// bottomLine/compactBottomLine call on effectively every frame — so the
// DoingUnderID refinement it does after releasing subAgents.mu must go
// through the same engineDo fence as every other store call in this
// package: a panic walking a corrupted tree must detach the engine with
// one notice, not take the render goroutine down with it.
func TestSubAgentStatesSurvivesAPanickingDoingUnderID(t *testing.T) {
	sub := &scriptedProvider{responses: []provider.ChatResponse{
		toolCall("ask_main", `{"question":"which tokenizer?"}`),
	}}
	f := newSubFixture(t, sub, nil)
	id := f.assign(t)
	f.ag.ScheduleSubAgents()
	wait(t, f.start, "start")
	wait(t, f.asks, "ask") // parked on ask_main: a stable "working" row to read

	var notices []string
	f.ag.Events.OnNotice = func(m string) { notices = append(notices, m) }
	f.ag.engineFault = func(op string) {
		if op == "sub-agent at" {
			panic("the store exploded in " + op)
		}
	}

	states := f.ag.SubAgentStates()
	var row *SubAgentState
	for i := range states {
		if states[i].Node == id {
			row = &states[i]
		}
	}
	if row == nil {
		t.Fatalf("the running row is still expected back from a panicking refinement: %+v", states)
	}
	// The refinement panicked before it could run at all (engineFault fires
	// ahead of fn in engineDo), so At is left at its fallback: the
	// dispatched root id, set before the fenced call.
	if row.At != id {
		t.Fatalf("At must still fall back to the root id when the refinement panics: %q", row.At)
	}

	n := 0
	for _, m := range notices {
		if strings.Contains(m, "continuing without working memory") {
			n++
			if !strings.Contains(m, "engine: sub-agent at failed") {
				t.Fatalf("the wrong op is named in the detach notice: %q", m)
			}
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly one detach notice, got %d: %q", n, notices)
	}

	// The engine is detached for the rest of the session: a second read
	// must not panic again (no second notice) and must still return the
	// row, unrefined.
	states2 := f.ag.SubAgentStates()
	if len(states2) != len(states) {
		t.Fatalf("a detached engine must not change how many rows come back: %+v", states2)
	}
	if err := f.ag.ReplyAsk(id, "use the old one"); err != nil {
		t.Fatalf("the caller (and the run behind the row) must be unharmed: %v", err)
	}
	wait(t, f.ends, "hand-back")
}
