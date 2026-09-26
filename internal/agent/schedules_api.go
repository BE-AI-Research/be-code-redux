package agent

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/schedule"
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

// SchedulesEnabled reports whether this session has a scheduler.
func (a *Agent) SchedulesEnabled() bool { return a.sched != nil }

// askSchedule raises the one "schedule" prompt (no "always"; spec §3.1). No
// approver is a refusal.
func (a *Agent) askSchedule(detail string) bool {
	return a.Tools.Approve != nil && a.Tools.Approve("schedule", detail)
}

func byLabel(by string) string {
	if by == "agent" {
		return "the model"
	}
	return "you"
}

// describe is the whole schedule for one prompt: nothing is asked later.
func describe(sc schedule.Schedule, sp schedule.Spec) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n  when: %s\n", sc.Name, sc.When)
	var nexts []string
	t := sc.Created
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
			fmt.Fprintf(&b, "    %s\n", line)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// admitLocked refuses a name already in use and an addition past
// schedules.max_active. Checked before the prompt, so a refusal never asks,
// and again after it, since the file may have changed meanwhile.
func (s *scheduler) admitLocked(name string) error {
	active := 0
	for _, o := range s.allLocked() {
		if o.Name == name && o.State != schedule.Done {
			return fmt.Errorf("a schedule named %q already exists; cancel it first", name)
		}
		if o.State == schedule.Active {
			active++
		}
	}
	if max := s.a.Cfg.Schedules.MaxActive; max > 0 && active >= max {
		return fmt.Errorf("%d schedules are already active (schedules.max_active); cancel one first", active)
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
	if by == "agent" && a.Tools.UntrustedWeb() {
		return "", errors.New("a web page was read during this request, so the model cannot create schedules until a person's next request; a person can add it with /schedule add")
	}
	minGap, _, _ := a.Cfg.Schedules.Durations()
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
	if g := sp.ShortestGap(now); sp.Recurring() && g < minGap {
		return "", fmt.Errorf("%q runs more often than every %s (schedules.min_interval)", sp.String(), minGap)
	}
	if due, ok := sp.Due(); ok && !due.After(now) {
		return "", fmt.Errorf("that time has already passed (%s)", due.Format("2006-01-02 15:04"))
	}
	allow, err := schedule.ParseAllowance(req.Allow)
	if err != nil {
		return "", err
	}
	sc := schedule.Schedule{ID: schedule.NewID(), Name: name, When: sp.String(),
		Instruction: strings.TrimSpace(req.Instruction), Task: strings.TrimSpace(req.Task), Allow: allow,
		State: schedule.Active, CreatedBy: by, Created: now}

	s.mu.Lock()
	s.reloadLocked()
	err = s.admitLocked(name)
	s.unlock()
	if err != nil {
		return "", err
	}

	head := "Add this schedule?"
	if by == "agent" {
		head = "The model wants to schedule:"
	}
	if !a.askSchedule(head + "\n\n" + describe(sc, sp)) {
		return "", errors.New("the schedule was not approved")
	}
	s.mu.Lock()
	s.reloadLocked()
	if err = s.admitLocked(name); err == nil {
		if err = s.putLocked(sc, sp.Recurring()); err == nil {
			s.approveLocked(sc)
		}
	}
	s.unlock()
	if err != nil {
		return "", err
	}
	s.kickLoop()
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
	sc, ok, _ := s.lookup(name)
	if !ok {
		return "", fmt.Errorf("no schedule named %q", name)
	}
	switch action {
	case "pause", "cancel":
		if by == "agent" && sc.CreatedBy != "agent" {
			sp, err := sc.Spec()
			detail := sc.Name + "\n  when: " + sc.When
			if err == nil {
				detail = describe(sc, sp)
			}
			if !a.askSchedule(fmt.Sprintf("The model wants to %s your schedule:\n\n%s", action, detail)) {
				return "", fmt.Errorf("%s of %q was not approved", action, name)
			}
		}
		s.mu.Lock()
		s.reloadLocked()
		cur, ok, project := s.findLocked(sc.ID)
		if ok {
			if action == "cancel" {
				s.removeLocked(cur.ID)
			} else {
				cur.State = schedule.Paused
				s.putLocked(cur, project)
			}
		}
		s.withdrawLocked(sc.ID)
		s.unlock()
		if !ok {
			return "", fmt.Errorf("no schedule named %q", name)
		}
		s.kickLoop()
		if action == "cancel" {
			return name + ": cancelled", nil
		}
		return name + ": paused", nil
	case "resume":
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
		if !a.askSchedule("Resume this schedule?\n\n" + describe(sc, sp)) {
			return "", fmt.Errorf("resuming %q was not approved", name)
		}
		s.mu.Lock()
		s.reloadLocked()
		cur, ok, project := s.findLocked(sc.ID)
		if ok && cur.Hash() == sc.Hash() {
			cur.State, cur.Failures = schedule.Active, 0
			s.putLocked(cur, project)
			s.approveLocked(cur)
		}
		s.unlock()
		switch {
		case !ok:
			return "", fmt.Errorf("no schedule named %q", name)
		case cur.Hash() != sc.Hash():
			return "", fmt.Errorf("%q changed while you were asked; nothing was resumed", name)
		}
		s.kickLoop()
		return name + ": active", nil
	case "run":
		if by != "person" {
			return "", errors.New("only a person can run a schedule now")
		}
		s.mu.Lock()
		approved := s.approvedLocked(sc)
		waiting := s.waitingLocked(sc.ID)
		s.unlock()
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
	s.mu.Lock()
	s.reloadLocked()
	all := s.allLocked()
	s.unlock()
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
	lines := strings.Split(describe(sc, sp), "\n")
	lines = append(lines, "  state: "+string(sc.State), "  set by: "+byLabel(sc.CreatedBy), "  stored in: "+where)
	if !sc.LastRun.IsZero() {
		lines = append(lines, fmt.Sprintf("  last run: %s — %s", sc.LastRun.Format("2006-01-02 15:04"), sc.LastOutcome))
	}
	return lines, nil
}

// NextSchedule is the soonest active schedule with no event already
// waiting, for the status line.
func (a *Agent) NextSchedule() (string, time.Time, bool) {
	s := a.sched
	if s == nil {
		return "", time.Time{}, false
	}
	s.mu.Lock()
	all := s.allLocked() // no reload: the status line is redrawn often
	waiting := map[string]bool{}
	for _, sc := range all {
		if s.waitingLocked(sc.ID) {
			waiting[sc.ID] = true
		}
	}
	s.unlock()
	var name string
	var at time.Time
	for _, sc := range all {
		if n, ok := sc.NextDue(); ok && !waiting[sc.ID] && (name == "" || n.Before(at)) {
			name, at = sc.Name, n
		}
	}
	return name, at, name != ""
}

// StartSchedules raises the startup prompt, when anything is active, and
// starts the loop (spec §3.5). yes approves exactly what was shown; no
// pauses every one of them. Plain mode calls it on its own goroutine's way
// into the REPL; the TUI uses StartSchedulesAsync.
func (a *Agent) StartSchedules() {
	s := a.sched
	if s == nil {
		return
	}
	s.mu.Lock()
	s.reloadLocked()
	var active []schedule.Schedule
	var b strings.Builder
	b.WriteString("These scheduled events will run while this session is open:\n")
	for _, sc := range s.allLocked() {
		if sc.State != schedule.Active {
			continue
		}
		active = append(active, sc)
		mark := ""
		if !s.approvedLocked(sc) {
			mark = "   (changed since approved)"
		}
		if sp, err := sc.Spec(); err == nil {
			fmt.Fprintf(&b, "\n%s%s", describe(sc, sp), mark)
		} else {
			fmt.Fprintf(&b, "\n%s\n  when: %s (unreadable: %v)%s", sc.Name, sc.When, err, mark)
		}
	}
	s.unlock()
	if len(active) > 0 {
		yes := a.askSchedule(b.String())
		s.mu.Lock()
		s.reloadLocked()
		for _, sc := range active {
			cur, ok, project := s.findLocked(sc.ID)
			switch {
			case !ok:
			case yes && cur.Hash() == sc.Hash():
				// Only what was shown: an edit made while the prompt was
				// open stays unapproved, and the loop pauses it at its time.
				s.approveLocked(cur)
			case !yes && cur.State == schedule.Active:
				cur.State = schedule.Paused
				s.putLocked(cur, project)
			}
		}
		s.unlock()
		if !yes {
			a.notice("%d scheduled events paused; /schedule resume <name> brings one back", len(active))
		}
	}
	s.startLoop()
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
