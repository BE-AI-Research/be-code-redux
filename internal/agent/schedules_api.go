package agent

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/schedule"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// The management side of scheduled events (schedules spec §3): adding,
// pausing, resuming, cancelling and running them by hand, the tables the
// UIs print, and the startup prompt. Every question goes to a person
// through askSchedule, and never while s.mu is held: a UI's approval can
// wait as long as a person takes, and the loop needs s.mu meanwhile. So
// each change is decided in three steps — read under the lock, ask with it
// released, then reread and apply under it, refusing when what the person
// was shown is no longer what is stored.

var errSchedulesOff = errors.New("scheduled events are disabled (schedules.enabled is false, or this is a one-shot run)")

// errDuringFired refuses what the model may not do while a scheduled
// event's turn runs (final review I6): each would raise a prompt with no
// deadline, and nobody is necessarily there to answer it.
var errDuringFired = errors.New("schedules cannot be created or resumed during a scheduled event; say what you wanted and a person can do it with /schedule")

// SchedulesEnabled reports whether this session has a scheduler.
func (a *Agent) SchedulesEnabled() bool { return a.sched != nil }

// askSchedule raises the one "schedule" prompt (no "always"; spec §3.1). No
// approver is a refusal. withdrawn reports a prompt that ended with nobody
// answering it — the session quit, the scheduler stopped, the prompt was
// taken down — which is not a "no" (final review I2): the UIs say so
// through tools.MarkWithdrawn, and the scheduler's own context ends when
// it stops.
func (a *Agent) askSchedule(detail string) (yes, withdrawn bool) {
	ctx, out := tools.WithAskOutcome(a.sched.ctx)
	switch {
	case a.Tools.ApproveCtx != nil:
		yes = a.Tools.ApproveCtx(ctx, "schedule", detail)
	case a.Tools.Approve != nil:
		yes = a.Tools.Approve("schedule", detail)
	}
	return yes, !yes && (out.Withdrawn() || ctx.Err() != nil)
}

// askedYes is askSchedule for a change that is simply not made unless a
// person said yes.
func (a *Agent) askedYes(detail string) bool {
	yes, _ := a.askSchedule(detail)
	return yes
}

func byLabel(by string) string {
	if by == "agent" {
		return "the model"
	}
	return "you"
}

// describe is the whole schedule for one prompt: nothing is asked later.
// The due times listed are the next three after now (and after its last
// run), never ones computed from its creation, which for an old schedule
// lie weeks in the past.
func describe(sc schedule.Schedule, sp schedule.Spec, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n  when: %s\n", sc.Name, sc.When)
	if due, ok := sp.Due(); ok {
		if due.After(now) {
			fmt.Fprintf(&b, "  next: %s\n", due.Format("Mon 2006-01-02 15:04"))
		} else {
			fmt.Fprintf(&b, "  next: %s (already passed)\n", due.Format("Mon 2006-01-02 15:04"))
		}
	} else {
		t := now
		if sc.LastRun.After(t) {
			t = sc.LastRun
		}
		var nexts []string
		for i := 0; i < 3; i++ {
			n, ok := sp.Next(t)
			if !ok {
				break
			}
			nexts = append(nexts, n.Format("Mon 2006-01-02 15:04"))
			t = n
		}
		if len(nexts) > 0 {
			fmt.Fprintf(&b, "  next: %s\n", strings.Join(nexts, " · "))
		}
	}
	fmt.Fprintf(&b, "  instruction: %s\n", strings.ReplaceAll(sc.Instruction, "\n", "\n    "))
	if sc.Task != "" {
		fmt.Fprintf(&b, "  linked to task %s\n", sc.Task)
	}
	if len(sc.Allow) == 0 {
		b.WriteString("  may do without asking: nothing (it can read; anything else asks)\n")
	} else {
		b.WriteString("  may do without asking:\n")
		for _, g := range sc.Allow {
			line := g.String()
			if g.Kind == "write" && g.Value == "." {
				line += "   ← anywhere in the workspace"
			}
			if g.Kind == "tool" && strings.ContainsAny(g.Value, "*?[") {
				line += "   ← every tool whose name matches"
			}
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// describeMarked is describe with mark on the schedule's name line.
func describeMarked(sc schedule.Schedule, sp schedule.Spec, now time.Time, mark string) string {
	d := describe(sc, sp, now)
	if mark == "" {
		return d
	}
	return strings.Replace(d, "\n", mark+"\n", 1)
}

func validBy(by string) error {
	if by != "person" && by != "agent" {
		return fmt.Errorf("by must be \"person\" or \"agent\" (got %q)", by)
	}
	return nil
}

// tooFrequent is add's min_interval refusal, shared with resume and the
// startup prompt (a hand edit can shorten an approved interval).
func (a *Agent) tooFrequent(sp schedule.Spec, now time.Time) error {
	minGap, _, _ := a.Cfg.Schedules.Durations()
	if sp.Recurring() && sp.ShortestGap(now) < minGap {
		return fmt.Errorf("%q runs more often than every %s (schedules.min_interval)", sp.String(), minGap)
	}
	return nil
}

// activeLocked counts active schedules other than except.
func (s *scheduler) activeLocked(except string) int {
	n := 0
	for _, o := range s.allLocked() {
		if o.State == schedule.Active && o.ID != except {
			n++
		}
	}
	return n
}

func maxActiveErr(active int) error {
	return fmt.Errorf("%d schedules are already active (schedules.max_active); cancel one first", active)
}

// admitLocked refuses a name already in use and an addition past
// schedules.max_active. Checked before the prompt, so a refusal never asks,
// and again after it, since the file may have changed meanwhile.
func (s *scheduler) admitLocked(name string) error {
	for _, o := range s.allLocked() {
		if o.Name == name && o.State != schedule.Done {
			return fmt.Errorf("a schedule named %q already exists; cancel it first", name)
		}
	}
	if active := s.activeLocked(""); s.a.Cfg.Schedules.MaxActive > 0 && active >= s.a.Cfg.Schedules.MaxActive {
		return maxActiveErr(active)
	}
	return nil
}

// withdrawLocked takes id's waiting event out of the queue and forgets it,
// so a paused or cancelled schedule leaves no dead event behind in the
// queue popup. An event already drained for its turn (armed) is refused by
// begin, which no longer finds it pending. s.mu, then inbox.mu.
func (s *scheduler) withdrawLocked(id string) {
	delete(s.pending, id)
	s.a.RemoveWhere(func(it InboxItem) bool { return it.Scheduled == id })
}

// waitingLocked reports whether id has an event queued, armed or pending.
func (s *scheduler) waitingLocked(id string) bool {
	if _, ok := s.pending[id]; ok {
		return true
	}
	return s.a.scheduledQueued()[id] || s.a.armedID() == id
}

// AddSchedule validates, asks once, and stores (spec §3.1–3.4). by is
// "person" or "agent".
func (a *Agent) AddSchedule(req schedule.Request, by string) (string, error) {
	s := a.sched
	if s == nil {
		return "", errSchedulesOff
	}
	if err := validBy(by); err != nil {
		return "", err
	}
	if by == "agent" && a.Tools.Fired() {
		return "", errDuringFired
	}
	if by == "agent" && a.Tools.UntrustedWeb() {
		return "", errors.New("a web page was read during this request, so the model cannot create schedules until a person's next request; a person can add it with /schedule add")
	}
	now := s.clock.Now()
	name := strings.TrimSpace(req.Name)
	if !schedule.ValidName(name) {
		return "", fmt.Errorf("name %q must be lowercase letters, digits and dashes (at most 40)", name)
	}
	if strings.TrimSpace(req.Instruction) == "" {
		return "", errors.New("an instruction is required: what should the model do when it fires?")
	}
	sp, err := schedule.Parse(req.When, now)
	if err != nil {
		return "", err
	}
	if err := a.tooFrequent(sp, now); err != nil {
		return "", err
	}
	if due, ok := sp.Due(); ok && !due.After(now) {
		return "", fmt.Errorf("that time has already passed (%s)", due.Format("2006-01-02 15:04"))
	}
	allow, err := schedule.ParseAllowance(req.Allow)
	if err != nil {
		return "", err
	}
	if task := strings.TrimSpace(req.Task); !schedule.ValidTask(task) {
		// Also what keeps a newline out: it would render as another line
		// of schedules.md.
		return "", fmt.Errorf("task %q is not a task id (digits and dots, e.g. 3.2)", task)
	}
	sc := schedule.Schedule{ID: schedule.NewID(), Name: name, When: sp.String(),
		Instruction: strings.TrimSpace(req.Instruction), Task: strings.TrimSpace(req.Task), Allow: allow,
		State: schedule.Active, CreatedBy: by, Created: now}

	s.locked(func() {
		s.reloadLocked()
		err = s.admitLocked(name)
	})
	if err != nil {
		return "", err
	}

	head := "Add this schedule?"
	if by == "agent" {
		head = "The model wants to schedule:"
	}
	// schedules.auto_approve_create: the model's schedule skips the prompt
	// only when it asks for nothing beyond schedules.allow — a grant the
	// person already gives every fired turn (no grants at all counts). A person's own add still
	// confirms (it is the person), and the refusals above (a fired turn, an
	// untrusted page, min_interval, max_active) have all applied already.
	// Never with inherit_session_approvals on (owner's ruling, 2026-10-01):
	// a schedule added unasked would then also run with the session's
	// shortcuts, which is more than schedules.allow ever granted.
	auto := by == "agent" && a.Cfg.Schedules.AutoApproveCreate && !a.Cfg.Schedules.InheritSessionApprovals &&
		tools.GrantsCovered(allow, s.standing)
	if !auto && !a.askedYes(head+"\n\n"+describe(sc, sp, now)) {
		return "", errors.New("the schedule was not approved")
	}
	s.locked(func() {
		s.reloadLocked()
		if err = s.admitLocked(name); err != nil {
			return
		}
		// A finished one-off keeps its name in the timers; the new
		// schedule replaces it, so the name means one thing.
		// Timers only: a done section of schedules.md is the person's
		// file to tidy, never ours.
		kept := s.timers[:0]
		for _, o := range s.timers {
			if o.Name != name || o.State != schedule.Done {
				kept = append(kept, o)
			}
		}
		if len(kept) != len(s.timers) {
			s.timers = kept
			s.saveTimersLocked()
		}
		if err = s.putLocked(sc, sp.Recurring()); err == nil {
			s.approveLocked(sc)
		}
	})
	if err != nil {
		return "", err
	}
	s.kickLoop()
	if auto {
		a.notice("the model added schedule %q without asking (schedules.auto_approve_create: its grants are within schedules.allow)", name)
	}
	n, _ := sp.Next(now)
	return fmt.Sprintf("scheduled %q (%s); next %s", name, sp.String(), n.Format("Mon 15:04")), nil
}

// ScheduleAction is pause | resume | cancel | run. Narrowing never asks,
// except the model narrowing a person's schedule; resuming always asks
// (spec §3.3); only a person runs one now.
func (a *Agent) ScheduleAction(action, name, by string) (string, error) {
	s := a.sched
	if s == nil {
		return "", errSchedulesOff
	}
	if err := validBy(by); err != nil {
		return "", err
	}
	sc, ok, _ := s.lookup(name)
	if !ok {
		return "", fmt.Errorf("no schedule named %q", name)
	}
	now := s.clock.Now()
	switch action {
	case "pause", "cancel":
		if by == "agent" && sc.CreatedBy != "agent" {
			if a.Tools.Fired() {
				// It would ask a person, with no deadline (final review I6).
				return "", fmt.Errorf("the model cannot %s a person's schedule during a scheduled event; a person can with /schedule %s", action, action)
			}
			sp, err := sc.Spec()
			detail := sc.Name + "\n  when: " + sc.When
			if err == nil {
				detail = describe(sc, sp, now)
			}
			if !a.askedYes(fmt.Sprintf("The model wants to %s your schedule:\n\n%s", action, detail)) {
				return "", fmt.Errorf("%s of %q was not approved", action, name)
			}
		}
		var ok bool
		s.locked(func() {
			s.reloadLocked()
			var cur schedule.Schedule
			var project bool
			cur, ok, project = s.findLocked(sc.ID)
			if ok {
				if action == "cancel" {
					s.removeLocked(cur.ID)
					s.revokeLocked(cur.ID)
				} else {
					s.pauseLocked(cur, project)
				}
			}
			s.withdrawLocked(sc.ID)
		})
		if !ok {
			return "", fmt.Errorf("no schedule named %q", name)
		}
		s.kickLoop()
		if action == "cancel" {
			return name + ": cancelled", nil
		}
		return name + ": paused", nil
	case "resume":
		if by == "agent" && a.Tools.Fired() {
			return "", errDuringFired
		}
		if by == "agent" && a.Tools.UntrustedWeb() {
			return "", errors.New("a web page was read during this request; a person can resume it with /schedule resume")
		}
		if sc.State == schedule.Done {
			return "", fmt.Errorf("%q already ran; add it again", name)
		}
		sp, err := sc.Spec()
		if err != nil {
			return "", fmt.Errorf("%q cannot be resumed: %v", name, err)
		}
		// Resuming is widening: add's refusals apply, before the prompt
		// (a refusal never asks) and again after it.
		check := func() error {
			if err := a.tooFrequent(sp, s.clock.Now()); err != nil {
				return err
			}
			if max := a.Cfg.Schedules.MaxActive; max > 0 {
				if n := s.activeLocked(sc.ID); n >= max {
					return maxActiveErr(n)
				}
			}
			return nil
		}
		s.locked(func() {
			s.reloadLocked()
			err = check()
		})
		if err != nil {
			return "", err
		}
		detail := "Resume this schedule?\n\n" + describe(sc, sp, now)
		if due, ok := sp.Due(); ok && !due.After(now) {
			detail += "\n  its time has passed; it will run as soon as it is resumed"
		}
		if !a.askedYes(detail) {
			return "", fmt.Errorf("resuming %q was not approved", name)
		}
		var ok, same bool
		s.locked(func() {
			s.reloadLocked()
			var cur schedule.Schedule
			var project bool
			cur, ok, project = s.findLocked(sc.ID)
			same = ok && cur.Hash() == sc.Hash()
			if same {
				err = check()
			}
			if same && err == nil {
				cur.State, cur.Failures = schedule.Active, 0
				s.putLocked(cur, project)
				s.approveLocked(cur)
				delete(s.held, cur.ID)
				delete(s.unconfirmed, cur.ID)
			}
		})
		switch {
		case !ok:
			return "", fmt.Errorf("no schedule named %q", name)
		case !same:
			return "", fmt.Errorf("%q changed while you were asked; nothing was resumed", name)
		case err != nil:
			return "", err
		}
		s.kickLoop()
		return name + ": active", nil
	case "run":
		if by != "person" {
			return "", errors.New("only a person can run a schedule now")
		}
		var approved, waiting bool
		s.locked(func() {
			approved = s.approvedLocked(sc)
			waiting = s.waitingLocked(sc.ID)
		})
		switch {
		case !approved:
			return "", fmt.Errorf("%q changed since it was approved; /schedule resume %s first", name, name)
		case waiting:
			return name + " is already queued", nil
		}
		if !a.fireNow(sc, true) {
			return name + " is already queued", nil
		}
		return name + ": queued", nil
	}
	return "", fmt.Errorf("unknown action %q (pause, resume, cancel, run)", action)
}

// ScheduleLines is /schedule's table.
func (a *Agent) ScheduleLines() []string {
	s := a.sched
	if s == nil {
		return []string{errSchedulesOff.Error()}
	}
	var all []schedule.Schedule
	s.locked(func() {
		s.reloadLocked()
		all = s.allLocked()
	})
	if len(all) == 0 {
		return []string{"no schedules; /schedule add <name> <when> -- <instruction>"}
	}
	out := []string{fmt.Sprintf("%-18s %-18s %-12s %-7s %-10s %s", "NAME", "WHEN", "NEXT", "STATE", "BY", "LAST")}
	for _, sc := range all {
		next := "-"
		if n, ok := sc.NextDue(); ok {
			next = n.Format("Mon 15:04")
		}
		last := "-"
		if !sc.LastRun.IsZero() {
			last = sc.LastRun.Format("01-02 15:04") + " " + sc.LastOutcome
		}
		out = append(out, fmt.Sprintf("%-18s %-18s %-12s %-7s %-10s %s", sc.Name, sc.When, next, sc.State, byLabel(sc.CreatedBy), last))
	}
	return out
}

// ScheduleShow is one schedule in full.
func (a *Agent) ScheduleShow(name string) ([]string, error) {
	if a.sched == nil {
		return nil, errSchedulesOff
	}
	sc, ok, project := a.sched.lookup(name)
	if !ok {
		return nil, fmt.Errorf("no schedule named %q", name)
	}
	sp, err := sc.Spec()
	if err != nil {
		return nil, err
	}
	where := "this session (one-off)"
	if project {
		where = ".be-code/schedules.md"
	}
	lines := strings.Split(describe(sc, sp, a.sched.clock.Now()), "\n")
	lines = append(lines, "  state: "+string(sc.State), "  set by: "+byLabel(sc.CreatedBy), "  stored in: "+where)
	if !sc.LastRun.IsZero() {
		lines = append(lines, fmt.Sprintf("  last run: %s — %s", sc.LastRun.Format("2006-01-02 15:04"), sc.LastOutcome))
	}
	return lines, nil
}

// NextSchedule is the soonest active schedule with no event already
// waiting, for the status line. It reads a snapshot published lock-free by
// the scheduler (publishNextLocked) rather than taking its mutex, which is
// held across schedules.md/session saves — a slow disk write must not
// stall every terminal's render.
func (a *Agent) NextSchedule() (string, time.Time, bool) {
	s := a.sched
	if s == nil {
		return "", time.Time{}, false
	}
	snap := s.next.Load()
	if snap == nil || snap.name == "" {
		return "", time.Time{}, false
	}
	return snap.name, snap.at, true
}

// StartSchedules raises the startup prompt, when anything is active, and
// starts the loop (spec §3.5). It runs once per scheduler; a second call is
// a no-op. Plain mode calls it on its own goroutine's way into the REPL;
// the TUI uses StartSchedulesAsync.
func (a *Agent) StartSchedules() {
	s := a.sched
	if s == nil {
		return
	}
	s.gateOnce.Do(func() {
		a.gateSchedules(0, "These scheduled events will run while this session is open:")
		s.startLoop()
	})
}

// gateSchedules is the startup-style prompt. gen 0 is the startup prompt
// proper: every active schedule not held for a prompt of its own. gen > 0
// is the prompt for the timers loadTimers held under that generation.
// yes approves exactly what was shown — an edit made while the prompt was
// open stays unapproved, and the loop pauses it at its time — except a
// schedule add would refuse now (too frequent, past max_active), which
// stays paused with a notice. no pauses every one shown (and revokes its
// approval). A prompt nobody answered (withdrawn: the session quit, or the
// prompt was taken down) changes nothing and saves nothing: the startup
// prompt's schedules stay unconfirmed for this process and the next start
// asks again; held timers simply stay held (final review I2). Never asks
// under s.mu.
func (a *Agent) gateSchedules(gen int, head string) {
	s := a.sched
	var shown []schedule.Schedule
	var b strings.Builder
	confirm := a.Cfg.Schedules.ConfirmOnStart
	silent := 0
	s.locked(func() {
		if gen == 0 {
			s.gateBegun = true
		}
		s.reloadLocked()
		now := s.clock.Now()
		b.WriteString(head + "\n")
		var cands []schedule.Schedule
		isCand := map[string]bool{}
		for _, sc := range s.allLocked() {
			if sc.State != schedule.Active || (gen == 0) != (s.held[sc.ID] == 0) ||
				(gen != 0 && s.held[sc.ID] != gen) {
				continue
			}
			cands = append(cands, sc)
			isCand[sc.ID] = true
		}
		// count is what is already armed outside this prompt: active, not a
		// candidate here, not held for a prompt of its own — the same base
		// the "yes" path counts from, so the silent path below enforces
		// schedules.max_active exactly as a "yes" would.
		count := 0
		for _, o := range s.allLocked() {
			if o.State == schedule.Active && !isCand[o.ID] && s.held[o.ID] == 0 {
				count++
			}
		}
		max := a.Cfg.Schedules.MaxActive
		for _, sc := range cands {
			if !confirm && s.approvedLocked(sc) {
				// schedules.confirm_on_start false: what a person approved,
				// unchanged since, starts without being shown again —
				// unless add would refuse it now (min_interval raised
				// since, or max_active lowered or already reached), which
				// pauses it exactly as a "yes" would. A held timer of this
				// kind is released here, before the prompt for the rest, so
				// a withdrawn prompt never keeps it held.
				_, _, project := s.findLocked(sc.ID)
				if sp, err := sc.Spec(); err == nil {
					if ferr := a.tooFrequent(sp, now); ferr != nil {
						s.pauseLocked(sc, project)
						s.noteLocked("schedule %q stays paused: %v", sc.Name, ferr)
						continue
					}
				}
				if max > 0 && count >= max {
					s.pauseLocked(sc, project)
					s.noteLocked("schedule %q stays paused: %v", sc.Name, maxActiveErr(count))
					continue
				}
				count++
				silent++
				delete(s.held, sc.ID)
				continue
			}
			shown = append(shown, sc)
			if gen == 0 {
				// Not queued while it is being asked about: the loop may
				// already be running for what was let through silently.
				s.asking[sc.ID] = true
			}
			mark := ""
			if !s.approvedLocked(sc) {
				mark = "   (changed since approved)"
			}
			if sp, err := sc.Spec(); err == nil {
				fmt.Fprintf(&b, "\n%s", describeMarked(sc, sp, now, mark))
			} else {
				fmt.Fprintf(&b, "\n%s%s\n  when: %s (unreadable: %v)", sc.Name, mark, sc.When, err)
			}
		}
	})
	if len(shown) == 0 {
		a.releaseHeld(gen)
		s.kickLoop() // a held timer released above is due now
		return
	}
	if gen == 0 && silent > 0 {
		// What was let through silently need not wait on a prompt about
		// the others, which may stay open as long as nobody answers; the
		// others are held out of the loop by s.asking until it is.
		s.startLoop()
	}
	defer func() {
		if gen == 0 {
			s.locked(func() {
				for _, sc := range shown {
					delete(s.asking, sc.ID)
				}
			})
			s.kickLoop()
		}
	}()
	s.kickLoop()
	yes, withdrawn := a.askSchedule(b.String())
	if withdrawn {
		if gen == 0 {
			s.locked(func() {
				for _, sc := range shown {
					s.unconfirmed[sc.ID] = true
				}
			})
			a.notice("the scheduled-events question was not answered; they will not run this session unless you /schedule resume one")
		}
		return
	}
	paused := 0
	s.locked(func() {
		s.reloadLocked()
		now := s.clock.Now()
		ids := map[string]bool{}
		for _, sc := range shown {
			ids[sc.ID] = true
		}
		count := 0
		for _, o := range s.allLocked() {
			if o.State == schedule.Active && !ids[o.ID] && s.held[o.ID] == 0 {
				count++
			}
		}
		max := a.Cfg.Schedules.MaxActive
		for _, sc := range shown {
			cur, ok, project := s.findLocked(sc.ID)
			if !ok || cur.State != schedule.Active {
				continue
			}
			if gen != 0 && s.held[cur.ID] != gen {
				// A later switch replaced these timers (or held them again
				// under a newer prompt): this answer is stale for it.
				continue
			}
			pause := func(why error) {
				s.pauseLocked(cur, project)
				paused++
				if why != nil {
					s.noteLocked("schedule %q stays paused: %v", cur.Name, why)
				}
			}
			if !yes {
				pause(nil)
				continue
			}
			if sp, err := cur.Spec(); err == nil {
				if ferr := a.tooFrequent(sp, now); ferr != nil {
					pause(ferr) // add would refuse it: never left armed
					continue
				}
			}
			if cur.Hash() != sc.Hash() {
				count++ // still active; the loop pauses it when it comes due
				continue
			}
			if max > 0 && count >= max {
				pause(maxActiveErr(count))
				continue
			}
			s.approveLocked(cur)
			count++
		}
		s.releaseHeldLocked(gen)
	})
	if !yes && paused > 0 {
		a.notice("%d scheduled events paused; /schedule resume <name> brings one back", paused)
	}
	s.kickLoop()
}

// ConfirmHeldTimers raises the prompt for the timers the latest session
// switch held (loadTimers): yes approves what was shown, no pauses them, a
// prompt nobody answered leaves them held. A no-op when nothing is held.
// It asks through the approval seam and blocks until answered, so the
// caller picks the goroutine: plain mode calls it on the REPL goroutine
// right after the switch, the TUI on a goroutine of its own.
func (a *Agent) ConfirmHeldTimers() {
	s := a.sched
	if s == nil {
		return
	}
	var gen int
	waiting := false
	s.locked(func() {
		gen = s.holdGen
		for _, g := range s.held {
			if g == gen {
				waiting = true
			}
		}
	})
	if gen == 0 || !waiting {
		return
	}
	// Called on a bare goroutine by both UIs, with no fence of their own —
	// a panicking approver (or anything else downstream of Approve) must
	// not take the session host down with it. The timers simply stay held.
	defer func() {
		if r := recover(); r != nil {
			a.notice("confirming this session's timers failed (%v); they stay held", r)
		}
	}()
	a.gateSchedules(gen, "This session's timers will run while it is open:")
}

// releaseHeld drops the holds of generation gen (0: none).
func (a *Agent) releaseHeld(gen int) {
	if gen == 0 {
		return
	}
	s := a.sched
	s.locked(func() { s.releaseHeldLocked(gen) })
	s.kickLoop()
}

func (s *scheduler) releaseHeldLocked(gen int) {
	if gen == 0 {
		return
	}
	for id, g := range s.held {
		if g == gen {
			delete(s.held, id)
		}
	}
}

// StartSchedulesAsync is StartSchedules on a goroutine of its own, fenced
// like StartSubAgentsAsync: the prompt can wait as long as a person takes,
// and a terminal attaching later is shown it.
func (a *Agent) StartSchedulesAsync() {
	if a.sched == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				a.notice("schedules failed to start (%v); none will run this session", r)
			}
		}()
		a.StartSchedules()
	}()
}
