package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/subagent"
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
