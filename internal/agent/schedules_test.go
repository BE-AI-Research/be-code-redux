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
	"github.com/brown-enterprises/be-code/internal/schedule"
	"github.com/brown-enterprises/be-code/internal/store"
)

// withEngine (engine_test.go) attaches a working-memory store under t.TempDir().

var t0 = time.Date(2026, 9, 26, 10, 0, 0, 0, time.Local)

// schedAgent is a test agent with a scheduler on a fake clock, its approval
// store under the test's own temp dir.
func schedAgent(t *testing.T, p provider.Provider) (*Agent, *schedule.FakeClock, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // the approvals file goes under ~/.be-code
	ag, dir := newTestAgent(t, p, func(c *config.Config) { c.Schedules.PauseAfterFailures = 2 })
	ag.Session = &store.Session{ID: "s1"}
	clock := schedule.NewFakeClock(t0)
	ag.EnableSchedules(clock)
	t.Cleanup(ag.StopSchedules)
	return ag, clock, dir
}

// seed puts an approved schedule straight into a store.
func seed(t *testing.T, ag *Agent, sc schedule.Schedule, project bool) {
	t.Helper()
	s := ag.sched
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reloadLocked()
	if err := s.putLocked(sc, project); err != nil {
		t.Fatal(err)
	}
	s.approveLocked(sc)
}

func mk(name, when string) schedule.Schedule {
	return schedule.Schedule{ID: schedule.NewID(), Name: name, When: when, Instruction: "do " + name,
		State: schedule.Active, CreatedBy: "person", Created: t0}
}

// waitQueued polls (the loop runs on its own goroutine) for n scheduled items.
func waitQueued(t *testing.T, ag *Agent, n int) []InboxItem {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var got []InboxItem
		for _, it := range ag.PeekItems() {
			if it.Scheduled != "" {
				got = append(got, it)
			}
		}
		if len(got) == n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("want %d scheduled items, have %d", n, len(got))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFiresInDueOrderAndOnlyOnce(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	seed(t, ag, mk("later", "in 20m"), false)
	seed(t, ag, mk("sooner", "in 10m"), false)
	ag.sched.startLoop()
	clock.Advance(25 * time.Minute)
	items := waitQueued(t, ag, 2)
	if items[0].ScheduleName != "sooner" || items[1].ScheduleName != "later" {
		t.Fatalf("order %s, %s", items[0].ScheduleName, items[1].ScheduleName)
	}
	if !items[0].Harness {
		t.Fatal("a scheduled item is not typed by a person")
	}
	if !strings.HasPrefix(items[0].Text, `[Scheduled event "sooner" — in 10m, set by you 2026-09-26]`) {
		t.Fatalf("header: %q", items[0].Text)
	}
	clock.Advance(time.Minute)
	ag.sched.kickLoop()
	time.Sleep(50 * time.Millisecond)
	waitQueued(t, ag, 2) // not queued a second time
}

func TestScheduledItemsNeverDeliveredMidRun(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	ag.EnqueueScheduled("id1", "x", "scheduled text")
	ag.Enqueue("typed text")
	if got := ag.DrainInbox(); len(got) != 1 || got[0] != "typed text" {
		t.Fatalf("mid-run delivery takes only unscheduled lines: %v", got)
	}
	if ag.Pending() != 1 {
		t.Fatal("the scheduled item stays queued")
	}
}

func TestDrainForTurnPersonFirstThenOneScheduled(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	ag.EnqueueScheduled("id1", "a", "sched a")
	ag.EnqueueScheduled("id2", "b", "sched b")
	ag.Enqueue("typed")
	first := ag.DrainForTurn()
	if len(first) != 1 || first[0].Text != "typed" {
		t.Fatalf("person's line first, alone: %+v", first)
	}
	second := ag.DrainForTurn()
	if len(second) != 1 || second[0].Scheduled != "id1" {
		t.Fatalf("then one scheduled event: %+v", second)
	}
	if ag.Pending() != 1 {
		t.Fatal("the other waits for its own turn")
	}
}

func TestFiredTurnRunsUnderAllowanceAndRecordsOutcome(t *testing.T) {
	p := &scriptedProvider{responses: []provider.ChatResponse{
		{ToolCalls: []provider.ToolCall{{ID: "1", Name: "write_file", Arguments: `{"path":"docs/r.md","content":"x"}`}}},
		{ToolCalls: []provider.ToolCall{{ID: "2", Name: "write_file", Arguments: `{"path":"src/y.go","content":"x"}`}}},
		{Content: "done"},
	}}
	ag, clock, dir := schedAgent(t, p)
	var asked []string
	ag.Tools.Approve = func(action, detail string) bool { asked = append(asked, action); return false }
	ag.Tools.ApproveWrites = true
	sc := mk("report", "in 1m")
	sc.Allow = schedule.Allowance{{Kind: "write", Value: "docs"}}
	seed(t, ag, sc, false)
	ag.sched.startLoop()
	clock.Advance(2 * time.Minute)
	waitQueued(t, ag, 1)
	items := ag.DrainForTurn()
	if _, _, err := ag.RunFull(context.Background(), items[0].Text); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "docs", "r.md")); err != nil {
		t.Fatal("covered write landed without a prompt")
	}
	if strings.Join(asked, ",") != "file_write" {
		t.Fatalf("only the uncovered write asked: %v", asked)
	}
	if refused, _ := ag.Tools.ClearAllowance(); refused != "" {
		t.Fatal("the allowance is cleared when the turn ends")
	}
	got, _, _ := ag.sched.lookup(sc.ID)
	if got.LastOutcome != "refused: file_write" || got.State != schedule.Done || got.LastRun.IsZero() {
		t.Fatalf("outcome: %+v", got)
	}
	if len(ag.Session.Timers) != 1 || ag.Session.Timers[0].State != schedule.Done {
		t.Fatalf("timers mirrored into the session: %+v", ag.Session.Timers)
	}
}

func TestOrdinaryRequestGetsNoAllowance(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	ag.EnqueueScheduled("id1", "x", "scheduled")
	ag.DrainForTurn() // arms "scheduled"
	// A different request (a person typed something else) must not inherit it.
	if f := ag.takeFiring("something else"); f != nil {
		t.Fatal("the arm matches only its own text")
	}
	if f := ag.takeFiring("scheduled"); f != nil {
		t.Fatal("and is spent by the first take")
	}
}

func TestMissedOneOffRunsOnce(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	seed(t, ag, mk("missed", "in 5m"), false)
	clock.Set(t0.Add(3 * time.Hour)) // the session was closed past its time
	ag.sched.startLoop()
	waitQueued(t, ag, 1)
	ag.RunFull(context.Background(), ag.DrainForTurn()[0].Text)
	clock.Advance(time.Hour)
	ag.sched.kickLoop()
	time.Sleep(50 * time.Millisecond)
	if ag.Pending() != 0 {
		t.Fatal("a one-off runs once")
	}
}

func TestMissedRecurringRunsOnceNotPerMissedTime(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	seed(t, ag, mk("half-hourly", "every 30m"), true)
	clock.Set(t0.Add(5 * time.Hour)) // ten missed times
	ag.sched.startLoop()
	waitQueued(t, ag, 1)
	time.Sleep(50 * time.Millisecond)
	waitQueued(t, ag, 1)
}

func TestDroppedQueuedEventFiresAgain(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	seed(t, ag, mk("tick", "every 30m"), true)
	ag.sched.startLoop()
	clock.Advance(31 * time.Minute)
	waitQueued(t, ag, 1)
	ag.Remove(0) // a person dropped it from the queue popup
	clock.Advance(30 * time.Minute)
	waitQueued(t, ag, 1)
}

func TestHandEditedSchedulePausesAtFire(t *testing.T) {
	ag, clock, dir := schedAgent(t, &scriptedProvider{})
	sc := mk("nightly", "every 30m")
	seed(t, ag, sc, true)
	p := filepath.Join(dir, ".be-code", "schedules.md")
	b, _ := os.ReadFile(p)
	edited := strings.Replace(string(b), "state: active", "allow: write: .\nstate: active", 1)
	os.WriteFile(p, []byte(edited), 0o644)
	// The notice is raised on the scheduler's goroutine: guard the slice.
	var mu sync.Mutex
	var notes []string
	ag.Events.OnNotice = func(m string) { mu.Lock(); notes = append(notes, m); mu.Unlock() }
	ag.sched.startLoop()
	clock.Advance(31 * time.Minute)
	time.Sleep(100 * time.Millisecond)
	if ag.Pending() != 0 {
		t.Fatal("an edited schedule must not fire")
	}
	got, _, _ := ag.sched.lookup(sc.ID)
	mu.Lock()
	defer mu.Unlock()
	if got.State != schedule.Paused || !strings.Contains(strings.Join(notes, "\n"), "changed since approved") {
		t.Fatalf("paused with a notice: %+v %v", got.State, notes)
	}
}

func TestPauseAfterFailures(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	sc := mk("flaky", "every 30m")
	seed(t, ag, sc, true)
	ag.sched.finish(sc.ID, "checks failed")
	if got, _, _ := ag.sched.lookup(sc.ID); got.State != schedule.Active || got.Failures != 1 {
		t.Fatalf("one failure: %+v", got)
	}
	ag.sched.finish(sc.ID, "checks failed")
	if got, _, _ := ag.sched.lookup(sc.ID); got.State != schedule.Paused {
		t.Fatalf("paused after 2 (config): %+v", got)
	}
}

func TestOkResetsFailures(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	sc := mk("flaky", "every 30m")
	seed(t, ag, sc, true)
	ag.sched.finish(sc.ID, "checks failed")
	ag.sched.finish(sc.ID, "ok")
	if got, _, _ := ag.sched.lookup(sc.ID); got.Failures != 0 {
		t.Fatalf("%+v", got)
	}
}

func TestTaskLinkSetsDoing(t *testing.T) {
	var st *engine.Store
	var root, doingAtCall string
	p := &funcProvider{fn: func(provider.ChatRequest) (*provider.ChatResponse, error) {
		doingAtCall = st.DoingUnderID(root)
		return &provider.ChatResponse{Content: "done"}, nil
	}}
	ag, clock, _ := schedAgent(t, p)
	st = withEngine(t, ag)
	root = st.Plan("nightly work", []string{"step a", "step b"})
	sc := mk("linked", "in 1m")
	sc.Task = root + ".2"
	seed(t, ag, sc, false)
	ag.sched.startLoop()
	clock.Advance(2 * time.Minute)
	waitQueued(t, ag, 1)
	if _, _, err := ag.RunFull(context.Background(), ag.DrainForTurn()[0].Text); err != nil {
		t.Fatal(err)
	}
	if doingAtCall != root+".2" {
		t.Fatalf("the linked step was doing when the model was called: %q", doingAtCall)
	}
}

func TestMissingTaskLinkStillRuns(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	withEngine(t, ag)
	var notes []string
	ag.Events.OnNotice = func(m string) { notes = append(notes, m) }
	sc := mk("orphan", "in 1m")
	sc.Task = "9.9"
	seed(t, ag, sc, false)
	ag.sched.startLoop()
	clock.Advance(2 * time.Minute)
	waitQueued(t, ag, 1)
	if _, _, err := ag.RunFull(context.Background(), ag.DrainForTurn()[0].Text); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "running without it") {
		t.Fatalf("notice: %v", notes)
	}
	if got, _, _ := ag.sched.lookup(sc.ID); got.LastOutcome != "ok" {
		t.Fatalf("outcome %q", got.LastOutcome)
	}
}

func TestMaxRuntimeCancelsTheTurn(t *testing.T) {
	ag, clock, _ := schedAgent(t, ctxBlockProvider{})
	sc := mk("slow", "in 1m")
	sc.MaxRuntime = 30 * time.Millisecond
	seed(t, ag, sc, false)
	ag.sched.startLoop()
	clock.Advance(2 * time.Minute)
	waitQueued(t, ag, 1)
	_, _, err := ag.RunFull(context.Background(), ag.DrainForTurn()[0].Text)
	if err == nil || !strings.Contains(err.Error(), "max runtime") {
		t.Fatalf("err %v", err)
	}
	if got, _, _ := ag.sched.lookup(sc.ID); got.LastOutcome != "timed out" {
		t.Fatalf("outcome %q", got.LastOutcome)
	}
}

// ctxBlockProvider blocks every call until its context ends.
type ctxBlockProvider struct{}

func (ctxBlockProvider) Name() string { return "block" }
func (ctxBlockProvider) Chat(ctx context.Context, _ provider.ChatRequest, _ provider.StreamFunc) (*provider.ChatResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (ctxBlockProvider) ListModels(context.Context) ([]provider.ModelInfo, error) { return nil, nil }
func (ctxBlockProvider) Ping(context.Context) (string, error)                     { return "ok", nil }
