package agent

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/schedule"
	"github.com/brown-enterprises/be-code/internal/store"
)

// scheduler owns a session's scheduled events (schedules spec §1.3): the
// project's recurring schedules (schedules.md, which always wins and is
// reread on every wake) and the session's one-off timers. One goroutine,
// one timer armed for the nearest due time; a due event is queued, never run
// here. It is started by a UI (StartSchedules[Async]) and never by
// buildAgent, so nothing fires before approvals and events are wired.
//
// Lock order: s.mu, then the Agent's sessionMu/saveMu (saveTimersLocked
// goes through UpdateSession), inbox.mu (fireNow, queueDue) and fireMu
// (queueDue's armedID); inbox.mu before fireMu (DrainForTurn). Nothing
// holding any of those takes s.mu, and nothing under s.mu calls an Events
// callback: a notice raised under the lock is held in
// s.notes and delivered by unlock, because a UI's OnNotice may itself wait
// on something that is waiting on s.mu.
// pendingEvent is one queued event: when it was queued, and whether a
// person asked for it by hand (/schedule run), which may run a paused one.
type pendingEvent struct {
	queuedAt time.Time
	manual   bool
}

// nextSnap is the soonest active schedule with no event already waiting, as
// last published by publishNextLocked. A zero value (name "") means none.
type nextSnap struct {
	name string
	at   time.Time
}

type scheduler struct {
	a             *Agent
	clock         schedule.Clock
	projectPath   string
	approvalsPath string

	mu        sync.Mutex
	doc       schedule.Doc
	timers    []schedule.Schedule
	approvals schedule.Approvals
	warned    map[string]bool
	notes     []string // notices raised under mu, delivered by unlock
	// pending is every event this scheduler queued and begin has not yet
	// taken: begin runs only an event found here, so a queued event never
	// outlives a pause, an edit or a removal of its schedule.
	pending map[string]pendingEvent
	// next is the status line's view of the soonest due schedule,
	// republished (unlock, below) after every locked section that might
	// have changed the answer. Reading it (NextSchedule) never takes mu: a
	// slow schedules.md/session write must not stall every terminal's
	// render (schedules spec, controller ruling on Task 8's review).
	next atomic.Pointer[nextSnap]
	// floor is the latest LastRun this process set per schedule. A run
	// whose LastRun could not be saved would otherwise be due again on the
	// next reread of the file, and fire back to back.
	floor map[string]time.Time

	// held marks timers loaded by a session switch after the startup prompt
	// began, by the generation of the prompt that will confirm them; the
	// loop never queues a held timer (loadTimers).
	held      map[string]int
	holdGen   int
	gateBegun bool // the startup prompt has begun (StartSchedules)

	gateOnce  sync.Once
	startOnce sync.Once
	stopOnce  sync.Once
	kick      chan struct{}
	stop      chan struct{}
	done      chan struct{}
	started   bool
}

// EnableSchedules creates the scheduler. cmd calls it for interactive
// sessions when schedules.enabled; it does not start it.
func (a *Agent) EnableSchedules(clock schedule.Clock) {
	base, err := config.Dir()
	if err != nil {
		a.notice("schedules disabled: %v", err)
		return
	}
	s := &scheduler{a: a, clock: clock,
		projectPath:   filepath.Join(a.Tools.Root, ".be-code", "schedules.md"),
		approvalsPath: filepath.Join(base, "engine", engine.Key(a.Tools.Root), "schedules.json"),
		warned:        map[string]bool{},
		pending:       map[string]pendingEvent{},
		held:          map[string]int{},
		floor:         map[string]time.Time{},
		kick:          make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	s.approvals = schedule.LoadApprovals(s.approvalsPath)
	a.sessionMu.Lock()
	if a.Session != nil {
		s.timers = append([]schedule.Schedule(nil), a.Session.Timers...)
	}
	a.sessionMu.Unlock()
	a.sched = s
}

// noteLocked holds a notice until the lock is released (see scheduler).
func (s *scheduler) noteLocked(format string, args ...any) {
	s.notes = append(s.notes, fmt.Sprintf(format, args...))
}

// unlock releases mu and then delivers the notices raised under it. Every
// caller that took mu to use a *Locked method unlocks through here — which
// is also why it is the one place that republishes the next-due snapshot:
// every locked section that could change the answer ends by calling this.
func (s *scheduler) unlock() {
	s.publishNextLocked()
	notes := s.notes
	s.notes = nil
	s.mu.Unlock()
	for _, n := range notes {
		s.a.notice("%s", n)
	}
}

// publishNextLocked recomputes the soonest active schedule with no event
// already waiting and stores it lock-free in s.next. Call only while mu is
// held; it does not reload schedules.md itself (the status line is redrawn
// often — the same reasoning NextSchedule used to carry directly).
func (s *scheduler) publishNextLocked() {
	all := s.allLocked()
	waiting := map[string]bool{}
	for _, sc := range all {
		if s.waitingLocked(sc.ID) {
			waiting[sc.ID] = true
		}
	}
	var snap nextSnap
	for _, sc := range all {
		if n, ok := sc.NextDue(); ok && !waiting[sc.ID] && (snap.name == "" || n.Before(snap.at)) {
			snap = nextSnap{name: sc.Name, at: n}
		}
	}
	s.next.Store(&snap)
}

// loadTimers replaces the one-off timers with a newly installed session's
// (SetSession: a resume, or /clear with none). They are written back to the
// session too, so a save of the old list that raced the switch cannot stick.
//
// Once the startup prompt has begun, timers arriving this way (plain
// mode's /resume, a picker switch) were never shown to anyone in this
// process: their active ones are held — never queued — under a new
// generation until a person confirms them (spec §3.5, "starts or
// resumes"). loadTimers never asks: the caller that switched sessions calls
// ConfirmHeldTimers on a goroutine that may read the terminal — in plain
// mode the REPL's own, since a second reader of its input would take the
// person's next line as the answer.
func (s *scheduler) loadTimers(ts []schedule.Schedule) {
	s.mu.Lock()
	s.timers = append([]schedule.Schedule(nil), ts...)
	s.saveTimersLocked()
	s.held = map[string]int{}
	if s.gateBegun {
		s.holdGen++
		for _, sc := range s.timers {
			if sc.State == schedule.Active {
				s.held[sc.ID] = s.holdGen
			}
		}
	}
	s.unlock()
	s.kickLoop()
}

// reloadLocked rereads schedules.md: the file wins over what we hold.
func (s *scheduler) reloadLocked() {
	d, err := schedule.LoadFile(s.projectPath)
	if errors.Is(err, schedule.ErrBroken) {
		aside, rerr := schedule.RenameBroken(s.projectPath, s.clock.Now())
		if rerr == nil {
			s.noteLocked("schedules.md could not be read; moved aside to %s", aside)
		}
		d = schedule.Doc{}
	} else if err != nil {
		s.noteLocked("schedules.md: %v", err)
		return
	}
	for _, sec := range d.Sections {
		if sec.Err != nil && !s.warned[sec.Raw] {
			s.warned[sec.Raw] = true
			s.noteLocked("schedules.md: skipped %s: %v", firstLine(sec.Raw, 60), sec.Err)
		}
	}
	s.doc = d
}

// allLocked is every schedule, project first, with the LastRun floor
// applied.
func (s *scheduler) allLocked() []schedule.Schedule {
	all := append(s.doc.Schedules(), s.timers...)
	for i := range all {
		all[i] = s.floorLocked(all[i])
	}
	return all
}

// floorLocked is sc with LastRun no earlier than this process last set it.
func (s *scheduler) floorLocked(sc schedule.Schedule) schedule.Schedule {
	if f, ok := s.floor[sc.ID]; ok && f.After(sc.LastRun) {
		sc.LastRun = f
	}
	return sc
}

// findLocked finds by ID, else by name, with the LastRun floor applied. A
// name prefers a schedule that is not done: a finished one-off keeps its
// name in the session's timers, and must never shadow a live schedule
// added under the same name since. A done one is returned only when no
// live one has the name (/schedule show of a finished timer).
func (s *scheduler) findLocked(key string) (schedule.Schedule, bool, bool) {
	type hit struct {
		sc      schedule.Schedule
		project bool
	}
	var all []hit
	for _, sc := range s.doc.Schedules() {
		all = append(all, hit{sc, true})
	}
	for _, sc := range s.timers {
		all = append(all, hit{sc, false})
	}
	for _, h := range all {
		if h.sc.ID == key {
			return s.floorLocked(h.sc), true, h.project
		}
	}
	var done *hit
	for i, h := range all {
		if h.sc.Name != key {
			continue
		}
		if h.sc.State != schedule.Done {
			return s.floorLocked(h.sc), true, h.project
		}
		if done == nil {
			done = &all[i]
		}
	}
	if done != nil {
		return s.floorLocked(done.sc), true, done.project
	}
	return schedule.Schedule{}, false, false
}

// lookup is findLocked after a reload, for tests and callers outside mu.
func (s *scheduler) lookup(key string) (schedule.Schedule, bool, bool) {
	s.mu.Lock()
	defer s.unlock()
	s.reloadLocked()
	return s.findLocked(key)
}

// putLocked stores sc in its store and persists that store.
func (s *scheduler) putLocked(sc schedule.Schedule, project bool) error {
	if project {
		s.doc.Put(sc)
		if err := schedule.SaveFile(s.projectPath, s.doc); err != nil {
			s.noteLocked("could not save schedules.md: %v", err)
			return err
		}
		return nil
	}
	replaced := false
	for i := range s.timers {
		if s.timers[i].ID == sc.ID {
			s.timers[i], replaced = sc, true
		}
	}
	if !replaced {
		s.timers = append(s.timers, sc)
	}
	s.saveTimersLocked()
	return nil
}

func (s *scheduler) removeLocked(id string) bool {
	if s.doc.Remove(id) {
		if err := schedule.SaveFile(s.projectPath, s.doc); err != nil {
			s.noteLocked("could not save schedules.md: %v", err)
		}
		return true
	}
	for i := range s.timers {
		if s.timers[i].ID == id {
			s.timers = append(s.timers[:i], s.timers[i+1:]...)
			s.saveTimersLocked()
			return true
		}
	}
	return false
}

// saveTimersLocked mirrors the timers into the session (s.mu, then
// sessionMu and saveMu inside UpdateSession — never the other way round).
func (s *scheduler) saveTimersLocked() {
	ts := append([]schedule.Schedule(nil), s.timers...)
	s.a.UpdateSession(func(ss *store.Session) { ss.Timers = ts })
}

func (s *scheduler) approveLocked(sc schedule.Schedule) {
	s.approvals[sc.ID] = sc.Hash()
	if err := s.approvals.Save(s.approvalsPath); err != nil {
		s.noteLocked("could not save schedule approvals: %v", err)
	}
}

func (s *scheduler) approvedLocked(sc schedule.Schedule) bool {
	return s.approvals[sc.ID] == sc.Hash()
}

func (s *scheduler) kickLoop() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *scheduler) startLoop() {
	s.startOnce.Do(func() {
		select {
		case <-s.stop:
			return // stopped before it ever started: no pass
		default:
		}
		s.mu.Lock()
		s.started = true
		s.mu.Unlock()
		go s.loop()
	})
}

// StopSchedules stops the loop; a no-op when never enabled or started.
func (a *Agent) StopSchedules() {
	s := a.sched
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stop) })
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if started {
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
		}
	}
}

func (s *scheduler) loop() {
	defer close(s.done)
	defer func() {
		if r := recover(); r != nil {
			s.a.notice("schedules stopped: %v", r)
		}
	}()
	for {
		next, ok := s.queueDue()
		var timer schedule.Timer
		var c <-chan time.Time
		if ok {
			timer = s.clock.NewTimer(next.Sub(s.clock.Now()))
			c = timer.C()
		}
		select {
		case <-s.stop:
			if timer != nil {
				timer.Stop()
			}
			return
		case <-s.kick:
		case <-c:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

// queueDue queues every due, approved schedule not already waiting, pauses
// any whose content no longer matches its approval, and returns the nearest
// future due time. Re-arming from Now() on every wake is what makes a clock
// jump (suspend, NTP) run anything it skipped over at most once.
//
// A recurring schedule whose event is still waiting in the queue (or is
// queued by this very pass) arms the timer for its next time after now: if a person drops the queued event,
// that wake finds it no longer waiting and fires it again, rather than the
// loop sleeping on nothing until some other kick.
func (s *scheduler) queueDue() (time.Time, bool) {
	now := s.clock.Now()
	var fire []schedule.Schedule
	var next time.Time
	have := false
	consider := func(t time.Time) {
		if !have || t.Before(next) {
			next, have = t, true
		}
	}
	s.mu.Lock()
	s.reloadLocked()
	s.settleDroppedLocked()
	all := s.allLocked()
	sort.SliceStable(all, func(i, j int) bool {
		di, _ := all[i].NextDue()
		dj, _ := all[j].NextDue()
		return di.Before(dj)
	})
	for _, sc := range all {
		due, ok := sc.NextDue()
		if !ok {
			continue
		}
		if s.held[sc.ID] != 0 {
			continue // awaiting its own confirmation (loadTimers)
		}
		if _, waiting := s.pending[sc.ID]; waiting {
			considerNextAfter(sc, now, consider)
			continue
		}
		if due.After(now) {
			consider(due)
			continue
		}
		if !s.approvedLocked(sc) {
			_, _, project := s.findLocked(sc.ID)
			sc.State = schedule.Paused
			s.putLocked(sc, project)
			s.noteLocked("schedule %q changed since approved and was paused; /schedule resume %s to approve it", sc.Name, sc.Name)
			continue
		}
		fire = append(fire, sc)
		considerNextAfter(sc, now, consider) // it is waiting from here on
	}
	s.unlock()
	for _, sc := range fire {
		s.a.fireNow(sc, false)
	}
	return next, have
}

// settleDroppedLocked closes the events a person dropped from the queue: a
// pending event that is neither queued nor armed. That occurrence is
// skipped — a recurring schedule counts it as run at the time it was
// queued (so its next time still fires), a one-off is done. Reading the
// queue and then the arm is safe because DrainForTurn arms before it
// releases the queue, and a taken arm stays until begin has run.
func (s *scheduler) settleDroppedLocked() {
	if len(s.pending) == 0 {
		return
	}
	queued := s.a.scheduledQueued()
	armed := s.a.armedID()
	for id, pe := range s.pending {
		if queued[id] || armed == id {
			continue
		}
		delete(s.pending, id)
		sc, ok, project := s.findLocked(id)
		if !ok {
			continue
		}
		if sp, err := sc.Spec(); err == nil && sp.Recurring() {
			if pe.queuedAt.After(sc.LastRun) {
				sc.LastRun = pe.queuedAt
				s.floor[id] = pe.queuedAt
			}
		} else {
			sc.State = schedule.Done
			sc.LastOutcome = "dropped from the queue"
		}
		s.putLocked(sc, project)
	}
}

// considerNextAfter offers a recurring schedule's first time after now: the
// wake at which a queued event that has since been dropped fires again.
func considerNextAfter(sc schedule.Schedule, now time.Time, consider func(time.Time)) {
	if sp, err := sc.Spec(); err == nil && sp.Recurring() {
		if t, ok := sp.Next(now); ok {
			consider(t)
		}
	}
}

// fireNow queues sc's event and wakes an idle UI. manual is a person's own
// /schedule run, which begin lets run a paused schedule. The event is
// recorded as pending and queued under one hold of s.mu, so a begin can
// never see it queued but not pending. An event already pending is not
// queued again (a /schedule run racing the loop's own pass); false then.
func (a *Agent) fireNow(sc schedule.Schedule, manual bool) bool {
	s := a.sched
	if s == nil {
		return false
	}
	s.mu.Lock()
	if _, dup := s.pending[sc.ID]; dup {
		s.unlock()
		return false
	}
	s.pending[sc.ID] = pendingEvent{queuedAt: s.clock.Now(), manual: manual}
	a.EnqueueScheduled(sc.ID, sc.Name, fireText(sc))
	s.unlock()
	if a.Events.OnScheduleFire != nil {
		a.Events.OnScheduleFire(sc.Name)
	}
	return true
}

// fireText is the request a fired event becomes (schedules spec §2.2).
func fireText(sc schedule.Schedule) string {
	by := "you"
	if sc.CreatedBy == "agent" {
		by = "the model"
	}
	return fmt.Sprintf("[Scheduled event %q — %s, set by %s %s]\n%s",
		sc.Name, sc.When, by, sc.Created.Format("2006-01-02"), sc.Instruction)
}

// begin decides, at run time, whether a drained event may run, and marks a
// run's start. It refuses (with the reason) an event it did not queue or no
// longer holds as pending, one whose schedule is gone, one whose content no
// longer matches its approval (which it pauses — a grant written into the
// file after the event was queued must never run unattended), and one
// whose schedule is no longer active unless a person asked for it by hand.
// Checking only at queue time left all of that to whatever the file said
// by the time the turn began. On success: pending cleared, LastRun now,
// persisted, and floored in memory in case the save failed.
func (s *scheduler) begin(id string) (sc schedule.Schedule, project bool, reason string) {
	s.mu.Lock()
	defer s.unlock()
	s.reloadLocked()
	pe, isPending := s.pending[id]
	delete(s.pending, id)
	sc, ok, project := s.findLocked(id)
	switch {
	case !ok:
		return sc, false, "it no longer exists"
	case !isPending:
		return sc, project, "it is no longer queued"
	case s.held[id] != 0 && !pe.manual:
		return sc, project, "it is waiting for a person to confirm it"
	case !s.approvedLocked(sc):
		sc.State = schedule.Paused
		s.putLocked(sc, project)
		s.noteLocked("schedule %q changed since approved and was paused; /schedule resume %s to approve it", sc.Name, sc.Name)
		return sc, project, "it changed since it was approved"
	case sc.State != schedule.Active && !pe.manual:
		return sc, project, "it is " + string(sc.State)
	}
	sc.LastRun = s.clock.Now()
	s.floor[id] = sc.LastRun
	s.putLocked(sc, project)
	return sc, project, ""
}

// finish records a run's outcome (spec §2.6).
func (s *scheduler) finish(id, outcome string) {
	s.mu.Lock()
	s.reloadLocked()
	sc, ok, project := s.findLocked(id)
	if !ok {
		s.unlock()
		return
	}
	sc.LastOutcome = outcome
	paused := false
	if outcome == "ok" {
		sc.Failures = 0
	} else {
		sc.Failures++
	}
	sp, err := sc.Spec()
	switch {
	case err != nil:
		// Unparseable (a hand edit): record the outcome, decide nothing.
	case !sp.Recurring():
		sc.State = schedule.Done
	case s.a.Cfg.Schedules.PauseAfterFailures > 0 && sc.Failures >= s.a.Cfg.Schedules.PauseAfterFailures:
		sc.State, paused = schedule.Paused, true
	}
	s.putLocked(sc, project)
	s.unlock()
	s.a.notice("⏰ %s: %s", sc.Name, outcome)
	if paused {
		s.a.notice("schedule %q paused after %d failed runs in a row; /schedule resume %s when it is fixed", sc.Name, sc.Failures, sc.Name)
	}
	s.kickLoop()
}

// runFired runs a scheduled event's turn under its allowance (spec §2.2–2.6).
func (a *Agent) runFired(ctx context.Context, f *firing, input string) (answer string, rep *ReviewedReport, err error) {
	sc, project, reason := a.sched.begin(f.id)
	a.releaseFiring(f)
	if reason != "" {
		name := sc.Name
		if name == "" {
			name = f.name
		}
		a.notice("scheduled event %q was not run: %s", name, reason)
		return "", nil, nil
	}
	if !project {
		a.saveBeforeFiredRun()
	}
	_, askT, maxRT := a.Cfg.Schedules.Durations()
	if sc.AskTimeout > 0 {
		askT = sc.AskTimeout
	}
	if sc.MaxRuntime > 0 {
		maxRT = sc.MaxRuntime
	}
	if sc.Task != "" && a.engine() != nil {
		// Never through SetStatus on a closed node: that would reopen it.
		var st engine.Status
		var found bool
		var terr error
		a.engineDo("schedule task", func(es *engine.Store) {
			st, found = es.NodeStatus(sc.Task)
			if found && st != engine.StatusDone && st != engine.StatusDropped {
				terr = es.SetStatus(sc.Task, engine.StatusDoing, "")
			}
		})
		switch {
		case !found:
			a.notice("schedule %q: task %s is missing; running without it", sc.Name, sc.Task)
		case st == engine.StatusDone || st == engine.StatusDropped:
			a.notice("schedule %q: task %s is %s; running without it", sc.Name, sc.Task, st)
		case terr != nil:
			a.notice("schedule %q: task %s: %v; running without it", sc.Name, sc.Task, terr)
		}
	}
	a.Tools.SetAllowance(sc.Allow, askT)
	rctx, cancel := context.WithTimeout(ctx, maxRT)
	defer func() {
		refused, askTimedOut := a.Tools.ClearAllowance()
		timedOut := errors.Is(rctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		cancel()
		if r := recover(); r != nil {
			a.sched.finish(sc.ID, "error: "+firstLine(fmt.Sprint(r), 120))
			panic(r)
		}
		if timedOut {
			err = fmt.Errorf("scheduled event %q stopped after its max runtime of %s", sc.Name, maxRT)
		}
		if askTimedOut {
			a.notice("scheduled event %q: nobody answered %s within %s; refused", sc.Name, refused, askT)
		}
		a.sched.finish(sc.ID, firedOutcome(ctx, timedOut, err, rep, refused, askTimedOut))
	}()
	return a.runFull(rctx, input)
}

// saveBeforeFiredRun writes the session file once a one-off timer's LastRun
// is set, so a crash mid-run cannot leave it due again on resume. The same
// save tidyBeforeColdRead makes: under the turn lock (runFired has not taken
// it yet; run takes it later), never for an empty conversation (a session
// has no file until its first request, and the live registry depends on
// that), and through autosave's save guard.
func (a *Agent) saveBeforeFiredRun() {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	if a.History != nil && len(a.History.Messages) > 0 {
		a.autosave(a.lastUserInput)
	}
}

func firedOutcome(parent context.Context, timedOut bool, err error, rep *ReviewedReport, refused string, askTimedOut bool) string {
	switch {
	case parent.Err() != nil:
		return "cancelled"
	case timedOut:
		return "timed out"
	case err != nil:
		return "error: " + firstLine(err.Error(), 120)
	case refused != "" && askTimedOut:
		return "refused: " + refused + " (nobody answered)"
	case refused != "":
		return "refused: " + refused
	case rep != nil && rep.Verify != nil && !rep.Verify.Passed():
		return "checks failed"
	}
	return "ok"
}
