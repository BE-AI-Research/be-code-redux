package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/schedule"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// Tests for the four schedule settings (schedules.confirm_on_start,
// inherit_session_approvals, allow, auto_approve_create; brief
// .superpowers/sdd/2026-10-01-schedule-settings). Each is tested on and
// off; the safety rules that must hold with every one of them on are at
// the end.

// settingsAgent is schedAgent with the schedules block set before the
// scheduler is enabled (schedules.allow is read then).
func settingsAgent(t *testing.T, p provider.Provider, set func(*config.SchedulesConfig)) (*Agent, *schedule.FakeClock, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	ag, dir := newTestAgent(t, p, func(c *config.Config) {
		c.Schedules.PauseAfterFailures = 2
		if set != nil {
			set(&c.Schedules)
		}
	})
	ag.Session = &store.Session{ID: "s1"}
	clock := schedule.NewFakeClock(t0)
	ag.EnableSchedules(clock)
	t.Cleanup(ag.StopSchedules)
	return ag, clock, dir
}

// allOn turns every setting on, with a standing allowance.
func allOn(s *config.SchedulesConfig) {
	s.ConfirmOnStart = false
	s.InheritSessionApprovals = true
	s.Allow = []string{"shell: go test*", "write: docs", "browser: *.example.com"}
	s.AutoApproveCreate = true
}

// safeLog is an approver log safe to append to from the loop goroutine.
type safeLog struct {
	mu  sync.Mutex
	log []string
}

func (l *safeLog) approver(answer bool) func(string, string) bool {
	return func(action, detail string) bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.log = append(l.log, action+"|"+detail)
		return answer
	}
}

func (l *safeLog) get() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.log...)
}

// --- confirm_on_start --------------------------------------------------------

func TestConfirmOnStartOffStartsApprovedSilently(t *testing.T) {
	ag, clock, _ := settingsAgent(t, &scriptedProvider{}, func(s *config.SchedulesConfig) { s.ConfirmOnStart = false })
	seed(t, ag, mk("a", "every 30m"), true)
	var l safeLog
	ag.Tools.Approve = l.approver(false)
	ag.StartSchedules()
	if got := l.get(); len(got) != 0 {
		t.Fatalf("nothing needs asking, so no prompt: %v", got)
	}
	clock.Advance(31 * time.Minute)
	waitQueued(t, ag, 1)
}

func TestConfirmOnStartOffStillAsksForChangedAndNew(t *testing.T) {
	ag, clock, _ := settingsAgent(t, &scriptedProvider{}, func(s *config.SchedulesConfig) { s.ConfirmOnStart = false })
	seed(t, ag, mk("kept", "every 30m"), true)
	b := mk("edited", "every 30m")
	seed(t, ag, b, true)
	ag.sched.mu.Lock()
	b.Instruction = "something else" // hand-edited: the approval no longer matches
	ag.sched.putLocked(b, true)
	ag.sched.putLocked(mk("fresh", "every 30m"), true) // never approved
	ag.sched.mu.Unlock()
	var l safeLog
	ag.Tools.Approve = l.approver(false)
	ag.StartSchedules()
	got := l.get()
	if len(got) != 1 || !strings.Contains(got[0], "edited") || !strings.Contains(got[0], "fresh") ||
		strings.Contains(got[0], "kept\n") {
		t.Fatalf("one prompt listing only what needs asking: %v", got)
	}
	for name, want := range map[string]schedule.State{"kept": schedule.Active, "edited": schedule.Paused, "fresh": schedule.Paused} {
		if sc := mustFind(t, ag, name); sc.State != want {
			t.Fatalf("%s: %s, want %s", name, sc.State, want)
		}
	}
	clock.Advance(31 * time.Minute)
	if items := waitQueued(t, ag, 1); items[0].ScheduleName != "kept" {
		t.Fatalf("the approved one runs: %+v", items)
	}
}

// With confirm_on_start on (the default) the approved one is listed and a
// "no" pauses it too.
func TestConfirmOnStartOnAsksForApproved(t *testing.T) {
	ag, _, _ := settingsAgent(t, &scriptedProvider{}, nil)
	seed(t, ag, mk("a", "every 30m"), true)
	var l safeLog
	ag.Tools.Approve = l.approver(false)
	ag.StartSchedules()
	if got := l.get(); len(got) != 1 || !strings.Contains(got[0], "a\n  when:") {
		t.Fatalf("asked: %v", got)
	}
	if mustFind(t, ag, "a").State != schedule.Paused {
		t.Fatal("no pauses it")
	}
}

// An approved schedule that add would now refuse (min_interval raised
// since) is not started silently: it stays paused, as a "yes" would leave it.
func TestConfirmOnStartOffPausesTooFrequent(t *testing.T) {
	ag, _, _ := settingsAgent(t, &scriptedProvider{}, func(s *config.SchedulesConfig) { s.ConfirmOnStart = false })
	seed(t, ag, mk("a", "every 30m"), true)
	ag.Cfg.Schedules.MinInterval = "1h"
	var l safeLog
	ag.Tools.Approve = l.approver(true)
	ag.StartSchedules()
	if len(l.get()) != 0 || mustFind(t, ag, "a").State != schedule.Paused {
		t.Fatalf("paused without asking: %v %+v", l.get(), mustFind(t, ag, "a"))
	}
}

func TestConfirmOnStartOffReleasesApprovedHeldTimers(t *testing.T) {
	for _, confirm := range []bool{false, true} {
		ag, clock, _ := settingsAgent(t, &scriptedProvider{}, func(s *config.SchedulesConfig) { s.ConfirmOnStart = confirm })
		var l safeLog
		ag.Tools.Approve = l.approver(true)
		ag.StartSchedules()
		tm := mk("t", "in 1m")
		seed(t, ag, tm, false) // approved in an earlier process
		ag.SetSession(&store.Session{ID: "s0"})
		clock.Advance(2 * time.Minute)
		ag.SetSession(&store.Session{ID: "s2", Timers: []schedule.Schedule{tm}})
		ag.ConfirmHeldTimers()
		if asked := len(l.get()) == 1; asked != confirm {
			t.Fatalf("confirm_on_start=%v: asked %v", confirm, l.get())
		}
		waitQueued(t, ag, 1)
		ag.StopSchedules()
	}
}

func TestConfirmOnStartOffStillAsksForChangedHeldTimer(t *testing.T) {
	ag, clock, _ := settingsAgent(t, &scriptedProvider{}, func(s *config.SchedulesConfig) { s.ConfirmOnStart = false })
	var l safeLog
	ag.Tools.Approve = l.approver(false)
	ag.StartSchedules()
	ok := mk("ok", "in 1m")
	seed(t, ag, ok, false)
	ag.SetSession(&store.Session{ID: "s0"})
	odd := mk("odd", "in 1m") // never approved
	clock.Advance(2 * time.Minute)
	ag.SetSession(&store.Session{ID: "s2", Timers: []schedule.Schedule{ok, odd}})
	ag.ConfirmHeldTimers()
	got := l.get()
	if len(got) != 1 || !strings.Contains(got[0], "odd") || strings.Contains(got[0], "ok\n") {
		t.Fatalf("asks for the unapproved timer only: %v", got)
	}
	if items := waitQueued(t, ag, 1); items[0].ScheduleName != "ok" {
		t.Fatalf("the approved timer runs: %+v", items)
	}
	if mustFind(t, ag, "odd").State != schedule.Paused {
		t.Fatal("no pauses the one asked about")
	}
}

// --- inherit_session_approvals ------------------------------------------------

// runOneFired queues sc (seeded, approved), drains it and runs its turn.
func runOneFired(t *testing.T, ag *Agent, clock *schedule.FakeClock, sc schedule.Schedule) {
	t.Helper()
	seed(t, ag, sc, false)
	ag.sched.startLoop()
	clock.Advance(2 * time.Minute)
	waitQueued(t, ag, 1)
	items := ag.DrainForTurn()
	if _, _, err := ag.RunFull(context.Background(), items[0].Text); err != nil {
		t.Fatal(err)
	}
}

func writeScript(path string) *scriptedProvider {
	return &scriptedProvider{responses: []provider.ChatResponse{
		{ToolCalls: []provider.ToolCall{{ID: "1", Name: "write_file", Arguments: `{"path":"` + path + `","content":"x"}`}}},
		{Content: "done"},
	}}
}

func TestInheritSessionApprovalsInFiredTurn(t *testing.T) {
	for _, inherit := range []bool{false, true} {
		ag, clock, dir := settingsAgent(t, writeScript("src/y.go"), func(s *config.SchedulesConfig) { s.InheritSessionApprovals = inherit })
		var l safeLog
		ag.Tools.Approve = l.approver(false)
		ag.Tools.ApproveWrites = false // the session's accept-all
		var shortcuts []bool
		ag.Tools.ApproveCtx = func(ctx context.Context, action, detail string) bool {
			shortcuts = append(shortcuts, tools.SessionShortcuts(ctx))
			return false
		}
		runOneFired(t, ag, clock, mk("w", "in 1m"))
		_, err := os.Stat(filepath.Join(dir, "src", "y.go"))
		if landed := err == nil; landed != inherit {
			t.Fatalf("inherit=%v: write landed=%v (asked %v)", inherit, landed, shortcuts)
		}
		if !inherit && (len(shortcuts) != 1 || shortcuts[0]) {
			t.Fatalf("without inherit the write asks, marked: %v", shortcuts)
		}
		if ag.Tools.Fired() {
			t.Fatal("the allowance is cleared after the turn")
		}
	}
}

// --- allow --------------------------------------------------------------------

func TestStandingAllowMergedIntoFiredTurn(t *testing.T) {
	for _, standing := range [][]string{nil, {"write: docs", "shell: *", "write: ../out"}} {
		ag, clock, dir := settingsAgent(t, writeScript("docs/r.md"), func(s *config.SchedulesConfig) { s.Allow = standing })
		var l safeLog
		ag.Tools.Approve = l.approver(false)
		ag.Tools.ApproveWrites = true
		runOneFired(t, ag, clock, mk("r", "in 1m"))
		_, err := os.Stat(filepath.Join(dir, "docs", "r.md"))
		if landed := err == nil; landed != (standing != nil) {
			t.Fatalf("allow=%v: landed=%v asked=%v", standing, landed, l.get())
		}
	}
}

func TestStandingAllowNotUsedOutsideFiredTurn(t *testing.T) {
	ag, _, dir := settingsAgent(t, writeScript("docs/r.md"), func(s *config.SchedulesConfig) { s.Allow = []string{"write: docs"} })
	var l safeLog
	ag.Tools.Approve = l.approver(false)
	ag.Tools.ApproveWrites = true
	if _, _, err := ag.RunFull(context.Background(), "write it"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "docs", "r.md")); err == nil || len(l.get()) != 1 {
		t.Fatalf("an ordinary request asks: %v", l.get())
	}
}

func TestStandingAllowNeverCoversSchedulesFile(t *testing.T) {
	ag, clock, dir := settingsAgent(t, writeScript(".be-code/schedules.md"), func(s *config.SchedulesConfig) { s.Allow = []string{"write: ."} })
	var l safeLog
	ag.Tools.Approve = l.approver(false)
	ag.Tools.ApproveWrites = true
	before, _ := os.ReadFile(filepath.Join(dir, ".be-code", "schedules.md"))
	runOneFired(t, ag, clock, mk("r", "in 1m"))
	after, _ := os.ReadFile(filepath.Join(dir, ".be-code", "schedules.md"))
	if string(before) != string(after) || len(l.get()) != 1 {
		t.Fatalf("schedules.md is protected from a standing grant: asked=%v", l.get())
	}
}

// --- auto_approve_create ----------------------------------------------------

func TestAutoApproveCreateCoveredVersusWider(t *testing.T) {
	set := func(s *config.SchedulesConfig) {
		s.AutoApproveCreate = true
		s.Allow = []string{"shell: go test*", "write: docs"}
	}
	ag, _, _ := settingsAgent(t, &scriptedProvider{}, set)
	var l safeLog
	ag.Tools.Approve = l.approver(false)
	if _, err := ag.AddSchedule(schedule.Request{Name: "tests", When: "every 1h", Instruction: "run tests",
		Allow: []string{"shell: go test ./...", "write: docs/reports"}}, "agent"); err != nil {
		t.Fatalf("covered: added without asking: %v", err)
	}
	if len(l.get()) != 0 {
		t.Fatalf("covered asks nothing: %v", l.get())
	}
	sc := mustFind(t, ag, "tests")
	if !approvedNow(ag, sc.ID) || sc.CreatedBy != "agent" {
		t.Fatalf("stored and approved: %+v", sc)
	}
	// Wider than schedules.allow: asks, and a no refuses.
	if _, err := ag.AddSchedule(schedule.Request{Name: "wide", When: "every 1h", Instruction: "x",
		Allow: []string{"shell: go test ./...", "write: src"}}, "agent"); err == nil {
		t.Fatal("a wider grant is not auto-approved")
	}
	if got := l.get(); len(got) != 1 || !strings.HasPrefix(got[0], "schedule|") {
		t.Fatalf("asked once: %v", got)
	}
	// A person's own add keeps its confirmation.
	if _, err := ag.AddSchedule(schedule.Request{Name: "mine", When: "every 1h", Instruction: "x",
		Allow: []string{"write: docs"}}, "person"); err == nil || len(l.get()) != 2 {
		t.Fatalf("a person's add asks: %v %v", err, l.get())
	}
	// Add's own refusals still apply.
	if _, err := ag.AddSchedule(schedule.Request{Name: "fast", When: "every 1m", Instruction: "x"}, "agent"); err == nil {
		t.Fatal("min_interval still refuses")
	}
}

func TestAutoApproveCreateOffAsks(t *testing.T) {
	ag, _, _ := settingsAgent(t, &scriptedProvider{}, func(s *config.SchedulesConfig) { s.Allow = []string{"write: docs"} })
	var l safeLog
	ag.Tools.Approve = l.approver(true)
	if _, err := ag.AddSchedule(schedule.Request{Name: "r", When: "every 1h", Instruction: "x",
		Allow: []string{"write: docs"}}, "agent"); err != nil {
		t.Fatal(err)
	}
	if len(l.get()) != 1 {
		t.Fatalf("off: the model's add asks: %v", l.get())
	}
}

// --- every setting on: what must stay true ------------------------------------

func TestAllSettingsOnKeepTheSafetyRules(t *testing.T) {
	ag, clock, _ := settingsAgent(t, &scriptedProvider{}, allOn)
	var l safeLog
	ag.Tools.Approve = l.approver(true)

	// The model cannot create a schedule after an untrusted page, even a
	// covered one.
	ag.Tools.MarkUntrustedWeb()
	if _, err := ag.AddSchedule(schedule.Request{Name: "n", When: "every 1h", Instruction: "x"}, "agent"); err == nil {
		t.Fatal("refused after a web page")
	}
	ag.Tools.ClearUntrustedWeb()

	// Nor during a fired turn, nor resume one; nor consent an online
	// co-worker.
	idle := mk("idle", "every 30m")
	idle.CreatedBy, idle.State = "agent", schedule.Paused
	seed(t, ag, idle, true)
	ag.Tools.SetFiredPolicy(nil, time.Minute, true)
	if _, err := ag.AddSchedule(schedule.Request{Name: "n", When: "every 1h", Instruction: "x"}, "agent"); err == nil ||
		!strings.Contains(err.Error(), "during a scheduled event") {
		t.Fatalf("refused during a fired turn: %v", err)
	}
	if _, err := ag.ScheduleAction("resume", "idle", "agent"); err == nil {
		t.Fatal("resume refused during a fired turn")
	}
	if ag.consent(config.CoworkerConfig{Name: "cloud", Online: true}, ConsultRequest{Origin: "tool"}) {
		t.Fatal("an online co-worker is still declined during a fired turn")
	}
	if !ag.firedTurn() {
		t.Fatal("an inheriting fired turn is still a fired turn (no sub-agent dispatch)")
	}
	ag.Tools.ClearAllowance()
	if len(l.get()) != 0 {
		t.Fatalf("nothing asked so far: %v", l.get())
	}

	// Resume always asks, even a covered schedule.
	if _, err := ag.ScheduleAction("resume", "idle", "agent"); err != nil {
		t.Fatal(err)
	}
	if got := l.get(); len(got) != 1 || !strings.HasPrefix(got[0], "schedule|Resume") {
		t.Fatalf("resume asked: %v", got)
	}

	// An edit after approval pauses at run time, with nobody asked.
	ag.Tools.Approve = l.approver(false)
	sc := mk("e", "every 30m")
	seed(t, ag, sc, true)
	ag.StartSchedules() // nothing needs asking
	editInstruction(t, ag, "e", "something else")
	clock.Advance(31 * time.Minute)
	settle(t, ag)
	if mustFind(t, ag, "e").State != schedule.Paused {
		t.Fatal("an edited schedule pauses until re-approved")
	}
	if got := l.get(); len(got) != 1 {
		t.Fatalf("asked nothing more: %v", got)
	}
}
