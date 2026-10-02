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

	// In a fired turn that inherits, with a standing grant for it: the
	// deny list still wins without asking, and after an untrusted page a
	// covered command still asks shell_after_web.
	ag.Tools.ShellDeny = []string{"go test ./secret*"}
	ag.prepareFiredRun(mk("f", "in 1m"), false, time.Minute)
	if res := ag.Tools.Dispatch(context.Background(), provider.ToolCall{ID: "d", Name: "shell",
		Arguments: `{"command":"go test ./secret"}`}); !res.IsError || len(l.get()) != 0 {
		t.Fatalf("denied without asking: %+v %v", res, l.get())
	}
	ag.Tools.MarkUntrustedWeb()
	ag.Tools.Approve = l.approver(false)
	if res := ag.Tools.Dispatch(context.Background(), provider.ToolCall{ID: "w", Name: "shell",
		Arguments: `{"command":"go test ./pkg"}`}); !res.IsError {
		t.Fatalf("refused: %+v", res)
	}
	if got := l.get(); len(got) != 1 || !strings.HasPrefix(got[0], "shell_after_web|") {
		t.Fatalf("shell_after_web asked: %v", got)
	}
	ag.Tools.ClearAllowance()
	ag.Tools.ClearUntrustedWeb()
	ag.Tools.ShellDeny = nil
	l.mu.Lock()
	l.log = nil
	l.mu.Unlock()
	ag.Tools.Approve = l.approver(true)

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

// --- fix round 1 ----------------------------------------------------------------

// max_active lowered below what was approved: the silent path pauses past
// the limit exactly as a "yes" would, without asking.
func TestConfirmOnStartOffEnforcesMaxActive(t *testing.T) {
	ag, _, _ := settingsAgent(t, &scriptedProvider{}, func(s *config.SchedulesConfig) { s.ConfirmOnStart = false })
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		seed(t, ag, mk(n, "every 30m"), true)
	}
	ag.Cfg.Schedules.MaxActive = 2
	notes := noteSink(ag)
	var l safeLog
	ag.Tools.Approve = l.approver(true)
	ag.StartSchedules()
	if len(l.get()) != 0 {
		t.Fatalf("nothing asked: %v", l.get())
	}
	active := 0
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		if mustFind(t, ag, n).State == schedule.Active {
			active++
		}
	}
	if active != 2 || strings.Count(notes(), "schedules.max_active") != 3 {
		t.Fatalf("two armed, three paused with the max_active notice: active=%d notes=%s", active, notes())
	}
}

// A switch that brings approved timers into a session already at the
// limit: they are paused, not released.
func TestConfirmOnStartOffHeldTimersRespectMaxActive(t *testing.T) {
	ag, clock, _ := settingsAgent(t, &scriptedProvider{}, func(s *config.SchedulesConfig) {
		s.ConfirmOnStart = false
		s.MaxActive = 1
	})
	seed(t, ag, mk("proj", "every 30m"), true)
	var l safeLog
	ag.Tools.Approve = l.approver(true)
	ag.StartSchedules()
	tm := mk("t", "in 1m")
	seed(t, ag, tm, false)
	ag.SetSession(&store.Session{ID: "s0"})
	ag.SetSession(&store.Session{ID: "s2", Timers: []schedule.Schedule{tm}})
	ag.ConfirmHeldTimers()
	if len(l.get()) != 0 {
		t.Fatalf("nothing asked: %v", l.get())
	}
	if mustFind(t, ag, "t").State != schedule.Paused || mustFind(t, ag, "proj").State != schedule.Active {
		t.Fatalf("the switched-in timer is paused at the limit: %+v", mustFind(t, ag, "t"))
	}
	clock.Advance(2 * time.Minute)
	settle(t, ag)
	if ag.Pending() != 0 {
		t.Fatal("nothing past the limit fires")
	}
}

// Owner's ruling: never auto-approve while inherit_session_approvals is on.
func TestAutoApproveCreateNotWhileInheriting(t *testing.T) {
	ag, _, _ := settingsAgent(t, &scriptedProvider{}, allOn)
	var l safeLog
	ag.Tools.Approve = l.approver(true)
	if _, err := ag.AddSchedule(schedule.Request{Name: "r", When: "every 1h", Instruction: "x",
		Allow: []string{"write: docs"}}, "agent"); err != nil {
		t.Fatal(err)
	}
	if got := l.get(); len(got) != 1 || !strings.HasPrefix(got[0], "schedule|The model wants to schedule") {
		t.Fatalf("a covered request still asks while inheriting: %v", got)
	}
}

// A request with no grants is covered.
func TestAutoApproveCreateNoGrantsIsCovered(t *testing.T) {
	ag, _, _ := settingsAgent(t, &scriptedProvider{}, func(s *config.SchedulesConfig) { s.AutoApproveCreate = true })
	var l safeLog
	ag.Tools.Approve = l.approver(false)
	if _, err := ag.AddSchedule(schedule.Request{Name: "r", When: "every 1h", Instruction: "x"}, "agent"); err != nil || len(l.get()) != 0 {
		t.Fatalf("no grants: added unasked: %v %v", err, l.get())
	}
}

// What was let through silently runs while the prompt about the others is
// still open; what the prompt shows neither runs nor is paused until it is
// answered.
func TestSilentStartDoesNotWaitOnOpenPrompt(t *testing.T) {
	ag, clock, _ := settingsAgent(t, &scriptedProvider{}, func(s *config.SchedulesConfig) { s.ConfirmOnStart = false })
	seed(t, ag, mk("a", "every 30m"), true)
	ag.sched.mu.Lock()
	ag.sched.putLocked(mk("b", "every 30m"), true) // never approved
	ag.sched.mu.Unlock()
	asked := make(chan string, 1)
	reply := make(chan bool)
	ag.Tools.Approve = func(_, detail string) bool { asked <- detail; return <-reply }
	ag.StartSchedulesAsync()
	select {
	case d := <-asked:
		if !strings.Contains(d, "b\n") || strings.Contains(d, "a\n  when") {
			t.Fatalf("asks about b only: %s", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no prompt")
	}
	clock.Advance(31 * time.Minute)
	if items := waitQueued(t, ag, 1); items[0].ScheduleName != "a" {
		t.Fatalf("a runs while the prompt is open: %+v", items)
	}
	settle(t, ag)
	if sc := mustFind(t, ag, "b"); sc.State != schedule.Active || waitingNow(ag, sc.ID) {
		t.Fatalf("b is neither queued nor paused while asked: %+v", sc)
	}
	reply <- true
	deadline := time.Now().Add(2 * time.Second)
	for !approvedNow(ag, "b") {
		if time.Now().After(deadline) {
			t.Fatal("yes approves b")
		}
		time.Sleep(5 * time.Millisecond)
	}
	clock.Advance(time.Minute)
	waitQueued(t, ag, 2)
}

// The real shell tool: an inheriting fired turn after an untrusted page
// still asks shell_after_web, even with a UI whose "a" (AutoApproveShell)
// answers every shell question it may.
func TestInheritingFiredTurnStillAsksShellAfterWeb(t *testing.T) {
	ag, _, _ := settingsAgent(t, &scriptedProvider{}, allOn)
	var actions []string
	ag.Tools.ApproveCtx = func(ctx context.Context, action, detail string) bool {
		actions = append(actions, action)
		// The UIs' AutoApproveShell shortcut, as both approvers apply it.
		return action == "shell" && tools.SessionShortcuts(ctx)
	}
	ag.prepareFiredRun(mk("w", "in 1m"), false, time.Minute)
	defer ag.Tools.ClearAllowance()
	ag.Tools.MarkUntrustedWeb()
	res := ag.Tools.Dispatch(context.Background(), provider.ToolCall{ID: "1", Name: "shell", Arguments: `{"command":"go test ./x"}`})
	if !res.IsError || strings.Join(actions, ",") != "shell_after_web" {
		t.Fatalf("a standing-covered command after a web page asks shell_after_web: %+v %v", res, actions)
	}
}
