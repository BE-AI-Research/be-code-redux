package agent

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/subagent"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// jobSessionEnded is the reason a job's step closes with when the session
// ends under it (dispatch-jobs spec §6).
const jobSessionEnded = "session ended"

// jobInterrupted is the reason an orphaned job's step closes with when a
// later session's resume pass finds it open with no run (ruling R8).
const jobInterrupted = "interrupted"

// jobSlot names the placeholder a DispatchJob holds in subAgents.pending
// while it files the step: the slot counts against max_concurrent before
// the step's id exists. The prefix cannot be a node id.
var jobSlot atomic.Int64

// DispatchJob starts a job now (dispatch-jobs spec §3): a step under the
// active root (or the node doing under it), owned by who, scoped to
// scopePaths, marked as a job, and dispatched at once through the same path
// the scheduler uses — same consent, snapshot branch, scoped registry and
// ask_main. Its first line of job is the step's text; the rest, and extra,
// become the step's note. It returns the step's id, the job id.
//
// scope is the raw list as the caller wrote it, kept for messages; the
// checks and the stored scope use scopePaths, cleaned.
//
// Refusals, in order, each a plain sentence: a fired turn
// (errFiredSubAgents, as task owner answers); "<who> is not a co-worker";
// "a job needs a description"; "a job needs a scope"; "scope <p> escapes
// the workspace"; "scope is outside <who>'s max_scope (<list>)"; "<who>
// cannot be dispatched: its max_scope is invalid"; "scope overlaps <id>";
// "dispatch needs the task record"; "<n> of <max> jobs already running;
// jobs wait first".
func (a *Agent) DispatchJob(ctx context.Context, who, job, scope, extra string, scopePaths []string) (id string, err error) {
	_ = scope // the tool's own messages quote it; every check uses scopePaths
	if a.firedTurn() {
		return "", errFiredSubAgents
	}
	s := a.subs
	cw, known := a.coworkerFor(who)
	if s == nil || !known {
		return "", fmt.Errorf("%s is not a co-worker", who)
	}
	job = strings.TrimSpace(job)
	if job == "" {
		return "", errors.New("a job needs a description")
	}
	clean, err := subagent.CleanJobScope(scopePaths)
	if err != nil {
		return "", err
	}
	if !subagent.Within(clean, cw.MaxScope) {
		return "", fmt.Errorf("scope is outside %s's max_scope (%s)", who, strings.Join(cw.MaxScope, ", "))
	}
	if cw.MaxScopeInvalid {
		return "", fmt.Errorf("%s cannot be dispatched: its max_scope is invalid", who)
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}

	// Read the tree before mu (the invariant on subAgents), then check and
	// reserve a slot in one critical section.
	steps := a.subSteps("dispatch")
	slot := fmt.Sprintf("job-slot-%d", jobSlot.Add(1))
	s.mu.Lock()
	if overlap := jobOverlapLocked(s, steps, clean); overlap != "" {
		s.mu.Unlock()
		return "", fmt.Errorf("scope overlaps %s", overlap)
	}
	if a.engine() == nil {
		s.mu.Unlock()
		return "", errors.New("dispatch needs the task record")
	}
	if n, max := len(s.runs)+len(s.pending), a.Cfg.SubAgents.MaxConcurrent; n >= max {
		s.mu.Unlock()
		return "", fmt.Errorf("%d of %d jobs already running; jobs wait first", n, max)
	}
	if s.stopping {
		s.mu.Unlock()
		return "", errors.New("the session is ending; nothing new is dispatched")
	}
	s.pending[slot] = true
	if s.jobScopes == nil {
		s.jobScopes = map[string][]string{}
	}
	s.jobScopes[slot] = clean
	s.mu.Unlock()
	release := func() {
		s.mu.Lock()
		delete(s.pending, slot)
		delete(s.jobScopes, slot)
		s.mu.Unlock()
	}

	text, note := job, ""
	if first, rest, ok := strings.Cut(job, "\n"); ok {
		text, note = strings.TrimSpace(first), strings.TrimSpace(rest)
	}
	if extra = strings.TrimSpace(extra); extra != "" {
		note = strings.TrimSpace(note + "\n\n" + extra)
	}
	ran := false
	a.engineDo("dispatch", func(st *engine.Store) {
		ran = true
		id, err = st.AddJob(st.JobParent(), text, note, who, clean)
	})
	if !ran {
		release()
		return "", errors.New("dispatch needs the task record")
	}
	if err != nil {
		release()
		return "", err
	}

	// The slot becomes the step's own reservation, which dispatchPicked
	// releases whatever happens.
	s.mu.Lock()
	delete(s.pending, slot)
	delete(s.jobScopes, slot)
	s.pending[id] = true
	s.jobScopes[id] = clean
	s.mu.Unlock()
	steps = a.subSteps("dispatch")
	if a.dispatchPicked(steps, subagent.Candidate{ID: id, Owner: who}, cw, true) {
		return id, nil
	}
	// Not installed: the session stopped, a scheduled event's turn began,
	// or the engine went away while the dispatch was built. The ready rule
	// never offers a job, so a step left todo here would wait forever.
	reason := "not started"
	if a.firedTurn() {
		reason = "not started: a scheduled event began"
	}
	a.engineDo("dispatch", func(st *engine.Store) { _ = st.CloseAs(id, who, "blocked", reason) })
	if a.firedTurn() {
		return "", errFiredSubAgents
	}
	return "", fmt.Errorf("%s could not be started; it is closed blocked (%s)", id, reason)
}

// coworkerFor is who's validated entry, as EnableSubAgents recorded it.
func (a *Agent) coworkerFor(who string) (cw config.CoworkerConfig, ok bool) {
	s := a.subs
	if s == nil {
		return cw, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cw, ok = s.cws[who]
	return cw, ok
}

// jobOverlapLocked names the first running run, or open assigned step,
// whose scope overlaps clean; "" when none does. Runs are checked by their
// live scope (a widened one included); steps cover the reservations and
// the scheduler's own assigned work. The caller holds s.mu.
func jobOverlapLocked(s *subAgents, steps []subagent.Step, clean []string) string {
	if id := jobScopeTakenLocked(s, clean); id != "" {
		return id
	}
	ids := make([]string, 0, len(s.runs))
	for id := range s.runs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if subagent.Overlap(clean, s.runs[id].d.Scope) {
			return id
		}
	}
	var found string
	var walk func(st []subagent.Step)
	walk = func(st []subagent.Step) {
		for _, x := range st {
			if found != "" {
				return
			}
			open := x.Status != "done" && x.Status != "blocked" && x.Status != "dropped"
			if open && x.Owner != "" && len(x.Scope) > 0 && subagent.Overlap(clean, x.Scope) {
				found = x.ID
				return
			}
			walk(x.Children)
		}
	}
	walk(steps)
	return found
}

// jobScopeTakenLocked names a job reservation not yet installed whose scope
// overlaps scope; "" when none does. A slot placeholder is named "a job
// being dispatched", since it has no id yet. The caller holds s.mu.
func jobScopeTakenLocked(s *subAgents, scope []string) string {
	keys := make([]string, 0, len(s.jobScopes))
	for k := range s.jobScopes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if subagent.Overlap(scope, s.jobScopes[k]) {
			if strings.HasPrefix(k, "job-slot-") {
				return "a job being dispatched"
			}
			return k
		}
	}
	return ""
}

// jobStoppedByMain is the reason a job stopped through the jobs tool closes
// with when the main model gave none.
const jobStoppedByMain = "stopped by the main model"

// jobTag is the prefix of every line a job queues for the main model — its
// hand-back, its ask, and (Task 5) its stall — so a person reading the
// queue, and JobWait, can tell which job a line is about.
func jobTag(id string) string { return "job " + id + " " }

// jobEvent is one thing a job did that JobWait returns: Kind is "done",
// "ask" or "stall", Line exactly the line that was queued for it.
type jobEvent struct {
	Kind string
	Line string
}

// sendEvent hands ev to a waiting JobWait without ever blocking the run. A
// full buffer drops its oldest event, never the new one: an old event is
// most likely stale (its line already delivered through the queue), and
// JobWait skips a stale one anyway (ruling R10), so the newest is what
// matters. The line itself is already queued, so nothing is lost to the
// main model either way.
func (r *subRun) sendEvent(ev jobEvent) {
	if r == nil || r.events == nil {
		return
	}
	for i := 0; i < cap(r.events)+1; i++ {
		select {
		case r.events <- ev:
			return
		default:
		}
		select {
		case <-r.events:
		default:
		}
	}
}

// jobFinishedState is a finished run's state word for JobStates and the
// already-finished answer. The caller holds s.mu.
func jobFinishedState(r *subRun) string {
	switch {
	case r.stopReason != "":
		return "stopped"
	case r.endHB.Status == "done":
		return "done"
	}
	return "blocked" // blocked, or interrupted at session end
}

// jobDelivered is JobWait's answer for a finished job whose hand-back is
// no longer queued: the main model already has it.
func jobDelivered(id, state string) string {
	return fmt.Sprintf("job %s already finished (%s); its hand-back was delivered earlier", id, state)
}

// jobRun is the running job id names, else its finished run; ok is false
// for an id that is no job of this session. The caller holds s.mu.
func jobRunLocked(s *subAgents, id string) (run *subRun, finished, ok bool) {
	if r := s.runs[id]; r != nil && r.job {
		return r, false, true
	}
	if r := s.jobsDone[id]; r != nil {
		return r, true, true
	}
	return nil, false, false
}

// runningJob is the running job id names, or the error the jobs tool
// reports: "no job <id>", or "job <id> has already finished".
func (a *Agent) runningJob(id string) (*subRun, error) {
	s := a.subs
	if s == nil {
		return nil, fmt.Errorf("no job %s", id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, finished, ok := jobRunLocked(s, id)
	switch {
	case !ok:
		return nil, fmt.Errorf("no job %s", id)
	case finished:
		return nil, fmt.Errorf("job %s has already finished", id)
	}
	return run, nil
}

// JobStates is the jobs tool's status: every running job, then every
// finished one, each sorted by id.
func (a *Agent) JobStates() []tools.JobState {
	s := a.subs
	if s == nil {
		return nil
	}
	maxRun := time.Duration(a.Cfg.SubAgents.MaxRuntime) * time.Second
	s.mu.Lock()
	var running, finished []*subRun
	for _, r := range s.runs {
		if r.job {
			running = append(running, r)
		}
	}
	for _, r := range s.jobsDone {
		finished = append(finished, r)
	}
	type row struct {
		st      tools.JobState
		scratch *Agent
	}
	byID := func(rs []*subRun) {
		sort.Slice(rs, func(i, j int) bool { return rs[i].d.Node < rs[j].d.Node })
	}
	byID(running)
	byID(finished)
	rows := make([]row, 0, len(running)+len(finished))
	for _, r := range running {
		st := tools.JobState{ID: r.d.Node, Owner: r.d.Owner, Elapsed: time.Since(r.started),
			At: r.d.Node, LastTool: r.lastTool, LastActivity: r.lastActivity,
			Runtime: r.runFor, MaxRuntime: maxRun}
		if !meterPaused(r.waiting) && !r.meterAt.IsZero() {
			st.Runtime += a.subNow().Sub(r.meterAt)
		}
		if r.at != "" {
			st.At = r.at
		}
		switch {
		case r.question != "":
			st.State = "asking"
		case r.waiting != "":
			st.State = "waiting (" + r.waiting + ")"
		case r.stalled:
			st.State = "stalled"
		default:
			st.State = "running"
		}
		rows = append(rows, row{st: st, scratch: r.scratch})
	}
	for _, r := range finished {
		hb := r.endHB
		st := tools.JobState{ID: r.d.Node, Owner: r.d.Owner, Elapsed: hb.Elapsed, Calls: hb.Calls,
			At: r.d.Node, LastTool: r.lastTool, LastActivity: r.lastActivity, Summary: hb.Summary}
		st.State = jobFinishedState(r)
		if st.Summary == "" {
			st.Summary = hb.Reason
		}
		rows = append(rows, row{st: st})
	}
	s.mu.Unlock()
	// Usage takes the scratch agent's own lock, so it is read outside s.mu.
	out := make([]tools.JobState, len(rows))
	for i, r := range rows {
		if r.scratch != nil {
			r.st.Calls = r.scratch.Usage().ToolCalls
		}
		out[i] = r.st
	}
	return out
}

// JobWait blocks until the job id (any running job when id is "") ends,
// asks, or stalls, or until d elapses or ctx ends. It returns the line
// queued for that event and takes exactly that line out of the queue; a
// wait that runs out returns "" and consumes nothing, so the line is still
// delivered the ordinary way. With id "" and nothing running it returns ""
// at once; a finished id answers at once with its hand-back, or — when the
// hand-back is no longer queued — says it was delivered earlier; an
// unknown id is "no job <id>".
//
// The queue is the truth of liveness (ruling R10): an ask or stall event
// whose line is no longer queued reached the main model some other way (a
// mid-turn delivery) and is skipped, and the wait goes on.
func (a *Agent) JobWait(ctx context.Context, id string, d time.Duration) (string, error) {
	s := a.subs
	if s == nil {
		if id == "" {
			return "", nil
		}
		return "", fmt.Errorf("no job %s", id)
	}
	var runs []*subRun
	s.mu.Lock()
	if id == "" {
		for _, r := range s.runs {
			if r.job {
				runs = append(runs, r)
			}
		}
	} else {
		run, finished, ok := jobRunLocked(s, id)
		if !ok {
			s.mu.Unlock()
			return "", fmt.Errorf("no job %s", id)
		}
		if finished {
			line, state := run.endLine, jobFinishedState(run)
			s.mu.Unlock()
			if a.consumeJobLine(line) == 0 {
				return jobDelivered(id, state), nil
			}
			return line, nil
		}
		runs = []*subRun{run}
	}
	s.mu.Unlock()
	if len(runs) == 0 {
		return "", nil
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].d.Node < runs[j].d.Node })

	// One select over every run's events and done, the deadline and ctx:
	// reflect.Select takes an event only when it is returned, so a wait
	// that ends otherwise leaves every channel as it found it.
	timer := time.NewTimer(d)
	defer timer.Stop()
	cases := make([]reflect.SelectCase, 0, 2*len(runs)+2)
	for _, r := range runs {
		cases = append(cases,
			reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(r.events)},
			reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(r.done)})
	}
	timerCase := len(cases)
	cases = append(cases,
		reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(timer.C)},
		reflect.SelectCase{Dir: reflect.SelectRecv, Chan: reflect.ValueOf(ctx.Done())})
	for {
		chosen, v, _ := reflect.Select(cases)
		if chosen >= timerCase {
			return "", nil // the deadline or ctx: nothing consumed
		}
		run := runs[chosen/2]
		ended := chosen%2 == 1
		line := ""
		if !ended {
			ev := v.Interface().(jobEvent)
			if !a.jobEventLive(run, ev) {
				continue // an ask already answered, a stall already extended
			}
			ended = ev.Kind == "done"
			line = ev.Line
		}
		if ended {
			// Finished: its hand-back, whatever is still in its events.
			s.mu.Lock()
			line = run.endLine
			state := jobFinishedState(run)
			s.mu.Unlock()
			if a.consumeJobLine(line) == 0 {
				return jobDelivered(run.d.Node, state), nil
			}
			return line, nil
		}
		if a.consumeJobLine(line) == 0 {
			continue // stale: the line already reached the main model
		}
		return line, nil
	}
}

// jobEventLive reports whether ev still describes run: a hand-back always
// does; an ask only while the run is still asking, a stall only while it
// is still stalled — an event JobReply or JobExtend has since dealt with
// is not returned as though it were news.
func (a *Agent) jobEventLive(run *subRun, ev jobEvent) bool {
	s := a.subs
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runs[run.d.Node] != run {
		// Finished since: the done case reports it with its hand-back.
		return ev.Kind == "done"
	}
	switch ev.Kind {
	case "ask":
		return run.question != ""
	case "stall":
		return run.stalled
	}
	return true
}

// consumeJobLine takes one returned line out of the main model's queue: it
// has been handed over as the jobs tool's result, and delivering it again
// as a queued line would tell the model twice.
// It returns how many queued items it removed: 0 means the line had
// already left the queue.
func (a *Agent) consumeJobLine(line string) int {
	if line == "" {
		return 0
	}
	return a.RemoveWhere(func(it InboxItem) bool { return it.Harness && it.Text == line })
}

// JobStop stops a running job; its step closes blocked with reason (default
// "stopped by the main model") and its hand-back is queued as usual.
func (a *Agent) JobStop(id, reason string) error {
	run, err := a.runningJob(id)
	if err != nil {
		return err
	}
	if reason = strings.TrimSpace(reason); reason == "" {
		reason = jobStoppedByMain
	}
	s := a.subs
	s.mu.Lock()
	if s.runs[id] != run {
		s.mu.Unlock()
		return fmt.Errorf("job %s has already finished", id)
	}
	run.stopReason = reason
	cancel := run.cancel
	s.mu.Unlock()
	cancel()
	return nil
}

// JobReply answers a running job's open ask; with no ask open, the text is
// queued on the job's own inbox and reaches it before its next request.
func (a *Agent) JobReply(id, text string) error {
	run, err := a.runningJob(id)
	if err != nil {
		return err
	}
	s := a.subs
	s.mu.Lock()
	if s.runs[id] != run {
		s.mu.Unlock()
		return fmt.Errorf("job %s has already finished", id)
	}
	asking, scratch := run.question != "", run.scratch
	s.mu.Unlock()
	if asking {
		if err := a.ReplyAsk(id, text); err == nil {
			return nil
		}
		// The ask closed in between (answered, timed out): fall through to
		// the job's inbox, which is still the right place for the text.
	}
	if scratch == nil {
		return fmt.Errorf("job %s has not started yet", id)
	}
	// A reply restarts the clock but not the stall count (spec §6).
	now := a.subNow()
	s.mu.Lock()
	if s.runs[id] != run {
		s.mu.Unlock()
		return fmt.Errorf("job %s has already finished", id)
	}
	run.lastActivity, run.lastIntervention, run.clockFrom = now, now, now
	s.mu.Unlock()
	scratch.EnqueueHarness(text)
	return nil
}

// JobExtend gives a running job more time: it is no longer stalled, its
// clock restarts from now, and the intervention is stamped so the stall
// supervisor does not report it again at once.
func (a *Agent) JobExtend(id string) error {
	run, err := a.runningJob(id)
	if err != nil {
		return err
	}
	now := a.subNow()
	s := a.subs
	s.mu.Lock()
	if s.runs[id] != run {
		s.mu.Unlock()
		return fmt.Errorf("job %s has already finished", id)
	}
	run.stalled = false
	run.lastActivity = now
	run.lastIntervention = now
	run.clockFrom = now
	s.mu.Unlock()
	return nil
}
