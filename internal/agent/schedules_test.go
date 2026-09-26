package agent

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
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

// ---- fix round 1: run-time checks, dropped events, durability ----

// noteSink collects notices from any goroutine.
func noteSink(ag *Agent) func() string {
	var mu sync.Mutex
	var notes []string
	ag.Events.OnNotice = func(m string) { mu.Lock(); notes = append(notes, m); mu.Unlock() }
	return func() string { mu.Lock(); defer mu.Unlock(); return strings.Join(notes, "\n") }
}

// countingProvider answers "done" and counts calls.
func countingProvider(calls *int) *funcProvider {
	return &funcProvider{fn: func(provider.ChatRequest) (*provider.ChatResponse, error) {
		*calls++
		return &provider.ChatResponse{Content: "done"}, nil
	}}
}

// queueOne seeds sc, starts the loop, moves past its time and waits for it.
func queueOne(t *testing.T, ag *Agent, clock *schedule.FakeClock, sc schedule.Schedule, project bool) {
	t.Helper()
	seed(t, ag, sc, project)
	ag.sched.startLoop()
	clock.Advance(31 * time.Minute)
	waitQueued(t, ag, 1)
}

func TestEditAfterQueueDoesNotRun(t *testing.T) {
	calls := 0
	ag, clock, dir := schedAgent(t, countingProvider(&calls))
	notes := noteSink(ag)
	sc := mk("nightly", "every 30m")
	queueOne(t, ag, clock, sc, true)
	// A grant forged into the file after the event was queued.
	p := filepath.Join(dir, ".be-code", "schedules.md")
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "state: active", "allow: write: .\nstate: active", 1)), 0o644)
	if _, _, err := ag.RunFull(context.Background(), ag.DrainForTurn()[0].Text); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("an event whose schedule changed since approval must not run")
	}
	got, _, _ := ag.sched.lookup(sc.ID)
	n := notes()
	if got.State != schedule.Paused || !strings.Contains(n, "changed since approved") ||
		!strings.Contains(n, `scheduled event "nightly" was not run: it changed since it was approved`) {
		t.Fatalf("paused with notices: %v %q", got.State, n)
	}
}

func setState(t *testing.T, ag *Agent, id string, st schedule.State) {
	t.Helper()
	s := ag.sched
	s.mu.Lock()
	defer s.unlock()
	sc, ok, project := s.findLocked(id)
	if !ok {
		t.Fatal("no such schedule")
	}
	sc.State = st
	s.putLocked(sc, project)
}

func TestPausedAfterQueueDoesNotRun(t *testing.T) {
	calls := 0
	ag, clock, _ := schedAgent(t, countingProvider(&calls))
	notes := noteSink(ag)
	sc := mk("tick", "every 30m")
	queueOne(t, ag, clock, sc, true)
	setState(t, ag, sc.ID, schedule.Paused)
	ag.RunFull(context.Background(), ag.DrainForTurn()[0].Text)
	if calls != 0 || !strings.Contains(notes(), "was not run: it is paused") {
		t.Fatalf("calls %d, notices %q", calls, notes())
	}
}

func TestRemovedAfterQueueDoesNotRun(t *testing.T) {
	calls := 0
	ag, clock, _ := schedAgent(t, countingProvider(&calls))
	notes := noteSink(ag)
	sc := mk("gone", "in 1m")
	queueOne(t, ag, clock, sc, false)
	ag.sched.mu.Lock()
	ag.sched.removeLocked(sc.ID)
	ag.sched.unlock()
	ag.RunFull(context.Background(), ag.DrainForTurn()[0].Text)
	if calls != 0 || !strings.Contains(notes(), "was not run: it no longer exists") {
		t.Fatalf("calls %d, notices %q", calls, notes())
	}
}

func TestWakeBetweenDrainAndRunFiresOnce(t *testing.T) {
	calls := 0
	ag, clock, _ := schedAgent(t, countingProvider(&calls))
	sc := mk("once", "in 1m")
	queueOne(t, ag, clock, sc, false)
	items := ag.DrainForTurn()
	ag.sched.kickLoop() // a wake in the drain→begin window
	time.Sleep(50 * time.Millisecond)
	if ag.Pending() != 0 {
		t.Fatal("re-queued while the drained event had not begun")
	}
	if _, _, err := ag.RunFull(context.Background(), items[0].Text); err != nil {
		t.Fatal(err)
	}
	ag.sched.kickLoop()
	time.Sleep(50 * time.Millisecond)
	got, _, _ := ag.sched.lookup(sc.ID)
	if calls != 1 || ag.Pending() != 0 || got.State != schedule.Done {
		t.Fatalf("calls %d pending %d state %s", calls, ag.Pending(), got.State)
	}
}

func TestDroppedOneOffIsDone(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	sc := mk("once", "in 1m")
	queueOne(t, ag, clock, sc, false)
	ag.Remove(0)
	ag.sched.kickLoop()
	time.Sleep(50 * time.Millisecond)
	got, _, _ := ag.sched.lookup(sc.ID)
	if ag.Pending() != 0 || got.State != schedule.Done || got.LastOutcome != "dropped from the queue" {
		t.Fatalf("pending %d %+v", ag.Pending(), got)
	}
}

func TestSaveFailureDoesNotRefire(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory the test cannot write")
	}
	ag, clock, dir := schedAgent(t, &scriptedProvider{})
	sc := mk("tick", "every 30m")
	queueOne(t, ag, clock, sc, true)
	pdir := filepath.Join(dir, ".be-code")
	if err := os.Chmod(pdir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(pdir, 0o755) })
	ag.RunFull(context.Background(), ag.DrainForTurn()[0].Text)
	clock.Advance(5 * time.Minute)
	ag.sched.kickLoop()
	time.Sleep(50 * time.Millisecond)
	if ag.Pending() != 0 {
		t.Fatal("a run whose LastRun could not be saved fired again at once")
	}
}

func TestTaskLinkToClosedNodeIsNotReopened(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	st := withEngine(t, ag)
	notes := noteSink(ag)
	root := st.Plan("work", []string{"a"})
	st.SetStatus(root+".1", engine.StatusDone, "")
	sc := mk("linked", "in 1m")
	sc.Task = root + ".1"
	queueOne(t, ag, clock, sc, false)
	ag.RunFull(context.Background(), ag.DrainForTurn()[0].Text)
	if s, _ := st.NodeStatus(root + ".1"); s != engine.StatusDone {
		t.Fatalf("a done node was reopened: %s", s)
	}
	if !strings.Contains(notes(), "task "+root+".1 is done; running without it") {
		t.Fatalf("notice %q", notes())
	}
}

// panicOnceProvider panics on its first call, then writes docs/r.md, then ends.
type panicOnceProvider struct{ n int }

func (p *panicOnceProvider) Name() string { return "panic" }
func (p *panicOnceProvider) Chat(context.Context, provider.ChatRequest, provider.StreamFunc) (*provider.ChatResponse, error) {
	p.n++
	switch p.n {
	case 1:
		panic("boom\nsecond line")
	case 2:
		return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "1", Name: "write_file", Arguments: `{"path":"docs/r.md","content":"x"}`}}}, nil
	}
	return &provider.ChatResponse{Content: "done"}, nil
}
func (p *panicOnceProvider) ListModels(context.Context) ([]provider.ModelInfo, error) {
	return nil, nil
}
func (p *panicOnceProvider) Ping(context.Context) (string, error) { return "ok", nil }

func TestPanicInFiredTurnClearsAllowance(t *testing.T) {
	ag, clock, _ := schedAgent(t, &panicOnceProvider{})
	var asked []string
	ag.Tools.Approve = func(action, detail string) bool { asked = append(asked, action); return false }
	ag.Tools.ApproveWrites = true
	sc := mk("boom", "in 1m")
	sc.Allow = schedule.Allowance{{Kind: "write", Value: "docs"}}
	queueOne(t, ag, clock, sc, false)
	func() {
		defer func() { recover() }()
		ag.RunFull(context.Background(), ag.DrainForTurn()[0].Text)
		t.Fatal("the panic propagates")
	}()
	if got, _, _ := ag.sched.lookup(sc.ID); got.LastOutcome != "error: boom…" {
		t.Fatalf("outcome %q", got.LastOutcome)
	}
	ag.RunFull(context.Background(), "an ordinary request")
	if strings.Join(asked, ",") != "file_write" {
		t.Fatalf("the allowance outlived the panicked turn: %v", asked)
	}
}

func TestTypedRequestAfterScheduledDrainGetsNoAllowance(t *testing.T) {
	p := &scriptedProvider{responses: []provider.ChatResponse{
		{ToolCalls: []provider.ToolCall{{ID: "1", Name: "write_file", Arguments: `{"path":"docs/r.md","content":"x"}`}}},
		{Content: "done"},
	}}
	ag, clock, _ := schedAgent(t, p)
	var asked []string
	ag.Tools.Approve = func(action, detail string) bool { asked = append(asked, action); return false }
	ag.Tools.ApproveWrites = true
	sc := mk("report", "in 1m")
	sc.Allow = schedule.Allowance{{Kind: "write", Value: "docs"}}
	queueOne(t, ag, clock, sc, false)
	ag.DrainForTurn() // armed, then a person types something else instead
	ag.RunFull(context.Background(), "typed by a person")
	if strings.Join(asked, ",") != "file_write" {
		t.Fatalf("a typed request ran under the schedule's allowance: %v", asked)
	}
	if got, _, _ := ag.sched.lookup(sc.ID); !got.LastRun.IsZero() {
		t.Fatal("the schedule did not run")
	}
}

func TestStaleArmClearedByDrain(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	ag.EnqueueScheduled("id1", "x", "same text")
	ag.DrainForTurn()
	ag.DrainItems()
	if ag.takeFiring("same text") != nil {
		t.Fatal("a drain clears a stale arm")
	}
}

func TestStartAfterStopRunsNoPass(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	seed(t, ag, mk("due", "in 1m"), false)
	clock.Advance(2 * time.Minute)
	ag.StopSchedules()
	ag.sched.startLoop()
	time.Sleep(50 * time.Millisecond)
	if ag.Pending() != 0 {
		t.Fatal("a stopped scheduler ran a pass")
	}
}

func TestFinishKeepsStateOnUnparseableSpec(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	sc := mk("odd", "whenever you like")
	seed(t, ag, sc, false)
	ag.sched.finish(sc.ID, "ok")
	if got, _, _ := ag.sched.lookup(sc.ID); got.State != schedule.Active || got.LastOutcome != "ok" {
		t.Fatalf("%+v", got)
	}
}

func TestNewSessionReplacesTimers(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	sc := mk("once", "in 1h")
	seed(t, ag, sc, false)
	ag.SetSession(&store.Session{ID: "s2"}) // /clear
	if _, ok, _ := ag.sched.lookup(sc.ID); ok {
		t.Fatal("a fresh session carried the old session's timers")
	}
	ag.Resume(&store.Session{ID: "s3", Timers: []schedule.Schedule{sc}})
	if _, ok, _ := ag.sched.lookup(sc.ID); !ok {
		t.Fatal("a resumed session's timers are loaded")
	}
}

func TestOneOffLastRunSavedBeforeTheRun(t *testing.T) {
	var saved []schedule.Schedule
	p := &funcProvider{fn: func(provider.ChatRequest) (*provider.ChatResponse, error) {
		if s, err := store.Load("s1"); err == nil {
			saved = s.Timers
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}}
	ag, clock, _ := schedAgent(t, p)
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "earlier"})
	sc := mk("once", "in 1m")
	queueOne(t, ag, clock, sc, false)
	ag.RunFull(context.Background(), ag.DrainForTurn()[0].Text)
	if len(saved) != 1 || saved[0].LastRun.IsZero() {
		t.Fatalf("the session file did not carry LastRun when the model was called: %+v", saved)
	}
}
