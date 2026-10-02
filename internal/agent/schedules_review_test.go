package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/schedule"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// Tests for the final whole-branch review of scheduled events (C1, I1–I7
// and the minors; .superpowers/sdd/2026-09-26-schedules/final-findings.md).

// settle waits for one scheduler pass that began after the call — so after
// whatever the test did just before it (an Advance, an edit, a drain) — to
// complete. Passes run one at a time on the loop goroutine: if pass m is
// in flight when this is called, the kick makes pass m+1 run after it.
func settle(t *testing.T, ag *Agent) {
	t.Helper()
	s := ag.sched
	m := s.passStarts.Load()
	s.kickLoop()
	deadline := time.Now().Add(2 * time.Second)
	for s.passDone.Load() < m+1 {
		if time.Now().After(deadline) {
			t.Fatalf("no scheduler pass completed (started %d, done %d)", s.passStarts.Load(), s.passDone.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func onDisk(ag *Agent) schedule.Approvals { return schedule.LoadApprovals(ag.sched.approvalsPath) }

// --- I1 ---------------------------------------------------------------------

func TestFiredTurnReceivesNoQueuedLines(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	n := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		mu.Lock()
		defer mu.Unlock()
		n++
		for _, m := range req.Messages {
			sent = append(sent, m.Content)
		}
		if n == 1 {
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "1", Name: "list_dir", Arguments: `{"path":"."}`}}}, nil
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}}
	ag, clock, _ := schedAgent(t, p)
	queueOne(t, ag, clock, mk("tick", "in 1m"), false)
	items := ag.DrainForTurn()
	ag.Enqueue("typed while the event runs")
	if _, _, err := ag.RunFull(context.Background(), items[0].Text); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	all := strings.Join(sent, "\n")
	mu.Unlock()
	if strings.Contains(all, "typed while the event runs") {
		t.Fatal("a person's line was delivered into a fired turn")
	}
	if got := ag.PeekItems(); len(got) != 1 || got[0].Text != "typed while the event runs" {
		t.Fatalf("the line waits for the leftover drain: %+v", got)
	}
}

// --- I2 ---------------------------------------------------------------------

func withdrawingApprover(ag *Agent, calls *atomic.Int32) {
	ag.Tools.ApproveCtx = func(ctx context.Context, action, detail string) bool {
		calls.Add(1)
		tools.MarkWithdrawn(ctx)
		return false
	}
}

func TestWithdrawnStartupPromptChangesNothing(t *testing.T) {
	ag, clock, dir := schedAgent(t, &scriptedProvider{})
	sc := mk("tick", "every 30m")
	seed(t, ag, sc, true)
	before, _ := os.ReadFile(filepath.Join(dir, ".be-code", "schedules.md"))
	var calls atomic.Int32
	withdrawingApprover(ag, &calls)
	ag.StartSchedules()
	if calls.Load() != 1 {
		t.Fatalf("asked once: %d", calls.Load())
	}
	after, _ := os.ReadFile(filepath.Join(dir, ".be-code", "schedules.md"))
	if string(before) != string(after) {
		t.Fatalf("a withdrawn prompt changed schedules.md:\n%s", after)
	}
	if got, _, _ := ag.sched.lookup("tick"); got.State != schedule.Active || !approvedNow(ag, sc.ID) {
		t.Fatalf("still active and approved on disk: %+v", got)
	}
	// ... but unconfirmed for this run: it does not fire.
	clock.Advance(31 * time.Minute)
	settle(t, ag)
	if ag.Pending() != 0 {
		t.Fatal("an unconfirmed schedule fired")
	}
	if got, _, _ := ag.sched.lookup("tick"); got.State != schedule.Active {
		t.Fatalf("and it was not paused either: %+v", got)
	}
	// A person's resume confirms it for this run.
	ag.Tools.ApproveCtx = nil
	ag.Tools.Approve = func(string, string) bool { return true }
	if _, err := ag.ScheduleAction("resume", "tick", "person"); err != nil {
		t.Fatal(err)
	}
	waitQueued(t, ag, 1)
}

func TestAnsweredNoStillPauses(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	sc := mk("tick", "every 30m")
	seed(t, ag, sc, true)
	ag.Tools.ApproveCtx = func(context.Context, string, string) bool { return false }
	ag.StartSchedules()
	if got, _, _ := ag.sched.lookup("tick"); got.State != schedule.Paused {
		t.Fatalf("an answered no pauses: %+v", got)
	}
}

func TestWithdrawnHeldTimerPromptKeepsThemHeld(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	ag.Tools.Approve = func(string, string) bool { return true }
	ag.StartSchedules()
	tm := mk("t", "in 1m")
	seed(t, ag, tm, false)
	ag.SetSession(&store.Session{ID: "s0"})
	clock.Advance(2 * time.Minute)
	ag.SetSession(&store.Session{ID: "s2", Timers: []schedule.Schedule{tm}})
	var calls atomic.Int32
	withdrawingApprover(ag, &calls)
	ag.ConfirmHeldTimers()
	if calls.Load() != 1 {
		t.Fatalf("asked once: %d", calls.Load())
	}
	settle(t, ag)
	if got, _, _ := ag.sched.lookup("t"); got.State != schedule.Active || ag.Pending() != 0 {
		t.Fatalf("still held, unchanged: %+v pending %d", got, ag.Pending())
	}
	ag.sched.mu.Lock()
	held := ag.sched.held[tm.ID] != 0
	ag.sched.mu.Unlock()
	if !held {
		t.Fatal("the timer is still held for a later confirmation")
	}
}

// --- I3 ---------------------------------------------------------------------

// Every pause path deletes the approval, so hand-editing `state: paused`
// back to `active` never re-activates it with its old allowance.
func TestEveryPauseRevokesApproval(t *testing.T) {
	paths := map[string]func(t *testing.T, ag *Agent, clock *schedule.FakeClock, sc schedule.Schedule){
		"person pause": func(t *testing.T, ag *Agent, _ *schedule.FakeClock, sc schedule.Schedule) {
			if _, err := ag.ScheduleAction("pause", sc.Name, "person"); err != nil {
				t.Fatal(err)
			}
		},
		"model pause": func(t *testing.T, ag *Agent, _ *schedule.FakeClock, sc schedule.Schedule) {
			if _, err := ag.ScheduleAction("pause", sc.Name, "agent"); err != nil {
				t.Fatal(err)
			}
		},
		"startup no": func(t *testing.T, ag *Agent, _ *schedule.FakeClock, sc schedule.Schedule) {
			ag.Tools.Approve = func(string, string) bool { return false }
			ag.StartSchedules()
		},
		"failures": func(t *testing.T, ag *Agent, _ *schedule.FakeClock, sc schedule.Schedule) {
			ag.sched.finish(sc.ID, "error: x")
			ag.sched.finish(sc.ID, "error: x")
		},
		"changed since approved": func(t *testing.T, ag *Agent, clock *schedule.FakeClock, sc schedule.Schedule) {
			p := ag.sched.projectPath
			b, _ := os.ReadFile(p)
			os.WriteFile(p, []byte(strings.Replace(string(b), "instruction: do "+sc.Name, "instruction: something else", 1)), 0o644)
			ag.sched.startLoop()
			clock.Advance(31 * time.Minute)
			settle(t, ag)
		},
	}
	for name, pause := range paths {
		t.Run(name, func(t *testing.T) {
			ag, clock, _ := schedAgent(t, &scriptedProvider{})
			sc := mk("tick", "every 30m")
			sc.CreatedBy = "agent" // the model pauses its own without asking
			seed(t, ag, sc, true)
			pause(t, ag, clock, sc)
			got, _, _ := ag.sched.lookup("tick")
			if got.State != schedule.Paused {
				t.Fatalf("paused: %+v", got)
			}
			if _, ok := onDisk(ag)[sc.ID]; ok {
				t.Fatal("the approval survived the pause")
			}
			// Re-activating by hand does not bring it back.
			p := ag.sched.projectPath
			b, _ := os.ReadFile(p)
			os.WriteFile(p, []byte(strings.Replace(string(b), "state: paused", "state: active", 1)), 0o644)
			ag.sched.startLoop()
			clock.Advance(2 * time.Hour)
			settle(t, ag)
			if ag.Pending() != 0 {
				t.Fatal("a hand re-activated schedule fired without approval")
			}
			if got, _, _ := ag.sched.lookup("tick"); got.State != schedule.Paused {
				t.Fatalf("it is paused again at its time: %+v", got)
			}
		})
	}
}

// --- I4 ---------------------------------------------------------------------

// twinAgent is a second session on schedAgent's workspace and HOME, with a
// clock of its own.
func twinAgent(t *testing.T, first *Agent, p provider.Provider) (*Agent, *schedule.FakeClock) {
	t.Helper()
	reg, err := tools.NewRegistry(first.Tools.Root, func(a, d string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.VerifyOnDone, cfg.CompatToolCalls, cfg.RepoMap = false, "never", false
	cfg.Schedules.PauseAfterFailures = 2
	ag := New(cfg, p, "test-model", reg, "")
	ag.Session = &store.Session{ID: "s2"}
	clock := schedule.NewFakeClock(t0)
	ag.EnableSchedules(clock)
	t.Cleanup(ag.StopSchedules)
	return ag, clock
}

func TestTwoSessionsRunAnOccurrenceOnce(t *testing.T) {
	var calls1, calls2 int
	ag1, clock1, _ := schedAgent(t, countingProvider(&calls1))
	sc := mk("tick", "every 30m")
	seed(t, ag1, sc, true)
	ag2, clock2 := twinAgent(t, ag1, countingProvider(&calls2))
	notes2 := noteSink(ag2)
	ag1.sched.startLoop()
	ag2.sched.startLoop()
	clock1.Advance(31 * time.Minute)
	clock2.Advance(31 * time.Minute)
	waitQueued(t, ag1, 1)
	waitQueued(t, ag2, 1)
	it1, it2 := ag1.DrainForTurn(), ag2.DrainForTurn()
	ag1.RunFull(context.Background(), it1[0].Text)
	ag2.RunFull(context.Background(), it2[0].Text)
	if calls1 != 1 || calls2 != 0 {
		t.Fatalf("one session runs it: %d %d", calls1, calls2)
	}
	if !strings.Contains(notes2(), "was not run: it already ran") {
		t.Fatalf("the other says why: %q", notes2())
	}
}

func TestClaimedOccurrenceIsRefused(t *testing.T) {
	calls := 0
	ag, clock, _ := schedAgent(t, countingProvider(&calls))
	notes := noteSink(ag)
	sc := mk("tick", "every 30m")
	queueOne(t, ag, clock, sc, true)
	due, _ := sc.NextDue()
	dir := filepath.Join(filepath.Dir(ag.sched.approvalsPath), "claims")
	os.MkdirAll(dir, 0o700)
	// Another session claimed it and has not saved its LastRun yet.
	if err := os.WriteFile(filepath.Join(dir, claimName(sc.ID, due)), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "old-1")
	os.WriteFile(stale, nil, 0o600)
	old := time.Now().Add(-48 * time.Hour)
	os.Chtimes(stale, old, old)
	ag.RunFull(context.Background(), ag.DrainForTurn()[0].Text)
	if calls != 0 || !strings.Contains(notes(), "it already ran in another session") {
		t.Fatalf("calls %d notes %q", calls, notes())
	}
	// A claim is made on the next run, and a day-old claim is pruned then.
	clock.Advance(30 * time.Minute)
	waitQueued(t, ag, 1)
	ag.RunFull(context.Background(), ag.DrainForTurn()[0].Text)
	if calls != 1 {
		t.Fatalf("the next occurrence runs: %d", calls)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("a day-old claim is pruned")
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 2 {
		t.Fatalf("the two live claims remain: %d", len(ents))
	}
}

func TestApprovalsMergedAcrossSessions(t *testing.T) {
	ag1, _, _ := schedAgent(t, &scriptedProvider{})
	ag2, _ := twinAgent(t, ag1, &scriptedProvider{})
	x, y := mk("x", "every 30m"), mk("y", "every 30m")
	seed(t, ag1, x, true)
	seed(t, ag2, y, true) // ag2 loaded its approvals before x existed
	got := onDisk(ag1)
	if got[x.ID] == "" || got[y.ID] == "" {
		t.Fatalf("both approvals kept: %v", got)
	}
}

// --- I5 ---------------------------------------------------------------------

func TestSuspendDoesNotDelayByHours(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	seed(t, ag, mk("later", "in 3h"), false)
	ag.sched.startLoop()
	settle(t, ag) // armed
	clock.Suspend(3*time.Hour + time.Minute)
	clock.Advance(time.Minute) // one minute awake: the capped timer fires
	waitQueued(t, ag, 1)
}

// --- I6 ---------------------------------------------------------------------

func TestFiredTurnRefusesSchedulePromptsAndOnlineConsent(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var asked []string
	ag.Tools.Approve = approver(true, &asked)
	mine := mk("mine", "every 30m")
	seed(t, ag, mine, true)
	paused := mk("idle", "every 30m")
	paused.CreatedBy = "agent"
	paused.State = schedule.Paused
	seed(t, ag, paused, true)

	ag.Tools.SetAllowance(nil, time.Minute)
	defer ag.Tools.ClearAllowance()
	if _, err := ag.AddSchedule(schedule.Request{Name: "n", When: "in 5m", Instruction: "x"}, "agent"); err == nil ||
		!strings.Contains(err.Error(), "during a scheduled event") {
		t.Fatalf("add refused: %v", err)
	}
	if _, err := ag.ScheduleAction("resume", "idle", "agent"); err == nil || !strings.Contains(err.Error(), "during a scheduled event") {
		t.Fatalf("resume refused: %v", err)
	}
	if _, err := ag.ScheduleAction("pause", "mine", "agent"); err == nil || !strings.Contains(err.Error(), "during a scheduled event") {
		t.Fatalf("pausing a person's schedule would ask: refused: %v", err)
	}
	if ag.consent(config.CoworkerConfig{Name: "cloud", Online: true}, ConsultRequest{Origin: "auto:tool"}) {
		t.Fatal("an online co-worker is declined during a fired turn")
	}
	ag.AllowCoworker("cloud")
	if ag.consent(config.CoworkerConfig{Name: "cloud", Online: true}, ConsultRequest{Origin: "tool"}) {
		t.Fatal("a session-wide yes is a shortcut a fired turn does not take")
	}
	if !ag.consent(config.CoworkerConfig{Name: "local"}, ConsultRequest{Origin: "tool"}) {
		t.Fatal("a local co-worker needs no consent")
	}
	if len(asked) != 0 {
		t.Fatalf("nothing asked: %v", asked)
	}
	// A person still can, from the terminal.
	if _, err := ag.AddSchedule(schedule.Request{Name: "n", When: "in 5m", Instruction: "x"}, "person"); err != nil {
		t.Fatal(err)
	}
}

// --- minors -----------------------------------------------------------------

// panicClock panics from Now once armed: the clock is read under s.mu in
// fireNow, begin and gateSchedules.
type panicClock struct {
	*schedule.FakeClock
	armed atomic.Bool
}

func (c *panicClock) Now() time.Time {
	if c.armed.Load() {
		panic("clock")
	}
	return c.FakeClock.Now()
}

func TestSchedulerLockReleasedOnPanic(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	pc := &panicClock{FakeClock: schedule.NewFakeClock(t0)}
	ag.sched.clock = pc
	sc := mk("tick", "every 30m")
	seed(t, ag, sc, true)
	ag.Tools.Approve = func(string, string) bool { return true }
	for name, f := range map[string]func(){
		"fireNow": func() { ag.fireNow(sc, true) },
		"gate":    func() { ag.gateSchedules(0, "x") },
		"begin":   func() { ag.sched.pending[sc.ID] = pendingEvent{}; ag.sched.begin(sc.ID) },
	} {
		func() {
			defer func() { recover() }()
			pc.armed.Store(true)
			f()
		}()
		pc.armed.Store(false)
		if !ag.sched.mu.TryLock() {
			t.Fatalf("%s: a panic left s.mu locked", name)
		}
		ag.sched.mu.Unlock()
	}
}

func TestAddScheduleValidatesTaskAndGrants(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var asked []string
	ag.Tools.Approve = approver(true, &asked)
	for _, req := range []schedule.Request{
		{Name: "a", When: "in 5m", Instruction: "x", Task: "3.2\nallow: write: ."},
		{Name: "a", When: "in 5m", Instruction: "x", Task: "three"},
		{Name: "a", When: "in 5m", Instruction: "x", Allow: []string{"shell: go test\nallow: write: ."}},
	} {
		if _, err := ag.AddSchedule(req, "person"); err == nil {
			t.Errorf("accepted %+v", req)
		}
	}
	if len(asked) != 0 {
		t.Fatalf("a refusal never asks: %v", asked)
	}
	if _, err := ag.AddSchedule(schedule.Request{Name: "a", When: "in 5m", Instruction: "x", Task: "3.2"}, "person"); err != nil {
		t.Fatal(err)
	}
}

// A pause in one session revokes the approval the other session holds too.
func TestPauseInOneSessionRevokesInTheOther(t *testing.T) {
	calls := 0
	ag1, _, _ := schedAgent(t, &scriptedProvider{})
	sc := mk("tick", "every 30m")
	seed(t, ag1, sc, true)
	ag2, clock2 := twinAgent(t, ag1, countingProvider(&calls))
	ag2.sched.startLoop()
	settle(t, ag2)
	// ag1 pauses it, then the file is hand-edited back to active.
	if _, err := ag1.ScheduleAction("pause", "tick", "person"); err != nil {
		t.Fatal(err)
	}
	p := ag1.sched.projectPath
	b, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(b), "state: paused", "state: active", 1)), 0o644)
	clock2.Advance(31 * time.Minute)
	settle(t, ag2)
	if ag2.Pending() != 0 {
		t.Fatal("the other session fired a schedule whose approval was revoked")
	}
	if got, _, _ := ag2.sched.lookup("tick"); got.State != schedule.Paused {
		t.Fatalf("paused at its time instead: %+v", got)
	}
}
