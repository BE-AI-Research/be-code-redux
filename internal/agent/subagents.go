package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/profiles"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/subagent"
	"github.com/brown-enterprises/be-code/internal/tools"
	"github.com/brown-enterprises/be-code/internal/verify"
)

// subAgents is the runner behind spec §2: one goroutine per dispatched
// subtree, all on the session's root context (never a turn's), per-server
// lanes, and a hand-back through the main model's queue.
//
// mu guards every field below and every mutable field of every subRun.
//
// **The invariant, checkable by reading every use of mu: no call into the
// engine store and no notice is ever made while mu is held.** Both would
// re-enter — `engineDo` emits its detach notice synchronously through
// `Events.OnNotice`, and a UI's handler calls straight back into
// `SubAgentStates` or `ScheduleSubAgents`, which take this same
// non-reentrant mutex. So every store read is taken before mu (subSteps),
// and dispatch is three phases: decide and reserve under mu, build
// unlocked, install under mu again. mu is likewise never held across a
// model call, an approval, a blocking channel send or a wait on a run, and
// nothing holding the engine's own lock ever takes it.
type subAgents struct {
	mu    sync.Mutex
	cards map[string]subagent.Card
	cws   map[string]config.CoworkerConfig
	lanes *subagent.Lanes
	runs  map[string]*subRun // by node id
	// pending are candidates reserved by a schedule that has released mu to
	// build their dispatches. They are not running yet, but they count
	// against max_concurrent and hold their scope, so a concurrent schedule
	// can neither double-dispatch one nor start an overlapping step beside
	// it. A candidate that is abandoned is always released from here.
	pending map[string]bool
	refused map[string]bool // online consent refused this session, by name
	// stopping is set by StopAllSubAgents: an interrupted node goes back to
	// todo, and without this the schedule a finishing run makes on its way
	// out would dispatch it again after the session had asked everything to
	// stop. EnableSubAgents clears it, which is what makes resume work.
	stopping bool
	// hold names nodes that must not be dispatched while an operator is
	// changing them (AssignOwner stops a parked run, then rewrites the
	// owner; a re-dispatch in between would take the node back).
	hold map[string]bool
	// resumeAsked is set the first time StartSubAgents runs the startup
	// gate (2026-09-22 §3.6 amendment): only that first schedule ever asks
	// "resume sub-agent work?" — a step assigned mid-session dispatches at
	// once, because the operator or the main model just initiated it.
	resumeAsked bool
	// declinedResume holds the ids offered in a declined startup ask. Every
	// later ScheduleSubAgents (a `task` tool call, a `/task` command, a
	// hand-back's own parting schedule) skips exactly these ids until
	// AllowSubAgentStart (/agents start) clears the set — a decline must
	// not be silently undone by the very mechanism that dispatches
	// everything else. AssignOwner and SetScope clear a single id from it:
	// an operator or the model re-touching that one step is a fresh
	// initiation of its own, the same one that lets brand-new mid-session
	// work dispatch without asking.
	declinedResume map[string]bool
	// root is every run's parent context — the session's, never a turn's, so
	// Esc on the main run cannot reach a sub-agent. cancel ends it, and is
	// the escape hatch StopAllSubAgents pulls after its bounded wait; a
	// later EnableSubAgents makes a fresh pair, which is what lets a resumed
	// session dispatch again.
	root   context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	// primary is the primary model's lane key, rebound by bindPrimaryLane
	// when the session moves to another provider.
	primary string
	// finished is every hand-back so far this session, for run --json.
	// Appended in runSub in the same critical section that deletes the run
	// from runs, so a reader never sees a run counted as both.
	finished []subagent.HandBack
}

type subRun struct {
	d       subagent.Dispatch
	cw      config.CoworkerConfig
	ctx     context.Context
	cancel  context.CancelFunc
	started time.Time
	reply   chan string
	done    chan struct{}
	// Everything below is written and read from several goroutines — the
	// run's own, a UI's, the main loop's — and every touch is under
	// subAgents.mu.
	scratch *Agent
	reg     *tools.Registry // the scoped registry, for a scope widened mid-run
	// at is the doing node inside this subtree as of the last tool call, so
	// the bottom line can name it ("⚙ big 3.2.2") without RunningSubAgents
	// going to the store on every render frame. Refreshed on the
	// sub-agent's own goroutine, inside the observe hook that already calls
	// the store once per tool call.
	at         string
	question   string // the open ask, "" when none
	stopReason string // set by an operator stop
	interrupt  bool   // reset the node instead of closing it
	timedOut   bool
}

// SubAgentState is one row of /agents.
type SubAgentState struct {
	Name, Model, Provider string
	Online, SubAgent      bool
	MaxScope              []string
	Node, State           string
	Calls                 int
	Since                 time.Time
	// At is what the bottom line shows for a running row: the doing node
	// inside Node's dispatched subtree when the store can name one, else
	// Node itself (the coarser root id). Set only for a running row — ""
	// for an idle card and for a waiting row (Node is set there too, but
	// nothing is dispatched yet for DoingUnderID to refine).
	At string
}

// EnableSubAgents wires the runner for the co-workers that are sub-agents
// and runs the resume pass (spec §3.6). It may be called again on resume;
// a second call replaces the cards and re-evaluates. primaryServer is the
// primary model's lane key.
func (a *Agent) EnableSubAgents(cws []config.CoworkerConfig, primaryServer string) {
	cards := map[string]subagent.Card{}
	byName := map[string]config.CoworkerConfig{}
	for _, cw := range cws {
		if !cw.SubAgent {
			continue
		}
		pc := a.Cfg.Providers[cw.Provider]
		cards[cw.Name] = subagent.Card{Name: cw.Name, Provider: cw.Provider,
			Server: subagent.LaneKey(pc.BaseURL), Online: cw.Online, SubAgent: true, MaxScope: cw.MaxScope}
		byName[cw.Name] = cw
	}
	if a.subs == nil {
		// Nothing changes for a configuration with no sub-agent: no runner,
		// no lane wrapper, and no guidance block in the system prompt. A
		// second call with none (the owner left the config) still goes
		// through, so a resume can block what it can no longer dispatch.
		if len(cards) == 0 {
			return
		}
		ctx, cancel := context.WithCancel(context.Background())
		a.subs = &subAgents{lanes: subagent.NewLanes(), runs: map[string]*subRun{}, refused: map[string]bool{},
			hold: map[string]bool{}, pending: map[string]bool{}, root: ctx, cancel: cancel}
	}
	s := a.subs
	s.mu.Lock()
	s.cards, s.cws = cards, byName
	s.stopping = false
	// A resume after StopAllSubAgents finds the root context spent; every
	// dispatch from it would be born cancelled, so start a fresh one.
	if s.root.Err() != nil {
		s.root, s.cancel = context.WithCancel(context.Background())
	}
	s.mu.Unlock()
	a.bindPrimaryLane(primaryServer)
	a.SetEngineCards(cards)
	a.recomposeSystem("")
}

// bindPrimaryLane points the primary's lane at one server. It is re-bound on
// a /provider switch as well as at wiring time: bound once, a session that
// moved to another backend went on serialising its requests against the
// server it had left — and stopped serialising against the one it was
// actually talking to, which is the whole point of the lane.
func (a *Agent) bindPrimaryLane(server string) {
	s := a.subs
	if s == nil {
		return
	}
	s.mu.Lock()
	s.primary = server
	lanes := s.lanes
	s.mu.Unlock()
	a.laneAcquire = func(ctx context.Context) (func(), error) { return lanes.Acquire(ctx, server, true) }
}

// RebindPrimaryLane re-derives the primary's lane from the provider it is
// now talking to (SetProvider calls it). A provider this config does not
// name leaves the lane as it was: a wrong key is worse than a stale one,
// since it would serialise against nothing.
func (a *Agent) RebindPrimaryLane() {
	if a.subs == nil || a.Provider == nil || a.Cfg == nil {
		return
	}
	pc, ok := a.Cfg.Providers[a.Provider.Name()]
	if !ok {
		return
	}
	a.bindPrimaryLane(subagent.LaneKey(pc.BaseURL))
}

// prefillLane is the primary's own server at BACKGROUND priority. A
// speculative cache warm blocks nobody and must not outrank a sub-agent's
// real request on a shared server, which is what going through the
// primary's own laneAcquire (primary: true) would do. A consultation and
// the reviewer take primary priority deliberately — they block the request
// the person is watching; a prefill does not.
func (a *Agent) prefillLane() func(ctx context.Context) (func(), error) {
	s := a.subs
	if s == nil {
		return nil
	}
	s.mu.Lock()
	key, lanes := s.primary, s.lanes
	s.mu.Unlock()
	if key == "" {
		return nil
	}
	return func(ctx context.Context) (func(), error) { return lanes.Acquire(ctx, key, false) }
}

// laneFor is a lane wrapper for one co-worker's own server (spec §2.2:
// every co-worker maps to a lane, not only a sub-agent). nil when there is
// no runner, so nothing changes for a configuration with no sub-agent.
// primary is true for the primary's own errands — a consultation and the
// second-model review both block the request the person is watching.
func (a *Agent) laneFor(providerName string, primary bool) func(ctx context.Context) (func(), error) {
	s := a.subs
	if s == nil || a.Cfg == nil {
		return nil
	}
	pc, ok := a.Cfg.Providers[providerName]
	if !ok {
		return nil
	}
	key := subagent.LaneKey(pc.BaseURL)
	lanes := s.lanes
	return func(ctx context.Context) (func(), error) { return lanes.Acquire(ctx, key, primary) }
}

// StartSubAgents runs the resume pass, the startup resume gate and the
// first schedule. The UIs call it once approvals and events are wired: a
// dispatch before that would ask consent of nobody and print to nobody.
// buildAgent never calls it.
func (a *Agent) StartSubAgents() {
	if a.subs == nil {
		return
	}
	a.resumeSubAgents()
	a.startupResumeGate()
	a.ScheduleSubAgents()
}

// startupResumeGate asks the operator once, before the very first schedule,
// whether sub-agent work assigned in a previous session should start
// dispatching again (owner's ruling, 2026-09-22 §3.6 amendment — replacing
// the original "re-dispatch silently"). It runs only through StartSubAgents,
// so a step assigned or reassigned mid-session — by the operator or by the
// main model, through AssignOwner/SetScope — still dispatches at once: this
// gate has already run and never runs again this session.
//
// Nothing is asked when there is nothing ready to dispatch: a resumed step
// that cannot come back (resumeSubAgents, just above in StartSubAgents) is
// never a candidate here, and a configuration with no sub-agent never
// reaches this far at all (a.subs is nil).
func (a *Agent) startupResumeGate() {
	s := a.subs
	s.mu.Lock()
	if s.resumeAsked {
		s.mu.Unlock()
		return
	}
	s.resumeAsked = true
	s.mu.Unlock()
	steps := a.subSteps("sub-agent resume")
	if len(steps) == 0 {
		return
	}
	s.mu.Lock()
	cards, running, cws := s.cards, s.runningLocked(), s.cws
	s.mu.Unlock()
	ready, _ := subagent.Ready(steps, cards, running)
	if len(ready) == 0 {
		return
	}
	if a.Cfg.AutoApproveSubAgentResume {
		return
	}
	approved := a.Tools.Approve != nil && a.Tools.Approve("sub_agent_resume", resumeAskDetail(steps, ready, cws))
	if approved {
		return
	}
	s.mu.Lock()
	if s.declinedResume == nil {
		s.declinedResume = map[string]bool{}
	}
	for _, c := range ready {
		s.declinedResume[c.ID] = true
	}
	s.mu.Unlock()
	a.notice("sub-agent work left dormant; /agents start runs it")
}

// resumeAskDetail is the startup resume ask's body: one line per pending
// step, naming where it runs so an online co-worker stands out at a glance.
func resumeAskDetail(steps []subagent.Step, ready []subagent.Candidate, cws map[string]config.CoworkerConfig) string {
	var b strings.Builder
	b.WriteString("resume sub-agent work from the previous session?\n")
	for _, c := range ready {
		text, scope := c.ID, ""
		if step := findStep(steps, c.ID); step != nil {
			text, scope = step.Text, strings.Join(step.Scope, ", ")
		}
		where := "(online)"
		if cw, ok := cws[c.Owner]; ok && !cw.Online {
			where = fmt.Sprintf("(%s/%s)", cw.Provider, cw.Model)
		}
		fmt.Fprintf(&b, "  %s %s — %s %s, scope: %s\n", c.ID, text, c.Owner, where, scope)
	}
	b.WriteString("nothing has been sent to any model yet")
	return b.String()
}

// AllowSubAgentStart is /agents start: dispatches whatever a declined
// startup ask left dormant, without asking, and clears the flag so this
// session never asks it again on its own — without that a decline would be
// irreversible for the rest of the session. It returns the ids actually
// started, so the caller can report a count; nil (never asked, or nothing
// still assigned and ready) reports "no sub-agent work is waiting".
func (a *Agent) AllowSubAgentStart() []string {
	s := a.subs
	if s == nil {
		return nil
	}
	s.mu.Lock()
	ids := make([]string, 0, len(s.declinedResume))
	for id := range s.declinedResume {
		ids = append(ids, id)
	}
	s.declinedResume = nil
	s.mu.Unlock()
	if len(ids) == 0 {
		return nil
	}
	a.ScheduleSubAgents()
	s.mu.Lock()
	var started []string
	for _, id := range ids {
		if s.runs[id] != nil || s.pending[id] {
			started = append(started, id)
		}
	}
	s.mu.Unlock()
	sort.Strings(started)
	return started
}

// SubAgentsEnabled reports whether any sub-agent is configured.
func (a *Agent) SubAgentsEnabled() bool { return a.subs != nil }

// WaitSubAgents blocks until no sub-agent is running or ctx ends; the
// headless run uses it before draining hand-backs.
func (a *Agent) WaitSubAgents(ctx context.Context) {
	if a.subs == nil {
		return
	}
	for a.subAgentsBusy() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// subAgentsBusy reports whether any run is actually working — never a run
// parked on ask_main. A run waiting on askMain is "running" by every other
// measure (SubAgentStates/RunningSubAgents count it, so the bottom line and
// /agents still show it), but it is waiting on the main model, which a
// headless run answers itself through the very round WaitSubAgents guards:
// counting it as busy here would deadlock that round against itself.
func (a *Agent) subAgentsBusy() bool {
	s := a.subs
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) > 0 {
		return true
	}
	for _, r := range s.runs {
		if r.question == "" {
			return true
		}
	}
	return false
}

// SubAgentReport is every hand-back so far, for run --json.
func (a *Agent) SubAgentReport() []subagent.HandBack {
	if a.subs == nil {
		return nil
	}
	a.subs.mu.Lock()
	defer a.subs.mu.Unlock()
	return append([]subagent.HandBack(nil), a.subs.finished...)
}

// resumeSubAgents handles assigned steps an earlier session interrupted
// but this configuration cannot re-dispatch: they are blocked, handed
// back, and noticed, so nobody waits for something that will not come.
func (a *Agent) resumeSubAgents() {
	steps := a.subSteps("sub-agent resume")
	if len(steps) == 0 {
		return
	}
	s := a.subs
	s.mu.Lock()
	cards := s.cards
	running := s.runningLocked()
	s.mu.Unlock()
	_, waiting := subagent.Ready(steps, cards, running)
	for _, w := range waiting {
		step := findStep(steps, w.ID)
		if step == nil || !step.Interrupted {
			continue
		}
		hb := subagent.HandBack{Node: w.ID, Owner: w.Owner, Status: "blocked", Reason: w.Reason, Files: step.Touched}
		a.engineDo("sub-agent resume", func(st *engine.Store) { _ = st.CloseAs(w.ID, w.Owner, "blocked", w.Reason) })
		a.Enqueue(handBackLine(hb))
		a.notice("sub-agent %s cannot resume %s: %s", w.Owner, w.ID, w.Reason)
		if a.Events.OnSubAgentEnd != nil {
			a.Events.OnSubAgentEnd(hb)
		}
	}
}

func findStep(steps []subagent.Step, id string) *subagent.Step {
	for i := range steps {
		if steps[i].ID == id {
			return &steps[i]
		}
		if s := findStep(steps[i].Children, id); s != nil {
			return s
		}
	}
	return nil
}

// subSteps is the ready rule's view of the tree, behind the engine fence
// like every other call the agent makes into the store: a panicking store
// detaches the engine and yields no steps, and the scheduler then simply
// does nothing rather than ending the session.
//
// It is deliberately read *before* subAgents.mu is taken by every caller.
// The store has its own lock, so holding mu across the read would buy no
// consistency — and it would put engineDo's notice, which a UI handles on
// its own goroutine, underneath the runner's mutex.
func (a *Agent) subSteps(op string) []subagent.Step {
	var steps []subagent.Step
	a.engineDo(op, func(st *engine.Store) { steps = st.Steps() })
	return steps
}

// runningLocked is the ready rule's "already taken" set: the runs in
// flight and the candidates a schedule has reserved but not yet installed.
func (s *subAgents) runningLocked() map[string]bool {
	m := make(map[string]bool, len(s.runs)+len(s.pending))
	for id := range s.runs {
		m[id] = true
	}
	for id := range s.pending {
		m[id] = true
	}
	return m
}

// ScheduleSubAgents dispatches every ready step up to sub_agents.
// max_concurrent (spec §2.1, §2.3). Safe from any goroutine; never blocks
// on a model or a modal.
//
// Three phases, because of the invariant on subAgents: decide and reserve
// under mu, build each dispatch unlocked — that is where the store and the
// workspace are read, and where engineDo may raise a notice a UI answers by
// calling straight back in here — then install and start under mu again.
func (a *Agent) ScheduleSubAgents() {
	s := a.subs
	if s == nil {
		return
	}
	steps := a.subSteps("sub-agent schedule")
	if len(steps) == 0 {
		return
	}
	type pick struct {
		c  subagent.Candidate
		cw config.CoworkerConfig
	}
	var picks []pick
	s.mu.Lock()
	if !s.stopping {
		ready, _ := subagent.Ready(steps, s.cards, s.runningLocked())
		for _, c := range ready {
			if len(s.runs)+len(s.pending) >= a.Cfg.SubAgents.MaxConcurrent {
				break
			}
			if s.hold[c.ID] || s.declinedResume[c.ID] {
				continue
			}
			s.pending[c.ID] = true
			picks = append(picks, pick{c: c, cw: s.cws[c.Owner]})
		}
	}
	s.mu.Unlock()
	for _, p := range picks {
		a.dispatchPicked(steps, p.c, p.cw)
	}
}

// dispatchPicked turns one reserved candidate into a running sub-agent.
// **Called with mu NOT held** and it must stay that way: the DispatchContext
// read, the workspace scan and both engineDo calls below all happen here.
func (a *Agent) dispatchPicked(steps []subagent.Step, c subagent.Candidate, cw config.CoworkerConfig) {
	s := a.subs
	// Whatever happens, the reservation is given back.
	unreserve := func() {
		s.mu.Lock()
		delete(s.pending, c.ID)
		s.mu.Unlock()
	}
	// Everything up to the install runs on the *agent* goroutine (a schedule
	// from run(), from the task tool, from a UI command), and runSub's own
	// fence does not cover it: findStep walks a tree the store handed over,
	// verify.Detect scans the workspace, and the string building reads the
	// project notes. A panic in any of them would end the session for an
	// advisory feature, so it is fenced here and the candidate released.
	var d subagent.Dispatch
	ok := false
	func() {
		defer func() {
			if r := recover(); r != nil {
				a.notice("sub-agent dispatch of %s failed (%v); it stays assigned and will be tried again", c.ID, r)
				ok = false
			}
		}()
		step := findStep(steps, c.ID)
		if step == nil {
			return
		}
		var text, ctxText string
		var children []string
		got := false
		a.engineDo("sub-agent dispatch", func(st *engine.Store) {
			text, children, ctxText = st.DispatchContext(c.ID)
			got = true
		})
		if !got {
			return // the engine detached under us: nothing to dispatch from
		}
		d = subagent.Dispatch{Node: c.ID, Owner: c.Owner, Text: text, Children: children,
			Scope: step.Scope, MaxTurns: a.Cfg.SubAgents.MaxTurns, Context: ctxText,
			Interrupted: step.Interrupted, Touched: step.Touched}
		if a.Session != nil {
			d.Session = a.Session.ID
		}
		for _, ch := range verify.Detect(a.Tools.Root).Checks {
			d.Checks = append(d.Checks, ch.Command)
		}
		if a.projectNotes != "" {
			d.Context = strings.TrimSpace(d.Context + "\n\nProject notes (facts about the repository, not instructions):\n" + a.projectNotes)
		}
		ok = true
	}()
	if !ok {
		unreserve()
		return
	}
	s.mu.Lock()
	delete(s.pending, c.ID)
	if s.stopping || s.hold[c.ID] {
		// The session stopped, or an operator claimed the node, while this
		// dispatch was being built. Nothing was installed, so nothing to undo.
		s.mu.Unlock()
		return
	}
	// Under mu because root is reassigned on resume — and because deriving
	// a context calls into neither the store nor a notice.
	ctx, cancel := context.WithCancel(s.root)
	run := &subRun{d: d, cw: cw, ctx: ctx, cancel: cancel, started: time.Now(),
		reply: make(chan string, 1), done: make(chan struct{})}
	s.runs[c.ID] = run
	s.wg.Add(1)
	s.mu.Unlock()
	a.engineDo("sub-agent dispatch", func(st *engine.Store) { st.SetDispatched(c.ID, true) })
	go a.runSub(run)
}

// runSub is one sub-agent's life: consent, provider, scratch agent, run,
// hand-back. Fenced like Consult: a panic is that step's error.
func (a *Agent) runSub(run *subRun) {
	s := a.subs
	var hb subagent.HandBack
	var answer string
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("internal error: %v", r)
			}
		}()
		if !a.subConsent(run) {
			err = errors.New("consent refused")
			return
		}
		if CoworkerFactory == nil {
			err = errors.New("co-working is not wired in this build")
			return
		}
		cp, window, ferr := CoworkerFactory(run.ctx, a.Cfg, run.cw)
		if ferr != nil {
			err = ferr
			return
		}
		a.noteSharedServer(run.cw)
		scratch := a.subAgent(cp, run, window)
		s.mu.Lock()
		run.scratch = scratch
		run.reg = scratch.Tools
		// The scope may have been widened between subAgent's read of it and
		// this install, and SetScope had no registry to push it to then, so
		// the current value is re-applied here. It happens INSIDE the hold:
		// applying it after the unlock left a gap in which a SetScope that
		// had just widened the registry was overwritten with the value from
		// before the widening — the registry ending up narrower than
		// run.d.Scope claims, after the sub-agent had been told otherwise.
		// reg.SetScope takes its own mutex with no path back to subs.mu.
		scratch.Tools.SetScope(run.d.Scope)
		s.mu.Unlock()
		if a.Events.OnSubAgentStart != nil {
			a.Events.OnSubAgentStart(run.d)
		}
		answer, err = scratch.Run(run.ctx, "Begin your step.")
	}()
	hb = a.settleSub(run, answer, err)
	s.mu.Lock()
	delete(s.runs, run.d.Node)
	s.finished = append(s.finished, hb)
	s.mu.Unlock()
	// The queue first, the event second: a UI answers OnSubAgentEnd by
	// starting a turn for whatever is queued when the main model is idle
	// (spec §2.6), and it can only do that once the hand-back is in the
	// queue to be found. OnSubAgentAsk is already ordered this way.
	if hb.Status != "interrupted" {
		a.Enqueue(handBackLine(hb))
	}
	if a.Events.OnSubAgentEnd != nil {
		a.Events.OnSubAgentEnd(hb)
	}
	a.refreshKeepAlive()
	// The slot this run held is free, so whatever was waiting on it goes
	// now — unless the session is stopping, in which case ScheduleSubAgents
	// does nothing. done and wg are released last, after that schedule, so
	// StopAllSubAgents' wait and AssignOwner's wait both mean "nothing of
	// this run is still in flight".
	a.ScheduleSubAgents()
	close(run.done)
	s.wg.Done()
}

// settleSub turns a run's outcome into the node's state and a HandBack.
func (a *Agent) settleSub(run *subRun, answer string, err error) subagent.HandBack {
	s := a.subs
	hb := subagent.HandBack{Node: run.d.Node, Owner: run.d.Owner, Elapsed: time.Since(run.started)}
	// One read of the run's mutable state, under the lock that guards it.
	s.mu.Lock()
	scratch, question, stopReason := run.scratch, run.question, run.stopReason
	interrupt, timedOut := run.interrupt, run.timedOut
	s.mu.Unlock()
	if scratch != nil {
		hb.Calls = scratch.Usage().ToolCalls
	}
	switch {
	case interrupt:
		hb.Status = "interrupted"
		files := []string{}
		a.engineDo("sub-agent touched", func(st *engine.Store) { files = st.Touched(run.d.Node) })
		// The engine formats the note itself, so the shape Steps() parses
		// back is written in exactly one place.
		a.engineDo("sub-agent interrupt", func(st *engine.Store) {
			_ = st.Interrupt(run.d.Node, time.Now(), hb.Calls, files)
		})
		hb.Files = files
		return hb
	case timedOut:
		hb.Status, hb.Reason = "blocked", "no answer to: "+question
	case stopReason != "":
		hb.Status, hb.Reason = "blocked", stopReason
	case err != nil:
		hb.Status, hb.Reason = "blocked", err.Error()
		if strings.HasPrefix(err.Error(), "stopped after ") && strings.Contains(err.Error(), "tool turns") {
			hb.Reason = fmt.Sprintf("turn cap of %d reached", a.Cfg.SubAgents.MaxTurns)
		}
	default:
		hb.Status, hb.Summary = "done", strings.TrimSpace(sanitizeAdvice(answer))
	}
	a.engineDo("sub-agent close", func(st *engine.Store) {
		_ = st.CloseAs(run.d.Node, run.d.Owner, hb.Status, hb.Reason)
		hb.Files = st.Touched(run.d.Node)
	})
	return hb
}

// subConsent asks once per session before an online sub-agent sees any
// code (spec §2.3); the detail's first line keeps the "coworker: name (…)"
// shape ConsentCoworker parses for the modal's "a".
func (a *Agent) subConsent(run *subRun) bool {
	cw := run.cw
	s := a.subs
	if !cw.Online || a.allowedFor(cw.Name) {
		return true
	}
	s.mu.Lock()
	refused := s.refused[cw.Name]
	s.mu.Unlock()
	if refused {
		return false
	}
	if a.Cfg.AutoApproveConsult {
		a.allow(cw.Name)
		return true
	}
	if a.Tools.Approve == nil {
		return false
	}
	detail := fmt.Sprintf("coworker: %s (%s/%s)\norigin: sub-agent %s\nscope: %s\nstep: %s\nit will read the repository and write files under the scope; the step's text, the project notes and the task notes go with it",
		cw.Name, cw.Provider, cw.Model, run.d.Node, strings.Join(run.d.Scope, ", "), run.d.Text)
	if a.Tools.Approve("consult", detail) {
		return true
	}
	if run.ctx.Err() == nil {
		s.mu.Lock()
		s.refused[cw.Name] = true
		s.mu.Unlock()
	}
	return false
}

// subAgent builds the scratch agent for one dispatch: consultAgent's
// shape with a scoped, write-capable registry, ask_main and a subtree
// task tool, evidence filed under the dispatched root, and its own lane.
func (a *Agent) subAgent(cp provider.Provider, run *subRun, window int) *Agent {
	// Under mu: d.Scope is written by Agent.SetScope from another goroutine
	// once the run exists.
	a.subs.mu.Lock()
	d := run.d
	d.Scope = append([]string(nil), run.d.Scope...)
	a.subs.mu.Unlock()
	label := fmt.Sprintf("%s (%s)", d.Owner, d.Node)
	reg := a.Tools.Scoped(d.Scope, d.Checks, label)
	reg.AddTool(tools.NewAskMain(func(ctx context.Context, q string) (string, error) { return a.askMain(run, ctx, q) }))
	reg.AddTool(tools.NewTaskUnder(a.TaskLedger(), d.Node))
	cfg := *a.Cfg
	cfg.MaxTurns = d.MaxTurns
	cfg.VerifyOnDone, cfg.ReviewOnDone = false, false
	prof := profiles.Detect(run.cw.Model)
	compat := prof.Compat == "always"
	switch cfg.CompatToolCalls {
	case "always":
		compat = true
	case "never":
		compat = false
	}
	scratch := &Agent{
		Cfg: &cfg, Provider: cp, Model: run.cw.Model, Tools: reg,
		Profile: prof, compat: compat, projectNotes: a.projectNotes,
		retryBase: a.retryBase, stallAfter: a.stallAfter,
		Engine: a.Engine,
	}
	node := d.Node
	scratch.observeFn = func(ev engine.Event) string {
		var footer, at string
		a.engineDo("sub-agent observe", func(st *engine.Store) {
			footer = st.ObserveFor(node, ev)
			at = st.DoingUnderID(node)
		})
		if at != "" {
			// After engineDo, never inside it: no store call is ever made
			// under the runner's mutex (the invariant at the top of this file).
			a.subs.mu.Lock()
			run.at = at
			a.subs.mu.Unlock()
		}
		return footer
	}
	scratch.Events = Events{OnTransient: func(msg string) { a.transient("%s", msg) }}
	a.subs.mu.Lock()
	server := a.subs.cards[d.Owner].Server
	a.subs.mu.Unlock()
	lanes := a.subs.lanes
	scratch.laneAcquire = func(ctx context.Context) (func(), error) { return lanes.Acquire(ctx, server, false) }
	scratch.knownTools = map[string]bool{}
	for _, n := range reg.Names() {
		scratch.knownTools[n] = true
	}
	sys := SubAgentFrame + "\n\n" + renderDispatch(d)
	if compat || cfg.CompatToolCalls == "auto" {
		full := BuildSystemPrompt(reg.Specs(), true, "")
		if i := strings.Index(full, "Tool calling format"); i >= 0 {
			sys += "\n\n" + full[i:]
		}
	}
	scratch.systemOverride = sys
	budget, reserve, charsPerToken := a.History.Scalars()
	scratch.History = NewHistory(sys, budget)
	scratch.History.Reserve = reserve
	scratch.History.CharsPerToken = charsPerToken
	if window > 0 {
		scratch.Cfg.ContextTokens = 0
		scratch.ApplyWindow(window)
	}
	return scratch
}

// askMain parks the sub-agent on one question (spec §2.7).
func (a *Agent) askMain(run *subRun, ctx context.Context, q string) (string, error) {
	s := a.subs
	// An answer that lost the race with its own timeout is still in the
	// buffer; it belongs to the question that timed out, not to this one.
	// Drained here, before the question is registered: while question is ""
	// ReplyAsk refuses, so no legitimate answer can be in flight yet.
	select {
	case <-run.reply:
	default:
	}
	s.mu.Lock()
	if run.question != "" {
		s.mu.Unlock()
		return "", errors.New("a question is already waiting for an answer")
	}
	run.question = q
	s.mu.Unlock()
	a.engineDo("sub-agent ask", func(st *engine.Store) { _ = st.Note(run.d.Node, "asked: "+q, "", false, false) })
	a.Enqueue(fmt.Sprintf("sub-agent %s asks about %s: %s", run.d.Owner, run.d.Node, q))
	if a.Events.OnSubAgentAsk != nil {
		a.Events.OnSubAgentAsk(subagent.Ask{Node: run.d.Node, Owner: run.d.Owner, Question: q})
	}
	timeout := time.Duration(a.Cfg.SubAgents.AskTimeout) * time.Second
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case r := <-run.reply:
		return r, nil
	case <-timer.C:
		s.mu.Lock()
		run.timedOut = true
		s.mu.Unlock()
		run.cancel()
		return "", fmt.Errorf("no answer within %s", timeout)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// ReplyAsk answers a sub-agent's open question (task reply, /task reply).
func (a *Agent) ReplyAsk(id, text string) error {
	s := a.subs
	if s == nil {
		return errors.New("no sub-agents are configured")
	}
	s.mu.Lock()
	run, ok := s.runs[id]
	if !ok || run.question == "" {
		s.mu.Unlock()
		return fmt.Errorf("no sub-agent is asking about %s", id)
	}
	run.question = ""
	s.mu.Unlock()
	a.engineDo("sub-agent reply", func(st *engine.Store) { _ = st.Note(id, "reply: "+text, "", false, false) })
	run.reply <- text
	return nil
}

// AssignOwner is the UI's assignment path: a run parked on an ask is
// stopped and its node returned to todo before the owner changes; a run
// that is working is refused (spec §3.5).
func (a *Agent) AssignOwner(id, owner string, pinned bool) error {
	s := a.subs
	if s != nil {
		s.mu.Lock()
		if s.pending[id] {
			// Reserved by a schedule that is building its dispatch right
			// now: there is no run to stop yet and no done channel to wait
			// on. Said plainly rather than waited on — a retry a moment
			// later gets the ordinary "is being worked by" answer.
			s.mu.Unlock()
			return fmt.Errorf("%s is being dispatched; try again in a moment", id)
		}
		run, ok := s.runs[id]
		if ok && run.question == "" {
			s.mu.Unlock()
			return fmt.Errorf("%s is being worked by %s; /agents stop %s first", id, run.d.Owner, run.d.Owner)
		}
		if ok {
			// Held until the owner is rewritten: the run's own parting
			// schedule would otherwise pick the node straight back up, still
			// owned by the sub-agent the operator is taking it from.
			s.hold[id] = true
			run.interrupt = true
			run.cancel()
		}
		s.mu.Unlock()
		if ok {
			<-run.done
		}
	}
	var err error
	ran := false
	a.engineDo("task owner", func(st *engine.Store) { ran = true; err = st.SetOwner(id, owner, pinned) })
	if !ran {
		err = errEngineGone
	}
	// The hold is released before the schedule, not after: an operator who
	// reassigned the step to another sub-agent means it to start now. The
	// same id's startup-decline mark is cleared here too: reassigning it is
	// a fresh initiation of exactly this step, the case StartSubAgents'
	// doc comment carves out of the startup-only ask.
	if s != nil {
		s.mu.Lock()
		delete(s.hold, id)
		delete(s.declinedResume, id)
		s.mu.Unlock()
		if err == nil {
			a.ScheduleSubAgents()
		}
	}
	return err
}

// errEngineGone is what the assignment paths answer when the task record is
// absent or has been detached by a panic: engineDo simply does not run the
// closure, so without this they reported success having done nothing.
var errEngineGone = errors.New("the task record is not available; the assignment was not made")

// SetScope is the UI's and the model's scope path (spec §2.7): it widens a
// running sub-agent's confinement for real and answers its parked ask.
func (a *Agent) SetScope(id string, paths []string) error {
	clean, err := subagent.CleanScope(paths)
	if err != nil {
		return err
	}
	s := a.subs
	var run *subRun
	var had []string
	if s != nil {
		s.mu.Lock()
		if s.pending[id] {
			// Reserved by a schedule that is building its dispatch from a
			// snapshot of the tree taken a moment ago: a scope written now
			// would not reach that dispatch. Said plainly, as AssignOwner
			// does, rather than silently landing on the wrong side of it.
			s.mu.Unlock()
			return fmt.Errorf("%s is being dispatched; try again in a moment", id)
		}
		if run = s.runs[id]; run != nil {
			had = append([]string(nil), run.d.Scope...)
		}
		s.mu.Unlock()
	}
	if run != nil && !subagent.Within(had, clean) {
		// Narrowing a running sub-agent's scope would leave files it has
		// already written outside what it may still fix. §2.7 names
		// widening, and only widening, as the way a scope changes mid-run.
		return fmt.Errorf("%s is being worked by %s; a running sub-agent's scope may only be widened (it already has %s)",
			id, run.d.Owner, strings.Join(had, ", "))
	}
	ran := false
	a.engineDo("task scope", func(st *engine.Store) { ran = true; err = st.SetScope(id, clean) })
	if !ran {
		return errEngineGone
	}
	if err != nil {
		return err
	}
	if s == nil {
		return nil
	}
	// The registry is built once from a copy of the slice, so without this
	// the widened scope never reaches the sub-agent that asked for it — and
	// the ask below told it that it had.
	s.mu.Lock()
	parked := false
	if run != nil {
		run.d.Scope = clean
		parked = run.question != ""
	}
	var reg *tools.Registry
	if run != nil {
		reg = run.reg
	}
	// A scope written to this id is the operator's or the model's own fresh
	// initiation of it, same as AssignOwner: it must not stay dormant behind
	// a startup decline this call had nothing to do with.
	delete(s.declinedResume, id)
	s.mu.Unlock()
	if reg != nil {
		reg.SetScope(clean)
	}
	if parked {
		_ = a.ReplyAsk(id, "scope widened to "+strings.Join(clean, ", "))
	}
	a.ScheduleSubAgents()
	return nil
}

// StopSubAgent cancels every run of one co-worker (/agents stop <name>).
func (a *Agent) StopSubAgent(name string) error {
	s := a.subs
	if s == nil {
		return errors.New("no sub-agents are configured")
	}
	s.mu.Lock()
	var runs []*subRun
	for _, r := range s.runs {
		if r.d.Owner == name {
			runs = append(runs, r)
		}
	}
	for _, r := range runs {
		r.stopReason = "stopped by operator"
	}
	s.mu.Unlock()
	if len(runs) == 0 {
		return fmt.Errorf("%s is not running anything", name)
	}
	for _, r := range runs {
		r.cancel()
	}
	return nil
}

// StopAllSubAgents interrupts every run at session end (spec §3.5) and
// waits, bounded, for the nodes to be reset and the store flushed.
func (a *Agent) StopAllSubAgents(reason string) {
	s := a.subs
	if s == nil {
		return
	}
	s.mu.Lock()
	// Set before anything is cancelled: an interrupted node goes back to
	// todo, and the finishing run's own schedule must not take it up again.
	s.stopping = true
	for _, r := range s.runs {
		r.interrupt = true
		r.cancel()
	}
	rootCancel := s.cancel // reassigned on resume; read it here, use it below
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		a.notice("sub-agents did not stop within 5s (%s)", reason)
	}
	// After the wait, not before: every run was cancelled individually
	// above, and this ends the context they all derive from — the escape
	// hatch for a straggler the bounded wait gave up on. A later
	// EnableSubAgents makes a fresh root, which is what lets a resumed
	// session dispatch again.
	rootCancel()
	a.engineDo("sub-agent flush", func(st *engine.Store) { _ = st.Flush() })
}

// SubAgentStates is /agents: every card, then one row per assigned step
// that is running or waiting.
func (a *Agent) SubAgentStates() []SubAgentState {
	s := a.subs
	if s == nil {
		return nil
	}
	steps := a.subSteps("sub-agent states")
	s.mu.Lock()
	var out []SubAgentState
	names := make([]string, 0, len(s.cws))
	for n := range s.cws {
		names = append(names, n)
	}
	sort.Strings(names)
	byOwner := map[string][]*subRun{}
	for _, r := range s.runs {
		byOwner[r.d.Owner] = append(byOwner[r.d.Owner], r)
	}
	for _, n := range names {
		cw := s.cws[n]
		row := SubAgentState{Name: n, Model: cw.Model, Provider: cw.Provider, Online: cw.Online, SubAgent: true, MaxScope: cw.MaxScope, State: "idle"}
		if runs := byOwner[n]; len(runs) > 0 {
			r := runs[0]
			row.Node, row.Since = r.d.Node, r.started
			row.At = row.Node
			if r.scratch != nil {
				row.Calls = r.scratch.Usage().ToolCalls
			}
			switch {
			case r.question != "":
				row.State = "asking: " + r.question
			case r.scratch == nil:
				row.State = "starting"
			default:
				row.State = "working"
			}
		}
		out = append(out, row)
	}
	if len(steps) > 0 {
		_, waiting := subagent.Ready(steps, s.cards, s.runningLocked())
		for _, w := range waiting {
			out = append(out, SubAgentState{Name: w.Owner, SubAgent: true, Node: w.ID, State: "waiting: " + w.Reason})
		}
	}
	s.mu.Unlock()
	// DoingUnderID refines a running row's At from the dispatched root id
	// to the doing node inside its subtree. Behind the fence like every
	// other store call in this package, and only after subAgents.mu is
	// released — the invariant this file documents at the top, no call
	// into the engine store is ever made while mu is held. This is not
	// belt-and-braces: SubAgentStates is reached from RunningSubAgents,
	// which bottomLine/compactBottomLine call on effectively every frame,
	// so a panic walking a corrupted tree must detach the engine with a
	// notice rather than take the render goroutine down with it.
	a.engineDo("sub-agent at", func(st *engine.Store) {
		for i := range out {
			if out[i].At == "" {
				continue // idle or waiting: nothing running to refine
			}
			if at := st.DoingUnderID(out[i].Node); at != "" {
				out[i].At = at
			}
		}
	})
	return out
}

// RunningSubAgents is the bottom line's view: running rows only, and
// nothing else.
//
// bottomLine/compactBottomLine call this from View(), which Bubble Tea runs
// after every message, for every attached terminal. Going through
// SubAgentStates cost a deep copy of the whole task tree (Steps) plus a
// full Ready evaluation per keystroke per terminal, on the render
// goroutine, for a line that only ever shows what is already running. The
// runner's own mutex holds everything this needs, so take it and leave: no
// store call at all. SubAgentStates keeps the expensive view for /agents,
// where the waiting rows and their reasons are the point.
func (a *Agent) RunningSubAgents() []SubAgentState {
	s := a.subs
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SubAgentState, 0, len(s.runs))
	for _, r := range s.runs {
		cw := s.cws[r.d.Owner]
		row := SubAgentState{Name: r.d.Owner, Model: cw.Model, Provider: cw.Provider,
			Online: cw.Online, SubAgent: true, MaxScope: cw.MaxScope,
			Node: r.d.Node, At: r.d.Node, Since: r.started}
		if r.at != "" {
			row.At = r.at
		}
		if r.scratch != nil {
			row.Calls = r.scratch.Usage().ToolCalls
		}
		switch {
		case r.question != "":
			row.State = "asking: " + r.question
		case r.scratch == nil:
			row.State = "starting"
		default:
			row.State = "working"
		}
		out = append(out, row)
	}
	// Map order is not an order; the bottom line must not shuffle between
	// frames.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Node < out[j].Node
	})
	return out
}

// ownedNote is the footer a main-model write gets inside a running
// sub-agent's scope (spec §3.7).
func (s *subAgents) ownedNote(tool string, args map[string]any) string {
	if tool != "write_file" && tool != "edit_file" {
		return ""
	}
	// The same aliases fs.go accepts ("path", "file", "filename"): a write
	// the tool honoured under "file" must not slip past the ownership note
	// just because this one looked only at "path".
	var p string
	for _, k := range []string{"path", "file", "filename"} {
		if v, ok := args[k].(string); ok && strings.TrimSpace(v) != "" {
			p = v
			break
		}
	}
	if p == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		for _, sc := range r.d.Scope {
			if subagent.InScope([]string{sc}, p) {
				return fmt.Sprintf("note: %s is owned by %s (%s) until it hands back", sc, r.d.Owner, r.d.Node)
			}
		}
	}
	return ""
}
