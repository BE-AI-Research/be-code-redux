package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
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
// Every hold of s.mu is released on a panic too (a deferred unlock, most
// through locked): a scheduler left locked would freeze every terminal.
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
	// unconfirmed marks schedules the startup prompt showed but nobody
	// answered (withdrawn: the session quit, or the prompt was taken
	// down). Nothing about them is changed or saved; they just never run
	// unasked in this process — the next start asks again, and a person's
	// /schedule resume confirms one now.
	unconfirmed map[string]bool

	// ctx lives as long as the scheduler: StopSchedules cancels it, which
	// withdraws a schedule prompt still open when the session ends.
	ctx    context.Context
	cancel context.CancelFunc

	// standing is schedules.allow, parsed once when the scheduler is
	// created (an unusable entry dropped; cmd warns about it): merged into
	// every fired turn's allowance, and what auto_approve_create measures a
	// model-created schedule's grants against. Never written after
	// EnableSchedules, so it is read without mu.
	standing schedule.Allowance

	// passStarts and passDone count the loop's passes: one starts when
	// queueDue begins, and is done once its timer is armed. Tests wait on
	// them (a pass that began after the thing they did has completed)
	// instead of sleeping and asserting that nothing happened.
	passStarts, passDone atomic.Int64

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
		unconfirmed:   map[string]bool{},
		floor:         map[string]time.Time{},
		kick:          make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.standing, _ = schedule.ParseStanding(a.Cfg.Schedules.Allow)
	s.approvals = schedule.LoadApprovals(s.approvalsPath)
	a.sessionMu.Lock()
	if a.Session != nil {
		s.timers = append([]schedule.Schedule(nil), a.Session.Timers...)
	}
	a.sessionMu.Unlock()
	a.sched = s
}

// locked runs fn holding mu and releases it through unlock even when fn
// panics.
func (s *scheduler) locked(fn func()) {
	s.mu.Lock()
	defer s.unlock()
	fn()
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
	s.locked(func() {
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
	})
	s.kickLoop()
}

// reloadLocked rereads schedules.md: the file wins over what we hold. It
// rereads the approvals too, which every session on this workspace shares:
// one another session revoked (a pause there) is revoked here as well.
func (s *scheduler) reloadLocked() {
	s.approvals = schedule.LoadApprovals(s.approvalsPath)
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
	h := sc.Hash()
	s.saveApprovalsLocked(func(a schedule.Approvals) { a[sc.ID] = h })
}

// revokeLocked forgets id's approval. Every pause does this (final review
// I3): a paused schedule comes back only through /schedule resume, which
// asks — never through `state: active` written into schedules.md.
func (s *scheduler) revokeLocked(id string) {
	s.saveApprovalsLocked(func(a schedule.Approvals) { delete(a, id) })
}

// saveApprovalsLocked applies change to the approvals as they are on disk
// now, not as this process last read them: another session on the same
// workspace shares the file, and a save of a stale copy would drop what it
// approved (or revoked) since.
func (s *scheduler) saveApprovalsLocked(change func(schedule.Approvals)) {
	a := schedule.LoadApprovals(s.approvalsPath)
	change(a)
	s.approvals = a
	if err := a.Save(s.approvalsPath); err != nil {
		s.noteLocked("could not save schedule approvals: %v", err)
	}
}

// pauseLocked pauses sc in its store and revokes its approval.
func (s *scheduler) pauseLocked(sc schedule.Schedule, project bool) {
	sc.State = schedule.Paused
	s.putLocked(sc, project)
	s.revokeLocked(sc.ID)
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
		s.locked(func() { s.started = true })
		go s.loop()
	})
}

// StopSchedules stops the loop; a no-op when never enabled or started.
func (a *Agent) StopSchedules() {
	s := a.sched
	if s == nil {
		return
	}
	s.stopOnce.Do(func() { close(s.stop); s.cancel() })
	var started bool
	s.locked(func() { started = s.started })
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
		// Never sleep longer than maxSleep: a timer measures time the
		// machine was awake, so after a suspend an uncapped one fires hours
		// late. Each wake re-arms from Now() (final review I5), and rereads
		// schedules.md, so a hand-added schedule is noticed too.
		d := maxSleep
		if ok {
			if until := next.Sub(s.clock.Now()); until < d {
				d = until
			}
		}
		timer := s.clock.NewTimer(d)
		c := timer.C()
		s.passDone.Add(1)
		select {
		case <-s.stop:
			timer.Stop()
			return
		case <-s.kick:
		case <-c:
		}
		timer.Stop()
	}
}

// maxSleep is the longest the loop waits between passes.
const maxSleep = time.Minute

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
	s.passStarts.Add(1)
	now := s.clock.Now()
	var fire []schedule.Schedule
	var next time.Time
	have := false
	consider := func(t time.Time) {
		if !have || t.Before(next) {
			next, have = t, true
		}
	}
	s.locked(func() {
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
			if s.held[sc.ID] != 0 || s.unconfirmed[sc.ID] {
				continue // awaiting a person's confirmation (loadTimers, gateSchedules)
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
				s.pauseLocked(sc, project)
				s.noteLocked("schedule %q changed since approved and was paused; /schedule resume %s to approve it", sc.Name, sc.Name)
				continue
			}
			fire = append(fire, sc)
			considerNextAfter(sc, now, consider) // it is waiting from here on
		}
	})
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
	dup := false
	s.locked(func() {
		if _, dup = s.pending[sc.ID]; dup {
			return
		}
		s.pending[sc.ID] = pendingEvent{queuedAt: s.clock.Now(), manual: manual}
		a.EnqueueScheduled(sc.ID, sc.Name, fireText(sc))
	})
	if dup {
		return false
	}
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
	case (s.held[id] != 0 || s.unconfirmed[id]) && !pe.manual:
		return sc, project, "it is waiting for a person to confirm it"
	case !s.approvedLocked(sc):
		s.pauseLocked(sc, project)
		s.noteLocked("schedule %q changed since approved and was paused; /schedule resume %s to approve it", sc.Name, sc.Name)
		return sc, project, "it changed since it was approved"
	case sc.State != schedule.Active && !pe.manual:
		return sc, project, "it is " + string(sc.State)
	}
	if !pe.manual {
		// Another session on this workspace may have run this very
		// occurrence (final review I4): its LastRun, reread just now,
		// already puts the next time after this event was queued — or it
		// holds the claim and has not saved LastRun yet.
		due, ok := sc.NextDue()
		if !ok || due.After(pe.queuedAt) {
			return sc, project, "it already ran in another session"
		}
		if taken, err := s.claimLocked(id, due); err != nil {
			return sc, project, fmt.Sprintf("it could not be claimed (%v)", err)
		} else if taken {
			// Counted as run here too, so the loop does not queue the
			// same occurrence again on its next wake.
			s.floor[id] = due
			return sc, project, "it already ran in another session"
		}
	}
	sc.LastRun = s.clock.Now()
	s.floor[id] = sc.LastRun
	s.putLocked(sc, project)
	return sc, project, ""
}

// finish records a run's outcome (spec §2.6).
func (s *scheduler) finish(id, outcome string) {
	var sc schedule.Schedule
	var ok, paused bool
	s.locked(func() {
		s.reloadLocked()
		var project bool
		sc, ok, project = s.findLocked(id)
		if !ok {
			return
		}
		sc.LastOutcome = outcome
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
			paused = true
		}
		if paused {
			s.pauseLocked(sc, project)
		} else {
			s.putLocked(sc, project)
		}
	})
	if !ok {
		return
	}
	s.a.notice("⏰ %s: %s", sc.Name, outcome)
	if paused {
		s.a.notice("schedule %q paused after %d failed runs in a row; /schedule resume %s when it is fixed", sc.Name, sc.Failures, sc.Name)
	}
	s.kickLoop()
}

// claimLocked claims one occurrence of id for this session with a file
// created O_EXCL under the engine dir, which every session on this
// workspace shares. taken is true when another session got there first.
// Claims older than a day are pruned on the way: by then every occurrence
// they guarded has long been recorded as LastRun.
func (s *scheduler) claimLocked(id string, due time.Time) (taken bool, err error) {
	dir := filepath.Join(filepath.Dir(s.approvalsPath), "claims")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	if ents, err := os.ReadDir(dir); err == nil {
		cutoff := time.Now().Add(-24 * time.Hour)
		for _, e := range ents {
			if info, err := e.Info(); err == nil && info.ModTime().Before(cutoff) {
				os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	f, err := os.OpenFile(filepath.Join(dir, claimName(id, due)), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	fmt.Fprintf(f, "%d\n", os.Getpid())
	return false, f.Close()
}

// claimName is the claim file for one occurrence: the schedule's id and
// its due time.
func claimName(id string, due time.Time) string {
	return fmt.Sprintf("%s-%d", id, due.Unix())
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
	a.prepareFiredRun(sc, !project, askT)
	rctx, cancel := context.WithTimeout(ctx, maxRT)
	defer func() {
		refused, askTimedOut := a.Tools.ClearAllowance()
		timedOut := errors.Is(rctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		cancel()
		// Sub-agent work held during the turn dispatches now, under a
		// watched session — on every exit path, a panic's included. Fenced
		// so a failure here can never mask the turn's own panic.
		a.scheduleAfterFired()
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

// scheduleAfterFired runs the one ScheduleSubAgents a fired turn owes on
// its way out (Tools.Fired is already false), never panicking.
func (a *Agent) scheduleAfterFired() {
	if a.subs == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			a.notice("sub-agent schedule after a scheduled event failed (%v); held work starts at the next schedule", r)
		}
	}()
	a.ScheduleSubAgents()
}

// prepareFiredRun installs the event's allowance, and for a one-off timer
// first writes the session file once its LastRun is set, so a crash
// mid-run cannot leave it due again on resume. Both happen under the turn
// lock (runFired has not taken it yet; run takes it per round later), so a
// request still finishing on the agent never runs a step under the event's
// allowance. The save is the one tidyBeforeColdRead makes: never for an
// empty conversation (a session has no file until its first request, and
// the live registry depends on that), and through autosave's save guard.
func (a *Agent) prepareFiredRun(sc schedule.Schedule, saveSession bool, askT time.Duration) {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	if saveSession && a.History != nil && len(a.History.Messages) > 0 {
		a.autosave(a.lastUserInput)
	}
	// schedules.allow rides with the schedule's own grants; the merged
	// allowance is checked by the same rules (protectedFromGrants among
	// them) and cleared with it.
	allow := append(append(schedule.Allowance(nil), sc.Allow...), a.sched.standing...)
	a.Tools.SetFiredPolicy(allow, askT, a.Cfg.Schedules.InheritSessionApprovals)
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
