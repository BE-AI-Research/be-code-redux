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
// mu guards every field below and every mutable field of every subRun. It
// is never held across a model call, an approval or a wait on a run, and
// nothing that holds the engine's own lock ever takes it, so the two orders
// cannot cross.
type subAgents struct {
	mu      sync.Mutex
	cards   map[string]subagent.Card
	cws     map[string]config.CoworkerConfig
	lanes   *subagent.Lanes
	runs    map[string]*subRun // by node id
	refused map[string]bool    // online consent refused this session, by name
	// stopping is set by StopAllSubAgents: an interrupted node goes back to
	// todo, and without this the schedule a finishing run makes on its way
	// out would dispatch it again after the session had asked everything to
	// stop. EnableSubAgents clears it, which is what makes resume work.
	stopping bool
	// hold names nodes that must not be dispatched while an operator is
	// changing them (AssignOwner stops a parked run, then rewrites the
	// owner; a re-dispatch in between would take the node back).
	hold    map[string]bool
	root    context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	primary string // the primary's lane key
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
	scratch    *Agent
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
			hold: map[string]bool{}, root: ctx, cancel: cancel}
	}
	s := a.subs
	s.mu.Lock()
	s.cards, s.cws, s.primary = cards, byName, primaryServer
	s.stopping = false
	s.mu.Unlock()
	a.laneAcquire = func(ctx context.Context) (func(), error) { return s.lanes.Acquire(ctx, primaryServer, true) }
	a.engineDo("sub-agent cards", func(st *engine.Store) { st.SetCards(cards) })
	a.recomposeSystem("")
}

// StartSubAgents runs the resume pass and the first schedule. The UIs
// call it once approvals and events are wired: a dispatch before that
// would ask consent of nobody and print to nobody. buildAgent never
// calls it.
func (a *Agent) StartSubAgents() {
	if a.subs == nil {
		return
	}
	a.resumeSubAgents()
	a.ScheduleSubAgents()
}

// SubAgentsEnabled reports whether any sub-agent is configured.
func (a *Agent) SubAgentsEnabled() bool { return a.subs != nil }

// resumeSubAgents handles assigned steps an earlier session interrupted
// but this configuration cannot re-dispatch: they are blocked, handed
// back, and noticed, so nobody waits for something that will not come.
func (a *Agent) resumeSubAgents() {
	st := a.engine()
	if st == nil {
		return
	}
	s := a.subs
	s.mu.Lock()
	cards := s.cards
	running := s.runningLocked()
	s.mu.Unlock()
	_, waiting := subagent.Ready(st.Steps(), cards, running)
	for _, w := range waiting {
		step := findStep(st.Steps(), w.ID)
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

func (s *subAgents) runningLocked() map[string]bool {
	m := map[string]bool{}
	for id := range s.runs {
		m[id] = true
	}
	return m
}

// ScheduleSubAgents dispatches every ready step up to sub_agents.
// max_concurrent (spec §2.1, §2.3). Safe from any goroutine; never blocks
// on a model or a modal.
func (a *Agent) ScheduleSubAgents() {
	s := a.subs
	st := a.engine()
	if s == nil || st == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return
	}
	ready, _ := subagent.Ready(st.Steps(), s.cards, s.runningLocked())
	for _, c := range ready {
		if len(s.runs) >= a.Cfg.SubAgents.MaxConcurrent {
			return
		}
		if s.hold[c.ID] {
			continue
		}
		a.dispatchLocked(c)
	}
}

func (a *Agent) dispatchLocked(c subagent.Candidate) {
	s := a.subs
	st := a.engine()
	step := findStep(st.Steps(), c.ID)
	if step == nil {
		return
	}
	text, children, ctxText := st.DispatchContext(c.ID)
	d := subagent.Dispatch{Node: c.ID, Owner: c.Owner, Text: text, Children: children,
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
	ctx, cancel := context.WithCancel(s.root)
	run := &subRun{d: d, cw: s.cws[c.Owner], ctx: ctx, cancel: cancel, started: time.Now(),
		reply: make(chan string, 1), done: make(chan struct{})}
	s.runs[c.ID] = run
	st.SetDispatched(c.ID, true)
	s.wg.Add(1)
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
		s.mu.Unlock()
		if a.Events.OnSubAgentStart != nil {
			a.Events.OnSubAgentStart(run.d)
		}
		answer, err = scratch.Run(run.ctx, "Begin your step.")
	}()
	hb = a.settleSub(run, answer, err)
	s.mu.Lock()
	delete(s.runs, run.d.Node)
	s.mu.Unlock()
	if a.Events.OnSubAgentEnd != nil {
		a.Events.OnSubAgentEnd(hb)
	}
	if hb.Status != "interrupted" {
		a.Enqueue(handBackLine(hb))
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
	st := a.engine()
	switch {
	case interrupt:
		hb.Status = "interrupted"
		files := []string{}
		if st != nil {
			files = st.Touched(run.d.Node)
		}
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
	d := run.d
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
		var footer string
		a.engineDo("sub-agent observe", func(st *engine.Store) { footer = st.ObserveFor(node, ev) })
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
	a.engineDo("task owner", func(st *engine.Store) { err = st.SetOwner(id, owner, pinned) })
	// The hold is released before the schedule, not after: an operator who
	// reassigned the step to another sub-agent means it to start now.
	if s != nil {
		s.mu.Lock()
		delete(s.hold, id)
		s.mu.Unlock()
		if err == nil {
			a.ScheduleSubAgents()
		}
	}
	return err
}

// SetScope is the UI's scope path; it also answers a parked ask.
func (a *Agent) SetScope(id string, paths []string) error {
	var err error
	a.engineDo("task scope", func(st *engine.Store) { err = st.SetScope(id, paths) })
	if err != nil {
		return err
	}
	if s := a.subs; s != nil {
		s.mu.Lock()
		run, ok := s.runs[id]
		parked := ok && run.question != ""
		s.mu.Unlock()
		if parked {
			_ = a.ReplyAsk(id, "scope widened to "+strings.Join(paths, ", "))
		}
		a.ScheduleSubAgents()
	}
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
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		a.notice("sub-agents did not stop within 5s (%s)", reason)
	}
	a.engineDo("sub-agent flush", func(st *engine.Store) { _ = st.Flush() })
}

// SubAgentStates is /agents: every card, then one row per assigned step
// that is running or waiting.
func (a *Agent) SubAgentStates() []SubAgentState {
	s := a.subs
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
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
	if st := a.engine(); st != nil {
		_, waiting := subagent.Ready(st.Steps(), s.cards, s.runningLocked())
		for _, w := range waiting {
			out = append(out, SubAgentState{Name: w.Owner, SubAgent: true, Node: w.ID, State: "waiting: " + w.Reason})
		}
	}
	return out
}

// RunningSubAgents is the bottom line's view: running rows only.
func (a *Agent) RunningSubAgents() []SubAgentState {
	var out []SubAgentState
	for _, st := range a.SubAgentStates() {
		if st.Node != "" && !strings.HasPrefix(st.State, "waiting") {
			out = append(out, st)
		}
	}
	return out
}

// ownedNote is the footer a main-model write gets inside a running
// sub-agent's scope (spec §3.7).
func (s *subAgents) ownedNote(tool string, args map[string]any) string {
	if tool != "write_file" && tool != "edit_file" {
		return ""
	}
	p, _ := args["path"].(string)
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
