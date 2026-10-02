package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/schedule"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/subagent"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// A scheduled event's turn may not assign or scope a sub-agent: the
// sub-agent's own registry would ask through the session's shortcuts, with
// no deadline, while nobody is watching. The model is refused outright.
func TestFiredTurnTaskToolRefusesOwnerAndScope(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, nil)
	id := f.st.Plan("port the scanner", []string{"port internal/scan"}) + ".1"
	task := tools.NewTask(f.ag.TaskLedger())

	f.ag.Tools.SetAllowance(nil, time.Minute)
	res := task.Run(context.Background(), map[string]any{"action": "owner", "id": id, "owner": "big"})
	if !res.IsError || !strings.Contains(res.Content, "during a scheduled event") {
		t.Fatalf("owner during a fired turn: %+v", res)
	}
	res = task.Run(context.Background(), map[string]any{"action": "scope", "id": id, "paths": []any{"internal/scan"}})
	if !res.IsError || !strings.Contains(res.Content, "during a scheduled event") {
		t.Fatalf("scope during a fired turn: %+v", res)
	}
	n := f.st.Tree().Find(id)
	if n == nil || n.Owner != "" || len(n.Scope) != 0 {
		t.Fatalf("the tree changed: %+v", n)
	}
	f.ag.Tools.ClearAllowance()

	// Outside a fired turn the same calls go through.
	if res := task.Run(context.Background(), map[string]any{"action": "owner", "id": id, "owner": "big"}); res.IsError {
		t.Fatalf("owner outside a fired turn: %+v", res)
	}
}

// A person's own /task assign and /task scope (ag.AssignOwner/SetScope
// directly, as ui.TaskVerb calls them) are not refused during a fired turn,
// but the step they make ready waits for the turn to end.
func TestPersonAssignDuringFiredTurnIsHeldNotRefused(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{responses: []provider.ChatResponse{{Content: "ported"}}}, nil)
	id := f.st.Plan("port the scanner", []string{"port internal/scan"}) + ".1"

	f.ag.Tools.SetAllowance(nil, time.Minute)
	if err := f.ag.AssignOwner(id, "big", true); err != nil {
		t.Fatalf("a person's assign is not refused: %v", err)
	}
	if err := f.ag.SetScope(id, []string{"internal/scan"}); err != nil {
		t.Fatalf("a person's scope is not refused: %v", err)
	}
	f.ag.ScheduleSubAgents()
	if f.ag.subAgentsBusy() {
		t.Fatal("dispatched during a fired turn")
	}
	if started := f.ag.AllowSubAgentStart(); len(started) != 0 || !f.ag.SubAgentsHeld() {
		t.Fatalf("/agents start during a fired turn is held: started %v held %v", started, f.ag.SubAgentsHeld())
	}
	select {
	case d := <-f.start:
		t.Fatalf("dispatched during a fired turn: %+v", d)
	case <-time.After(100 * time.Millisecond):
	}
	f.ag.Tools.ClearAllowance()
	if f.ag.SubAgentsHeld() {
		t.Fatal("held after the turn")
	}
	f.ag.ScheduleSubAgents()
	if d := wait(t, f.start, "start after the turn"); d.Node != id {
		t.Fatalf("dispatch: %+v", d)
	}
}

// subConsent never raises a prompt during a fired turn, and a decline there
// is not remembered as the person's refusal.
func TestSubConsentDeclinesDuringFiredTurn(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, func(c *config.Config) { c.Coworkers[0].Online = true })
	var asked atomic.Int32
	f.ag.Tools.Approve = func(string, string) bool { asked.Add(1); return true }
	f.ag.Cfg.AutoApproveConsult = true // -y is a session shortcut too
	run := &subRun{cw: f.ag.subs.cws["big"], ctx: context.Background(),
		d: subagent.Dispatch{Node: "1.1", Owner: "big", Scope: []string{"internal/scan"}}}

	f.ag.Tools.SetAllowance(nil, time.Minute)
	if f.ag.subConsent(run) {
		t.Fatal("an online sub-agent is declined during a fired turn")
	}
	if asked.Load() != 0 {
		t.Fatal("subConsent asked during a fired turn")
	}
	f.ag.Tools.ClearAllowance()
	f.ag.Cfg.AutoApproveConsult = false
	if !f.ag.subConsent(run) || asked.Load() != 1 {
		t.Fatalf("after the turn the person is asked (asked %d)", asked.Load())
	}
}

// A step that becomes ready during a fired turn (owner and scope set before
// it, its predecessor closed by the turn's own model) is not dispatched
// until runFired returns, and then it is.
func TestStepReadyDuringFiredTurnDispatchesAfterIt(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var subCalled atomic.Bool
	sub := &funcProvider{fn: func(provider.ChatRequest) (*provider.ChatResponse, error) {
		subCalled.Store(true)
		return &provider.ChatResponse{Content: "ported"}, nil
	}}
	f := newSubFixture(t, nil, nil)
	CoworkerFactory = func(context.Context, *config.Config, config.CoworkerConfig) (provider.Provider, int, error) {
		return sub, 0, nil
	}
	root := f.st.Plan("port the scanner", []string{"read the old scanner", "port internal/scan"})
	first, second := root+".1", root+".2"
	if err := f.st.SetOwner(second, "big", false); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetScope(second, []string{"internal/scan"}); err != nil {
		t.Fatal(err)
	}
	f.ag.Tools.AddTool(tools.NewTask(f.ag.TaskLedger()))

	var during atomic.Bool
	calls := 0
	f.ag.Provider = &funcProvider{fn: func(provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls == 1 {
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "1", Name: "task",
				Arguments: `{"action":"status","id":"` + first + `","status":"done"}`}}}, nil
		}
		if f.ag.subAgentsBusy() || subCalled.Load() || len(f.start) > 0 || len(f.ends) > 0 {
			during.Store(true)
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}}

	if f.ag.Session == nil {
		f.ag.Session = &store.Session{ID: "s1"}
	}
	clock := schedule.NewFakeClock(t0)
	f.ag.EnableSchedules(clock)
	t.Cleanup(f.ag.StopSchedules)
	sc := mk("tick", "in 1m")
	seed(t, f.ag, sc, false)
	f.ag.sched.startLoop()
	clock.Advance(2 * time.Minute)
	waitQueued(t, f.ag, 1)
	items := f.ag.DrainForTurn()
	if _, _, err := f.ag.RunFull(context.Background(), items[0].Text); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("the fired turn did not run (calls %d)", calls)
	}
	if during.Load() {
		t.Fatal("a sub-agent was dispatched during the fired turn")
	}
	if d := wait(t, f.start, "dispatch after the fired turn"); d.Node != second {
		t.Fatalf("dispatch: %+v", d)
	}
}

// The closing schedule runs on a panicking fired turn too, and does not
// swallow the panic.
func TestHeldSubAgentWorkDispatchesAfterPanickedFiredTurn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	f := newSubFixture(t, &scriptedProvider{responses: []provider.ChatResponse{{Content: "ported"}}}, nil)
	id := f.assign(t) // ready, but nothing has scheduled it yet
	f.ag.Provider = &panicOnceProvider{}
	if f.ag.Session == nil {
		f.ag.Session = &store.Session{ID: "s1"}
	}
	clock := schedule.NewFakeClock(t0)
	f.ag.EnableSchedules(clock)
	t.Cleanup(f.ag.StopSchedules)
	queueOne(t, f.ag, clock, mk("boom", "in 1m"), false)
	panicked := false
	func() {
		defer func() { panicked = recover() != nil }()
		f.ag.RunFull(context.Background(), f.ag.DrainForTurn()[0].Text)
	}()
	if !panicked {
		t.Fatal("the turn's panic propagates")
	}
	if d := wait(t, f.start, "dispatch after the panicked turn"); d.Node != id {
		t.Fatalf("dispatch: %+v", d)
	}
}
