package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/subagent"
)

// compactionWait is the wait reason of a run inside its own housekeeping
// call (a compaction summary): a call that streams nothing back, so no
// delta restarts the stall clock, bracketed instead like a tool call
// (ruling R20). It is the job's own running time, so the meter runs.
const compactionWait = "compaction"

// supervisorTick is how often the supervisor looks at the running jobs
// (dispatch-jobs spec §6).
const supervisorTick = 30 * time.Second

// Stop reasons the supervisor closes a job's step with (spec §6).
const (
	jobStalledReason = "stalled"
	jobOutOfTime     = "ran out of time"
)

// supervisor ticks every 30 s (tick and now are fields replaced by tests)
// over every running job: advances runFor while not waiting; a job quiet for
// StallAfter (waits excluded) is marked stalled, its count raised, one line
// queued (never a second while the first is unconsumed); the third stall —
// the second on a headless run — stops the job with reason "stalled" and
// notices the operator; a job past MaxRuntime stops with "ran out of time".
// Lines fire after a fired turn ends, never during.
//
// Only jobs are supervised: a standing sub-agent step has no `jobs` tool to
// extend, stop or reply to it, so the stall line's instructions would be
// wrong for it.
//
// The clock fields on each subRun (clockFrom, meterAt) are read and written
// only under subAgents.mu, always with now() as the source, so a test's
// fake clock and every stamp agree.
type supervisor struct {
	a    *Agent
	now  func() time.Time
	tick time.Duration

	mu   sync.Mutex    // guards stop only
	stop chan struct{} // nil while not running
}

func newSupervisor(a *Agent) *supervisor {
	return &supervisor{a: a, now: time.Now, tick: supervisorTick}
}

// subNow is the jobs' clock: the supervisor's, so a test's fake clock moves
// every stamp the stall rules compare, and time.Now otherwise.
func (a *Agent) subNow() time.Time {
	if a.subs != nil && a.subs.sup != nil && a.subs.sup.now != nil {
		return a.subs.sup.now()
	}
	return time.Now()
}

// startSupervisor starts the ticking goroutine; a second call while it runs
// is a no-op. StartSubAgents calls it.
func (a *Agent) startSupervisor() {
	s := a.subs
	if s == nil || s.sup == nil {
		return
	}
	sp := s.sup
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.stop != nil {
		return
	}
	stop := make(chan struct{})
	sp.stop = stop
	tick := sp.tick
	if tick <= 0 {
		tick = supervisorTick
	}
	go func() {
		t := time.NewTicker(tick)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				sp.sweep()
			}
		}
	}()
}

// stopSupervisor stops the ticking goroutine; StopAllSubAgents calls it.
// A later startSupervisor (a resumed session) starts a fresh one.
func (a *Agent) stopSupervisor() {
	s := a.subs
	if s == nil || s.sup == nil {
		return
	}
	sp := s.sup
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.stop != nil {
		close(sp.stop)
		sp.stop = nil
	}
}

// sweep is one supervisor pass. Panic-fenced: supervision is advisory and
// must never end the session.
func (sp *supervisor) sweep() {
	defer func() {
		if r := recover(); r != nil {
			sp.a.notice("job supervisor failed (%v); stalled jobs are not being reported", r)
		}
	}()
	sp.a.sweepJobs()
}

// supAction is one decision a sweep made under mu and carries out after.
type supAction struct {
	run    *subRun
	line   string // a stall line to queue
	stop   bool   // cancel the run (its stopReason is already set)
	notice string // tell the operator
}

// sweepJobs collects every decision under subAgents.mu, releases it, then
// queues, stops and notices — those take the inbox's lock, the store's
// (through the run's own settle) and a UI's, never under mu.
func (a *Agent) sweepJobs() {
	s := a.subs
	if s == nil || s.sup == nil {
		return
	}
	stallAfter := time.Duration(a.Cfg.SubAgents.StallAfter) * time.Second
	maxRun := time.Duration(a.Cfg.SubAgents.MaxRuntime) * time.Second
	fired := a.firedTurn()
	limit := 3
	if a.Headless {
		limit = 2
	}
	// The queue is the truth of what the main model has not read yet,
	// taken before mu (its lock is never nested inside ours).
	queued := map[string]bool{}
	for _, it := range a.PeekItems() {
		if it.Harness {
			queued[it.Text] = true
		}
	}

	s.mu.Lock()
	now := s.sup.now()
	runs := make([]*subRun, 0, len(s.runs))
	for _, r := range s.runs {
		if r.job {
			runs = append(runs, r)
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].d.Node < runs[j].d.Node })
	var acts []supAction
	for _, r := range runs {
		if r.stopReason != "" || r.interrupt {
			continue // already on its way out
		}
		if !meterPaused(r.waiting) {
			r.runFor += now.Sub(r.meterAt)
			r.meterAt = now
		}
		if maxRun > 0 && r.runFor >= maxRun {
			r.stopReason = jobOutOfTime
			acts = append(acts, supAction{run: r, stop: true})
			continue
		}
		// A wait (a tool call in flight included) is never a stall; during a scheduled event's turn nothing
		// is judged at all — the turn receives no queued line and could not
		// act on one — and the quiet time it covers is judged by the first
		// sweep after it, which is how a stall "fires after the turn ends".
		// A compaction is a wait too, but a bounded one: a model call that
		// streams nothing has no limit of its own short of the HTTP client's,
		// so one longer than twice stall_after is judged quiet after all —
		// the main model can still jobs stop a summary call that hung.
		compacting := r.waiting == compactionWait && now.Sub(r.waitSince) >= 2*stallAfter
		if stallAfter <= 0 || (r.waiting != "" && !compacting) || fired {
			continue
		}
		if r.stallLine != "" && queued[r.stallLine] {
			// The last notice is still unread: never a second beside it,
			// and nothing is counted against a job whose notice nobody has
			// seen. The clock restarts from each such look, so once the
			// line is read the job gets a whole stall_after before the next.
			r.clockFrom = now
			continue
		}
		if now.Sub(r.clockFrom) < stallAfter {
			continue
		}
		r.stalls++
		r.stalled = true
		if r.stalls >= limit {
			r.stopReason = jobStalledReason
			times := "three times"
			if limit == 2 {
				times = "twice"
			}
			acts = append(acts, supAction{run: r, stop: true,
				notice: fmt.Sprintf("job %s (%s) stalled %s and was stopped; its step is blocked. /task show %s", r.d.Node, r.d.Owner, times, r.d.Node)})
			continue
		}
		line := stallLine(r, now)
		r.stallAt, r.stallLine, r.clockFrom = now, line, now
		acts = append(acts, supAction{run: r, line: line})
	}
	s.mu.Unlock()

	// A record untouched for StaleAfter stops vouching for its jobs
	// (Suspect), and a job parked for an hour in a question or a lane
	// would then be closed by a session starting next door. Each sweep
	// with a job running refreshes it; the ledger skips its git lookup
	// for a forced write, so this is one small file every tick.
	if len(runs) > 0 {
		a.jobsChanged()
	}

	for _, act := range acts {
		if act.line != "" {
			// Re-checked under mu: progress in the gap since the decision
			// (noteActivity) or the run's end has already answered it.
			s.mu.Lock()
			live := s.runs[act.run.d.Node] == act.run && act.run.stalled && act.run.stallLine == act.line
			s.mu.Unlock()
			if live {
				a.EnqueueHarness(act.line)
				act.run.sendEvent(jobEvent{Kind: "stall", Line: act.line})
				// The queue first, the event second, as a hand-back and an
				// ask do: a UI answers it by starting a turn for whatever is
				// queued when the main model is idle.
				if a.Events.OnJobStall != nil {
					a.Events.OnJobStall(act.run.d.Node, act.line)
				}
			}
		}
		if act.stop {
			act.run.cancel()
		}
		if act.notice != "" {
			a.notice("%s", act.notice)
		}
	}
}

// stallLine is the line queued for a stalled job, in the spec's exact words
// (dispatch-jobs spec §6). The caller holds subAgents.mu and has already
// counted this stall; run.stallAt is still the previous one's time.
func stallLine(run *subRun, now time.Time) string {
	id, who := run.d.Node, run.d.Owner
	quiet := engine.ShortDuration(now.Sub(run.clockFrom))
	head := fmt.Sprintf("(%s) has been quiet for %s", who, quiet)
	if run.stalls >= 2 {
		again := "stalled again; you did not act on the first notice"
		// Not Before, rather than After: an extend or reply answering the
		// last line may carry that line's own timestamp on a coarse clock.
		if !run.lastIntervention.IsZero() && !run.lastIntervention.Before(run.stallAt) {
			again = "stalled again after your extend/reply"
		}
		head = fmt.Sprintf("(%s) %s; quiet for %s", who, again, quiet)
	}
	calls := fmt.Sprintf("%d tool calls", run.calls)
	if run.calls == 1 {
		calls = "1 tool call"
	}
	if run.lastTool != "" {
		calls += fmt.Sprintf(", last %s %s ago", run.lastTool, engine.ShortDuration(now.Sub(run.lastActivity)))
	}
	return jobTag(id) + head + ": " + calls + ". " +
		fmt.Sprintf("jobs extend %s to give it more time, jobs stop %s to end it, jobs reply %s <text> to steer it, or jobs status %s to look.", id, id, id, id)
}

// noteActivity records progress — a tool call done (tool non-empty, file
// the path it touched when it had one) or a streamed delta (tool "") —
// which restarts the stall clock and clears the stall count (spec §6). A
// stall line still unread when the job moves again is taken back out of
// the queue: the job is no longer quiet.
func (a *Agent) noteActivity(run *subRun, tool, file string) {
	s := a.subs
	if s == nil || run == nil {
		return
	}
	now := a.subNow()
	s.mu.Lock()
	run.lastActivity, run.clockFrom = now, now
	if tool != "" {
		run.calls++
		run.lastTool = strings.TrimSpace(tool + " " + file)
	}
	run.stalls = 0
	stale := ""
	if run.stalled {
		stale = run.stallLine
	}
	run.stalled = false
	s.mu.Unlock()
	if stale != "" {
		a.RemoveWhere(func(it InboxItem) bool { return it.Harness && it.Text == stale })
	}
}

// setWaiting records that run is waiting on someone else — reason is
// "ask_main", "lane", "consent" or "approval" — or, with "", that the wait
// ended. The stall clock stops while it waits; when the wait ends it starts
// again from zero (clockFrom is stamped, lastActivity is not: a wait ending
// is not the job's progress). The run-time meter stops too, for every wait
// but a tool call in flight (meterPaused). A wait that began inside a tool
// call (an approval, ask_main) hands back to that tool's wait when it ends,
// rather than leaving the tool's remaining time on the stall clock.
func (a *Agent) setWaiting(run *subRun, reason string) {
	s := a.subs
	if s == nil || run == nil {
		return
	}
	now := a.subNow()
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case reason == "" && run.inTool != "":
		reason = toolWait(run.inTool)
	case reason == "" && run.inChore:
		// The lane wait a chore takes on its way to the server (or a
		// spend-cap prompt inside it) ends inside the chore: back to the
		// chore's own wait, not to quiet — and the chore's bound and the
		// stall clock both start over here, so time spent queued behind
		// the primary (minutes, when it is busy) is never counted against
		// the summary call that has only now gone out.
		changeWaitLocked(run, compactionWait, now)
		run.waitSince, run.clockFrom = now, now
		return
	}
	changeWaitLocked(run, reason, now)
}

// choreStarted and choreEnded bracket one housekeeping call of the run
// (OnChoreStart/OnChoreEnd from the scratch agent's choreDo): a wait for
// the stall clock, on the meter, shown as "waiting (compaction)". The
// clock starts afresh when the chore ends, as after any wait.
func (a *Agent) choreStarted(run *subRun) {
	s := a.subs
	if s == nil || run == nil {
		return
	}
	now := a.subNow()
	s.mu.Lock()
	defer s.mu.Unlock()
	run.inChore = true
	changeWaitLocked(run, compactionWait, now)
}

func (a *Agent) choreEnded(run *subRun) {
	s := a.subs
	if s == nil || run == nil {
		return
	}
	now := a.subNow()
	s.mu.Lock()
	defer s.mu.Unlock()
	run.inChore = false
	reason := ""
	if run.inTool != "" {
		reason = toolWait(run.inTool)
	}
	changeWaitLocked(run, reason, now)
}

// toolStarted and toolEnded bracket one tool call of the run (ruling R11):
// a tool in flight is a wait for the stall clock — every scoped tool is
// bounded, and a check running for minutes is not quiet — but not for the
// run-time meter. jobs status and /agents show "waiting (tool: <name>)".
func (a *Agent) toolStarted(run *subRun, name string) {
	s := a.subs
	if s == nil || run == nil {
		return
	}
	now := a.subNow()
	s.mu.Lock()
	defer s.mu.Unlock()
	run.inTool = name
	changeWaitLocked(run, toolWait(name), now)
}

func (a *Agent) toolEnded(run *subRun) {
	s := a.subs
	if s == nil || run == nil {
		return
	}
	now := a.subNow()
	s.mu.Lock()
	defer s.mu.Unlock()
	run.inTool = ""
	changeWaitLocked(run, "", now)
}

func toolWait(name string) string { return "tool: " + name }

// meterPaused reports whether a run waiting for reason is off the run-time
// meter: every wait on someone else is; a tool call in flight and the
// run's own compaction are the job's own running time.
func meterPaused(reason string) bool {
	return reason != "" && !strings.HasPrefix(reason, "tool: ") && reason != compactionWait
}

// laneSayAfter is how long the primary's lane acquire waits in silence
// before the wait is said (a variable for the tests).
var laneSayAfter = 2 * time.Second

// acquirePrimaryLane takes the primary's lane on server and, when a request
// already in flight there holds it past laneSayAfter, says so once for this
// wait — naming the co-worker whose job is in its model call on that
// server when there is one — and takes the notice down again as soon as
// the lane is had (a toast would otherwise outlive the wait by its own
// duration). A co-worker on the primary's own server shares its lane
// (spec §2.2), so the primary's next request — the turn that answers a
// hand-back or a stall line, or a chore of its own — goes out only when
// that co-worker's call ends; said, the pause is a wait, and unsaid it
// reads as a hang. The notice is emitted under its own lock so that the
// clearing, which waits for that lock, can never overtake it.
func (a *Agent) acquirePrimaryLane(ctx context.Context, lanes *subagent.Lanes, server string) (func(), error) {
	var mu sync.Mutex
	over, said := false, false
	timer := time.AfterFunc(laneSayAfter, func() {
		mu.Lock()
		defer mu.Unlock()
		if over {
			return
		}
		said = true
		a.transient("waiting for the shared server: %s is in flight; the request goes out when it ends", a.sharingServer(server))
	})
	release, err := lanes.Acquire(ctx, server, true)
	mu.Lock()
	over = true
	clear := said
	mu.Unlock()
	timer.Stop()
	if clear {
		a.transientClear()
	}
	return release, err
}

// transientClear takes a transient notice down early. Only a UI with its
// own transient channel (a toast) has anything to clear; one that keeps
// them as notices is left alone, since an empty notice would print.
func (a *Agent) transientClear() {
	if a.Events.OnTransient != nil {
		a.Events.OnTransient("")
	}
}

// sharingServer names what holds the primary's server: the co-workers
// whose run on it is in a model call of its own — not one inside a tool
// call, a question or a consent prompt, which hold no lane — ("big's
// request", "big's and small's requests"), or, with none, "another
// request" (an API call, a consultation).
func (a *Agent) sharingServer(server string) string {
	s := a.subs
	if s == nil {
		return "another request"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	seen := map[string]bool{}
	for _, r := range s.runs {
		inCall := (r.waiting == "" || r.waiting == compactionWait) && r.inTool == ""
		if inCall && s.cards[r.d.Owner].Server == server && !seen[r.d.Owner] {
			seen[r.d.Owner] = true
			names = append(names, r.d.Owner)
		}
	}
	sort.Strings(names)
	switch len(names) {
	case 0:
		return "another request"
	case 1:
		return names[0] + "'s request"
	default:
		return strings.Join(names, "'s and ") + "'s requests"
	}
}

// changeWaitLocked moves run to reason, bringing the meter up to date. The
// caller holds subAgents.mu.
func changeWaitLocked(run *subRun, reason string, now time.Time) {
	if run.waiting == reason {
		return
	}
	if !meterPaused(run.waiting) {
		run.runFor += now.Sub(run.meterAt)
	}
	run.meterAt = now
	switch {
	case reason == "":
		run.clockFrom = now
	case run.waiting == "":
		run.waitSince = now
	}
	run.waiting = reason
}

// toolFile is the path a tool call names, for the "last read_file x.go"
// part of a stall line; "" when its arguments name none.
func toolFile(args string) string {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return ""
	}
	for _, k := range []string{"path", "file", "pattern", "command"} {
		if v, ok := m[k].(string); ok && v != "" {
			if r := []rune(v); len(r) > 60 {
				v = string(r[:60]) + "…"
			}
			return v
		}
	}
	return ""
}
