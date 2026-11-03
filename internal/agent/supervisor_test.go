package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
)

// stallClock is the supervisor's fake clock: set once before anything runs,
// advanced by the test, read by every goroutine that stamps a run.
type stallClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stallClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stallClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type stallSetup struct {
	f       *subFixture
	clk     *stallClock
	id      string
	run     *subRun
	mu      sync.Mutex
	notices []string
}

func (s *stallSetup) noticed() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.notices...)
}

// stallJob dispatches one job whose provider parks, on a fake clock, with
// stall_after 180 s and max_runtime 1800 s unless mut says otherwise.
func stallJob(t *testing.T, mut func(*config.Config)) *stallSetup {
	t.Helper()
	f := newSubFixture(t, &scriptedProvider{}, func(c *config.Config) {
		c.SubAgents.StallAfter = 180
		c.SubAgents.MaxRuntime = 1800
		if mut != nil {
			mut(c)
		}
	})
	bp := parkJobs(t)
	s := &stallSetup{f: f, clk: &stallClock{t: time.Now()}}
	f.ag.Events.OnNotice = func(m string) {
		s.mu.Lock()
		s.notices = append(s.notices, m)
		s.mu.Unlock()
	}
	f.ag.subs.sup.now = s.clk.Now
	f.st.Plan("work", nil)
	s.id = dispatchJob(t, f, "port it", "internal/scan")
	wait(t, bp.entered, "the job's request")
	f.ag.subs.mu.Lock()
	s.run = f.ag.subs.runs[s.id]
	f.ag.subs.mu.Unlock()
	return s
}

func (s *stallSetup) stalls() (int, bool) {
	s.f.ag.subs.mu.Lock()
	defer s.f.ag.subs.mu.Unlock()
	return s.run.stalls, s.run.stalled
}

func (s *stallSetup) stallOnce(t *testing.T) {
	t.Helper()
	s.clk.Advance(181 * time.Second)
	s.f.ag.subs.sup.sweep()
}

func TestFirstStallQueuesOneLineAndCountsOne(t *testing.T) {
	s := stallJob(t, nil)
	s.f.ag.subs.sup.sweep()
	if s.f.ag.Pending() != 0 {
		t.Fatalf("a fresh job stalled: %q", s.f.ag.Peek())
	}
	s.stallOnce(t)
	q := s.f.ag.Peek()
	if len(q) != 1 {
		t.Fatalf("queued: %q", q)
	}
	want := []string{"job " + s.id + " (big) has been quiet for 3m", "jobs extend " + s.id + " to give it more time",
		"jobs stop " + s.id + " to end it", "jobs reply " + s.id + " <text> to steer it", "jobs status " + s.id + " to look."}
	for _, w := range want {
		if !strings.Contains(q[0], w) {
			t.Fatalf("stall line %q lacks %q", q[0], w)
		}
	}
	if n, stalled := s.stalls(); n != 1 || !stalled {
		t.Fatalf("stalls %d stalled %v", n, stalled)
	}
}

func TestSupervisorQueuesOneStallLineAtATime(t *testing.T) {
	s := stallJob(t, nil)
	s.stallOnce(t)
	s.stallOnce(t)
	s.stallOnce(t)
	if q := s.f.ag.Peek(); len(q) != 1 {
		t.Fatalf("queued: %q", q)
	}
	if n, _ := s.stalls(); n != 1 {
		t.Fatalf("an unread notice was counted again: stalls %d", n)
	}
}

func TestExtendResetsTheClockNotTheCount(t *testing.T) {
	s := stallJob(t, nil)
	s.stallOnce(t)
	s.f.ag.DrainInbox()
	if err := s.f.ag.JobExtend(s.id); err != nil {
		t.Fatal(err)
	}
	if n, stalled := s.stalls(); n != 1 || stalled {
		t.Fatalf("after extend: stalls %d stalled %v", n, stalled)
	}
	s.clk.Advance(90 * time.Second)
	s.f.ag.subs.sup.sweep()
	if s.f.ag.Pending() != 0 {
		t.Fatalf("stalled before stall_after: %q", s.f.ag.Peek())
	}
	s.clk.Advance(91 * time.Second)
	s.f.ag.subs.sup.sweep()
	q := s.f.ag.Peek()
	if len(q) != 1 || !strings.Contains(q[0], "stalled again after your extend/reply") {
		t.Fatalf("queued: %q", q)
	}
	if n, _ := s.stalls(); n != 2 {
		t.Fatalf("stalls %d", n)
	}
}

func TestIgnoredNoticeStallsAgainLater(t *testing.T) {
	s := stallJob(t, nil)
	s.stallOnce(t)
	s.f.ag.DrainInbox()
	s.clk.Advance(60 * time.Second)
	s.f.ag.subs.sup.sweep()
	if s.f.ag.Pending() != 0 {
		t.Fatalf("stalled again at once: %q", s.f.ag.Peek())
	}
	s.clk.Advance(121 * time.Second)
	s.f.ag.subs.sup.sweep()
	q := s.f.ag.Peek()
	if len(q) != 1 || !strings.Contains(q[0], "stalled again; you did not act on the first notice") {
		t.Fatalf("queued: %q", q)
	}
}

func TestProgressResetsTheCount(t *testing.T) {
	s := stallJob(t, nil)
	s.stallOnce(t)
	s.f.ag.noteActivity(s.run, "read_file", "x.go")
	if n, stalled := s.stalls(); n != 0 || stalled {
		t.Fatalf("after progress: stalls %d stalled %v", n, stalled)
	}
	if s.f.ag.Pending() != 0 {
		t.Fatalf("a stall line outlived the progress that answered it: %q", s.f.ag.Peek())
	}
	s.f.ag.subs.mu.Lock()
	last := s.run.lastTool
	s.f.ag.subs.mu.Unlock()
	if last != "read_file x.go" {
		t.Fatalf("lastTool %q", last)
	}
	s.stallOnce(t)
	if q := s.f.ag.Peek(); len(q) != 1 || !strings.Contains(q[0], "has been quiet") || !strings.Contains(q[0], "last read_file x.go") {
		t.Fatalf("after progress the next stall is a first one: %q", q)
	}
}

func TestThirdStallStopsAndNoticesTheOperator(t *testing.T) {
	s := stallJob(t, nil)
	for i := 0; i < 2; i++ {
		s.stallOnce(t)
		s.f.ag.DrainInbox()
	}
	s.stallOnce(t)
	hb := wait(t, s.f.ends, "hand-back")
	if hb.Status != "blocked" || hb.Reason != "stalled" {
		t.Fatalf("hand-back: %+v", hb)
	}
	s.f.ag.WaitSubAgents(context.Background())
	if n := nodeOf(t, s.f.st, s.id); n.Status != engine.StatusBlocked || n.Reason != "stalled" {
		t.Fatalf("step: %s %q", n.Status, n.Reason)
	}
	if q := queued(s.f); !strings.Contains(q, "job "+s.id+" ") || strings.Contains(q, "quiet for") {
		t.Fatalf("queued: %q", q)
	}
	want := fmt.Sprintf("job %s (big) stalled three times and was stopped; its step is blocked. /task show %s", s.id, s.id)
	found := false
	for _, n := range s.noticed() {
		found = found || n == want
	}
	if !found {
		t.Fatalf("notices %q lack %q", s.noticed(), want)
	}
}

func TestHeadlessStopsAtTheSecondStall(t *testing.T) {
	s := stallJob(t, nil)
	s.f.ag.Headless = true
	s.stallOnce(t)
	s.f.ag.DrainInbox()
	s.stallOnce(t)
	hb := wait(t, s.f.ends, "hand-back")
	if hb.Status != "blocked" || hb.Reason != "stalled" {
		t.Fatalf("hand-back: %+v", hb)
	}
}

func TestWaitingPausesTheClock(t *testing.T) {
	s := stallJob(t, nil)
	s.f.ag.setWaiting(s.run, "approval")
	s.f.ag.subs.mu.Lock()
	before := s.run.runFor
	s.f.ag.subs.mu.Unlock()
	s.clk.Advance(24 * time.Hour)
	s.f.ag.subs.sup.sweep()
	s.f.ag.subs.mu.Lock()
	after := s.run.runFor
	s.f.ag.subs.mu.Unlock()
	if s.f.ag.Pending() != 0 || after != before {
		t.Fatalf("a wait counted: queued %q runFor %s → %s", s.f.ag.Peek(), before, after)
	}
	s.f.ag.setWaiting(s.run, "")
	s.f.ag.subs.sup.sweep()
	if s.f.ag.Pending() != 0 {
		t.Fatalf("the clock did not restart at the wait's end: %q", s.f.ag.Peek())
	}
	s.stallOnce(t)
	if s.f.ag.Pending() != 1 {
		t.Fatalf("no stall after the wait: %q", s.f.ag.Peek())
	}
}

func TestAskMainNeverStalls(t *testing.T) {
	s := stallJob(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	answered := make(chan string, 1)
	go func() {
		r, _ := s.f.ag.askMain(s.run, ctx, "which tokenizer?")
		answered <- r
	}()
	wait(t, s.f.asks, "the ask")
	s.clk.Advance(24 * time.Hour)
	s.f.ag.subs.sup.sweep()
	q := s.f.ag.Peek()
	if len(q) != 1 || !strings.Contains(q[0], "asks: which tokenizer?") {
		t.Fatalf("queued: %q", q)
	}
	if n, stalled := s.stalls(); n != 0 || stalled {
		t.Fatalf("an asking job stalled: %d %v", n, stalled)
	}
	if err := s.f.ag.ReplyAsk(s.id, "the new one"); err != nil {
		t.Fatal(err)
	}
	if r := wait(t, answered, "the answer"); r != "the new one" {
		t.Fatalf("answer %q", r)
	}
	s.f.ag.subs.mu.Lock()
	w := s.run.waiting
	s.f.ag.subs.mu.Unlock()
	if w != "" {
		t.Fatalf("still waiting %q after the answer", w)
	}
}

func TestMaxRuntimeExcludesWaits(t *testing.T) {
	s := stallJob(t, func(c *config.Config) { c.SubAgents.StallAfter = 0; c.SubAgents.MaxRuntime = 600 })
	s.f.ag.setWaiting(s.run, "lane")
	s.clk.Advance(time.Hour)
	s.f.ag.subs.sup.sweep()
	s.f.ag.setWaiting(s.run, "")
	s.f.ag.subs.sup.sweep()
	select {
	case hb := <-s.f.ends:
		t.Fatalf("an hour's wait ran the job out of time: %+v", hb)
	default:
	}
	s.clk.Advance(300 * time.Second)
	s.f.ag.subs.sup.sweep()
	s.clk.Advance(301 * time.Second)
	s.f.ag.subs.sup.sweep()
	hb := wait(t, s.f.ends, "hand-back")
	if hb.Status != "blocked" || hb.Reason != "ran out of time" {
		t.Fatalf("hand-back: %+v", hb)
	}
	s.f.ag.WaitSubAgents(context.Background())
	if n := nodeOf(t, s.f.st, s.id); n.Status != engine.StatusBlocked || n.Reason != "ran out of time" {
		t.Fatalf("step: %s %q", n.Status, n.Reason)
	}
}

func TestStallDuringFiredTurnIsHeld(t *testing.T) {
	s := stallJob(t, nil)
	s.f.ag.Tools.SetAllowance(nil, time.Minute)
	s.stallOnce(t)
	if s.f.ag.Pending() != 0 {
		t.Fatalf("a stall line was queued during a fired turn: %q", s.f.ag.Peek())
	}
	s.f.ag.Tools.ClearAllowance()
	s.f.ag.subs.sup.sweep()
	if q := s.f.ag.Peek(); len(q) != 1 || !strings.Contains(q[0], "has been quiet") {
		t.Fatalf("the held stall was not delivered after the turn: %q", q)
	}
}

func TestStallAfterZeroDisables(t *testing.T) {
	s := stallJob(t, func(c *config.Config) { c.SubAgents.StallAfter = 0; c.SubAgents.MaxRuntime = 0 })
	s.clk.Advance(48 * time.Hour)
	s.f.ag.subs.sup.sweep()
	if s.f.ag.Pending() != 0 {
		t.Fatalf("queued: %q", s.f.ag.Peek())
	}
	if n, stalled := s.stalls(); n != 0 || stalled {
		t.Fatalf("stalls %d %v", n, stalled)
	}
}

func TestSupervisorStartsAndStops(t *testing.T) {
	s := stallJob(t, nil)
	s.f.ag.startSupervisor()
	s.f.ag.startSupervisor() // a second start is a no-op
	s.f.ag.stopSupervisor()
	s.f.ag.stopSupervisor()
}

func TestStallFiresOnJobStallWithTheQueuedLine(t *testing.T) {
	s := stallJob(t, nil)
	type ev struct{ id, line string }
	got := make(chan ev, 2)
	s.f.ag.Events.OnJobStall = func(id, line string) { got <- ev{id, line} }
	s.stallOnce(t)
	e := wait(t, got, "OnJobStall")
	if q := s.f.ag.Peek(); e.id != s.id || len(q) != 1 || q[0] != e.line {
		t.Fatalf("hook (%q, %q), queued %q", e.id, e.line, q)
	}
}

// TestToolInFlightIsNotQuiet is ruling R11: a tool call in flight is a wait
// for the stall clock but not for the run-time meter.
func TestToolInFlightIsNotQuiet(t *testing.T) {
	s := stallJob(t, nil)
	s.f.ag.toolStarted(s.run, "shell")
	s.clk.Advance(400 * time.Second)
	s.f.ag.subs.sup.sweep()
	if s.f.ag.Pending() != 0 {
		t.Fatalf("a running check stalled: %q", s.f.ag.Peek())
	}
	var state string
	for _, js := range s.f.ag.JobStates() {
		if js.ID == s.id {
			state = js.State
		}
	}
	if state != "waiting (tool: shell)" {
		t.Fatalf("status %q", state)
	}
	// An approval inside the tool hands back to the tool's wait.
	s.f.ag.setWaiting(s.run, "approval")
	s.f.ag.setWaiting(s.run, "")
	s.f.ag.subs.mu.Lock()
	w, runFor := s.run.waiting, s.run.runFor
	s.f.ag.subs.mu.Unlock()
	if w != "tool: shell" {
		t.Fatalf("after the inner approval: waiting %q", w)
	}
	if runFor < 400*time.Second {
		t.Fatalf("the tool's time was not metered: runFor %s", runFor)
	}
	s.f.ag.toolEnded(s.run)
	s.clk.Advance(100 * time.Second)
	s.f.ag.subs.sup.sweep()
	if s.f.ag.Pending() != 0 {
		t.Fatalf("stalled before stall_after after the tool: %q", s.f.ag.Peek())
	}
	s.clk.Advance(81 * time.Second)
	s.f.ag.subs.sup.sweep()
	if q := s.f.ag.Peek(); len(q) != 1 || !strings.Contains(q[0], "has been quiet for 3m") {
		t.Fatalf("queued: %q", q)
	}
}

// TestWaitSubAgentsReturnsOnAStallLine is ruling R13: headless, the main
// model gets its turn as soon as a job stalls, not when every run ends.
func TestWaitSubAgentsReturnsOnAStallLine(t *testing.T) {
	s := stallJob(t, nil)
	s.stallOnce(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	s.f.ag.WaitSubAgents(ctx)
	if d := time.Since(start); d > time.Second {
		t.Fatalf("WaitSubAgents took %s with a stall line queued", d)
	}
	if len(s.f.ag.RunningSubAgents()) != 1 {
		t.Fatal("the job is no longer running")
	}
	// Once the line is read, the wait is for the run again.
	s.f.ag.DrainInbox()
	short, cancel2 := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel2()
	start = time.Now()
	s.f.ag.WaitSubAgents(short)
	if d := time.Since(start); d < 250*time.Millisecond {
		t.Fatalf("WaitSubAgents returned after %s with nothing unread", d)
	}
}
