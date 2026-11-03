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
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/subagent"
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

// A sub-agent's compaction summary streams nothing back (ruling R20): it is
// a wait for the stall clock, like a tool call, and the job's own time on
// the run-time meter. The lane wait inside it hands back to the compaction
// wait; one that runs past twice stall_after is judged quiet after all; and
// the clock starts afresh when the chore ends.
func TestCompactionIsAWaitNotAStall(t *testing.T) {
	s := stallJob(t, nil)
	s.f.ag.choreStarted(s.run)
	s.clk.Advance(181 * time.Second)
	s.f.ag.subs.sup.sweep()
	if s.f.ag.Pending() != 0 {
		t.Fatalf("a compacting job stalled: %q", s.f.ag.Peek())
	}
	s.f.ag.subs.mu.Lock()
	waiting, runFor := s.run.waiting, s.run.runFor
	s.f.ag.subs.mu.Unlock()
	if waiting != compactionWait {
		t.Fatalf("waiting %q, want %q", waiting, compactionWait)
	}
	if runFor < 181*time.Second {
		t.Fatalf("the meter paused for a compaction: ran %s", runFor)
	}
	s.f.ag.setWaiting(s.run, "lane")
	s.f.ag.setWaiting(s.run, "")
	s.f.ag.subs.mu.Lock()
	waiting = s.run.waiting
	s.f.ag.subs.mu.Unlock()
	if waiting != compactionWait {
		t.Fatalf("the lane wait ending left %q, want the compaction wait back", waiting)
	}
	// Twice stall_after inside the chore: quiet after all.
	s.clk.Advance(180 * time.Second)
	s.f.ag.subs.sup.sweep()
	if q := s.f.ag.Peek(); len(q) != 1 || !strings.Contains(q[0], "has been quiet for") {
		t.Fatalf("a chore past twice stall_after was not reported: %q", q)
	}
	s.f.ag.DrainInbox()
	s.f.ag.choreEnded(s.run)
	s.f.ag.subs.mu.Lock()
	waiting = s.run.waiting
	s.f.ag.subs.mu.Unlock()
	if waiting != "" {
		t.Fatalf("after the chore: waiting %q", waiting)
	}
	s.clk.Advance(90 * time.Second)
	s.f.ag.subs.sup.sweep()
	if s.f.ag.Pending() != 0 {
		t.Fatalf("stalled before a fresh stall_after after the chore: %q", s.f.ag.Peek())
	}
	s.clk.Advance(91 * time.Second)
	s.f.ag.subs.sup.sweep()
	if s.f.ag.Pending() != 1 {
		t.Fatalf("no stall after the chore ended and stall_after passed: %q", s.f.ag.Peek())
	}
}

// choreDo brackets a housekeeping call with OnChoreStart/OnChoreEnd (the
// primary branch here; the helper branch shares the bracket, which sits
// above both), so a runner can see the call that streams nothing.
func TestChoreDoFiresTheChoreEvents(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{responses: []provider.ChatResponse{{Content: "summary"}}}, nil)
	var starts, ends int
	ag.Events.OnChoreStart = func() { starts++ }
	ag.Events.OnChoreEnd = func() {
		ends++
		if starts != 1 {
			t.Errorf("OnChoreEnd before OnChoreStart")
		}
	}
	_, err := ag.choreDo(context.Background(), ag.primaryTarget(), choreOpts{}, provider.ChatRequest{Messages: []provider.Message{{Role: "user", Content: "summarise"}}})
	if err != nil {
		t.Fatal(err)
	}
	if starts != 1 || ends != 1 {
		t.Fatalf("starts %d ends %d", starts, ends)
	}
}

// The startup pass closes a job left open with no run (R8) — unless another
// live session reports running it (OtherJobs, from the work ledger): then it
// is left alone and the operator told which session has it. A job nobody
// reports is closed as before, however many sessions are live.
func TestOrphanJobIsKeptWhileAnotherSessionRunsIt(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, nil)
	var mu sync.Mutex
	var notices []string
	f.ag.Events.OnNotice = func(m string) { mu.Lock(); notices = append(notices, m); mu.Unlock() }
	root := f.st.Plan("work", nil)
	// Filed as a job with no run in this process: what another session's
	// live job, or a crash, looks like from here.
	orphan, err := f.st.AddJob(root, "port it", "", "big", []string{"internal/scan"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.st.Flush(); err != nil {
		t.Fatal(err)
	}
	looked := 0
	f.ag.OtherJobs = func() map[string]int { looked++; return map[string]int{orphan: 4242} }
	f.ag.closeOrphanJobs(f.ag.subSteps("test"), map[string]bool{})
	if st := nodeOf(t, f.st, orphan).Status; st != engine.StatusTodo && st != engine.StatusDoing {
		t.Fatalf("job closed as %q while another session runs it", st)
	}
	mu.Lock()
	got := strings.Join(notices, "\n")
	mu.Unlock()
	if !strings.Contains(got, "job "+orphan+" (big) is open") || !strings.Contains(got, "pid 4242") || !strings.Contains(got, "left as is") {
		t.Fatalf("notice: %q", got)
	}
	if looked != 1 {
		t.Fatalf("the records were read %d times for one orphan", looked)
	}
	// Another session is live but does not report this job: a leftover.
	f.ag.OtherJobs = func() map[string]int { return map[string]int{"9.9": 4242} }
	f.ag.closeOrphanJobs(f.ag.subSteps("test"), map[string]bool{})
	if n := nodeOf(t, f.st, orphan); n.Status != engine.StatusBlocked || n.Reason != jobInterrupted {
		t.Fatalf("a job no live session reports must close: %s %q", n.Status, n.Reason)
	}
}

// The work ledger is told at once when a job run begins and ends, and reads
// the running ids back through RunningJobIDs.
func TestJobRunsAreReportedToTheWorkLedger(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, nil)
	bp := parkJobs(t)
	changed := make(chan struct{}, 8)
	f.ag.JobsChanged = func() { changed <- struct{}{} }
	f.st.Plan("work", nil)
	id := dispatchJob(t, f, "port it", "internal/scan")
	wait(t, bp.entered, "the job's request")
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("JobsChanged did not fire on dispatch")
	}
	if ids := f.ag.RunningJobIDs(); len(ids) != 1 || ids[0] != id {
		t.Fatalf("running: %q", ids)
	}
	f.ag.StopAllSubAgents("test over")
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("JobsChanged did not fire when the run ended")
	}
	if ids := f.ag.RunningJobIDs(); len(ids) != 0 {
		t.Fatalf("still running after stop: %q", ids)
	}
}

// A primary lane acquire that waits behind a request on the same server is
// said after laneSayAfter — naming the co-worker whose job is in its model
// call there, else "another request" — and taken down once the lane is had.
// A short acquire says nothing.
func TestPrimaryLaneWaitIsSaid(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, nil)
	old := laneSayAfter
	laneSayAfter = 10 * time.Millisecond
	t.Cleanup(func() { laneSayAfter = old })
	said := make(chan string, 8)
	f.ag.Events.OnTransient = func(m string) { said <- m }
	const primary = "http://primary:11434"
	hear := func(t *testing.T, what string) string {
		t.Helper()
		select {
		case m := <-said:
			return m
		case <-time.After(5 * time.Second):
			t.Fatalf("nothing said: %s", what)
			return ""
		}
	}
	waitBehind := func(t *testing.T, want, never string) {
		t.Helper()
		release, err := f.ag.subs.lanes.Acquire(context.Background(), primary, false)
		if err != nil {
			t.Fatal(err)
		}
		got := make(chan error, 1)
		go func() {
			rel, err := f.ag.laneAcquire(context.Background())
			if err == nil {
				rel()
			}
			got <- err
		}()
		m := hear(t, "the wait")
		if !strings.Contains(m, "waiting for the shared server: "+want+" is in flight; the request goes out when it ends") || (never != "" && strings.Contains(m, never)) {
			t.Fatalf("said %q", m)
		}
		release()
		if err := <-got; err != nil {
			t.Fatal(err)
		}
		if m := hear(t, "the clear"); m != "" {
			t.Fatalf("the notice was not taken down: %q", m)
		}
		select {
		case m := <-said:
			t.Fatalf("extra: %q", m)
		default:
		}
	}
	// Nobody's run on the server: another request.
	waitBehind(t, "another request", "")
	// A co-worker's job in its model call on the primary's server is named;
	// one inside a tool call holds no lane and is not.
	s := f.ag.subs
	s.mu.Lock()
	s.cards["near"] = subagent.Card{Name: "near", Server: primary}
	s.cards["busy"] = subagent.Card{Name: "busy", Server: primary}
	s.runs["9.1"] = &subRun{d: subagent.Dispatch{Node: "9.1", Owner: "near"}, job: true}
	s.runs["9.2"] = &subRun{d: subagent.Dispatch{Node: "9.2", Owner: "busy"}, job: true, inTool: "shell", waiting: "tool: shell"}
	s.mu.Unlock()
	t.Cleanup(func() {
		s.mu.Lock()
		delete(s.runs, "9.1")
		delete(s.runs, "9.2")
		s.mu.Unlock()
	})
	waitBehind(t, "near's request", "busy")
	// Held and released long before laneSayAfter: nothing is said.
	laneSayAfter = time.Hour
	if rel, err := f.ag.laneAcquire(context.Background()); err != nil {
		t.Fatal(err)
	} else {
		rel()
	}
	select {
	case m := <-said:
		t.Fatalf("a short acquire was announced: %q", m)
	default:
	}
}
