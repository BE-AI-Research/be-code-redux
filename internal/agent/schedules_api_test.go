package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/schedule"
	"github.com/brown-enterprises/be-code/internal/store"
)

func approver(answer bool, log *[]string) func(string, string) bool {
	return func(action, detail string) bool {
		*log = append(*log, action+"|"+detail)
		return answer
	}
}

func TestAddScheduleAsksOnceWithTheWholeSummary(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(true, &log)
	msg, err := ag.AddSchedule(schedule.Request{Name: "nightly", When: "weekdays 09:00",
		Instruction: "run the tests", Task: "3", Allow: []string{"shell: go test ./...", "write: ."}}, "agent")
	if err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || !strings.HasPrefix(log[0], "schedule|") {
		t.Fatalf("one schedule prompt: %v", log)
	}
	for _, want := range []string{"The model wants to schedule", "nightly", "weekdays 09:00", "next:",
		"run the tests", "task 3", "shell: go test ./...", "anywhere in the workspace"} {
		if !strings.Contains(log[0], want) {
			t.Errorf("summary lacks %q:\n%s", want, log[0])
		}
	}
	if !strings.Contains(msg, "nightly") {
		t.Fatalf("msg %q", msg)
	}
	if sc, ok, project := ag.sched.lookup("nightly"); !ok || !project || sc.CreatedBy != "agent" {
		t.Fatalf("recurring goes to schedules.md: %+v %v %v", sc, ok, project)
	}
}

func TestAddScheduleRefusals(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(true, &log)
	cases := []struct {
		req  schedule.Request
		want string
	}{
		{schedule.Request{Name: "Bad Name", When: "in 5m", Instruction: "x"}, "name"},
		{schedule.Request{Name: "a", When: "every 1m", Instruction: "x"}, "more often"},
		{schedule.Request{Name: "a", When: "at 2020-01-01 09:00", Instruction: "x"}, "passed"},
		{schedule.Request{Name: "a", When: "in 5m", Instruction: " "}, "instruction"},
		{schedule.Request{Name: "a", When: "in 5m", Instruction: "x", Allow: []string{"shell: *"}}, "every command"},
	}
	for _, c := range cases {
		if _, err := ag.AddSchedule(c.req, "person"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: err %v, want %q", c.req, err, c.want)
		}
	}
	if len(log) != 0 {
		t.Fatal("an invalid schedule never reaches a prompt")
	}
	ag.AddSchedule(schedule.Request{Name: "dup", When: "in 5m", Instruction: "x"}, "person")
	if _, err := ag.AddSchedule(schedule.Request{Name: "dup", When: "in 9m", Instruction: "y"}, "person"); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestAddScheduleRefusedAndDeclined(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(false, &log)
	if _, err := ag.AddSchedule(schedule.Request{Name: "a", When: "in 5m", Instruction: "x"}, "person"); err == nil {
		t.Fatal("a declined prompt adds nothing")
	}
	if _, ok, _ := ag.sched.lookup("a"); ok {
		t.Fatal("nothing stored")
	}
	ag.Tools.Approve = approver(true, &log)
	ag.Tools.MarkUntrustedWeb()
	if _, err := ag.AddSchedule(schedule.Request{Name: "b", When: "in 5m", Instruction: "x"}, "agent"); err == nil ||
		!strings.Contains(err.Error(), "/schedule add") {
		t.Fatalf("model-created schedules are refused after a web page: %v", err)
	}
	if _, err := ag.AddSchedule(schedule.Request{Name: "c", When: "in 5m", Instruction: "x"}, "person"); err != nil {
		t.Fatalf("a person can still add one: %v", err)
	}
}

func TestMaxActive(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	ag.Cfg.Schedules.MaxActive = 1
	var log []string
	ag.Tools.Approve = approver(true, &log)
	ag.AddSchedule(schedule.Request{Name: "a", When: "in 5m", Instruction: "x"}, "person")
	if _, err := ag.AddSchedule(schedule.Request{Name: "b", When: "in 5m", Instruction: "x"}, "person"); err == nil ||
		!strings.Contains(err.Error(), "max_active") {
		t.Fatalf("err %v", err)
	}
}

func TestModelCannotCancelPersonsScheduleWithoutAsking(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(true, &log)
	ag.AddSchedule(schedule.Request{Name: "mine", When: "daily 09:00", Instruction: "x"}, "person")
	ag.AddSchedule(schedule.Request{Name: "its", When: "daily 10:00", Instruction: "x"}, "agent")
	log = nil
	ag.Tools.Approve = approver(false, &log)
	if _, err := ag.ScheduleAction("cancel", "mine", "agent"); err == nil {
		t.Fatal("declined: the person's schedule stays")
	}
	if len(log) != 1 {
		t.Fatalf("asked the person: %v", log)
	}
	if sc, ok, _ := ag.sched.lookup("mine"); !ok || sc.State != schedule.Active {
		t.Fatalf("declined: the person's schedule is untouched: %+v %v", sc, ok)
	}
	if _, err := ag.ScheduleAction("cancel", "its", "agent"); err != nil {
		t.Fatalf("its own: no prompt needed: %v", err)
	}
	if len(log) != 1 {
		t.Fatal("narrowing its own schedule never asks")
	}
	if _, ok, _ := ag.sched.lookup("its"); ok {
		t.Fatal("cancelled")
	}
}

func TestResumeAsksAndRecordsApproval(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(true, &log)
	ag.AddSchedule(schedule.Request{Name: "n", When: "daily 09:00", Instruction: "x"}, "person")
	ag.ScheduleAction("pause", "n", "person")
	log = nil
	if _, err := ag.ScheduleAction("resume", "n", "person"); err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || !strings.HasPrefix(log[0], "schedule|") {
		t.Fatalf("resume asks: %v", log)
	}
	if sc, _, _ := ag.sched.lookup("n"); sc.State != schedule.Active {
		t.Fatal("active again")
	}
}

func TestRunQueuesNow(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(true, &log)
	ag.AddSchedule(schedule.Request{Name: "n", When: "daily 09:00", Instruction: "x"}, "person")
	if _, err := ag.ScheduleAction("run", "n", "person"); err != nil {
		t.Fatal(err)
	}
	if items := ag.PeekItems(); len(items) != 1 || items[0].ScheduleName != "n" {
		t.Fatalf("queued: %+v", items)
	}
}

func TestStartupGate(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	seed(t, ag, mk("a", "every 30m"), true)
	b := mk("b", "every 30m")
	seed(t, ag, b, true)
	// b is edited by hand: its approval no longer matches.
	ag.sched.mu.Lock()
	b.Instruction = "something else"
	ag.sched.putLocked(b, true)
	ag.sched.mu.Unlock()
	var log []string
	ag.Tools.Approve = approver(false, &log)
	ag.StartSchedules()
	if len(log) != 1 || !strings.Contains(log[0], "changed since approved") {
		t.Fatalf("one startup prompt marking b: %v", log)
	}
	for _, n := range []string{"a", "b"} {
		if sc, _, _ := ag.sched.lookup(n); sc.State != schedule.Paused {
			t.Fatalf("%s paused after no", n)
		}
	}
	clock.Advance(time.Hour)
	time.Sleep(50 * time.Millisecond)
	if ag.Pending() != 0 {
		t.Fatal("nothing fires after no")
	}
}

func TestStartupGateYesApprovesAndFires(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	sc := mk("a", "every 30m")
	ag.sched.mu.Lock()
	ag.sched.reloadLocked()
	ag.sched.putLocked(sc, true) // never approved (hand-written)
	ag.sched.mu.Unlock()
	var log []string
	ag.Tools.Approve = approver(true, &log)
	ag.StartSchedules()
	clock.Advance(31 * time.Minute)
	waitQueued(t, ag, 1)
}

func TestNothingFiresBeforeStart(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	seed(t, ag, mk("a", "in 1m"), false)
	clock.Advance(time.Hour)
	time.Sleep(50 * time.Millisecond)
	if ag.Pending() != 0 {
		t.Fatal("the loop has not started")
	}
}

func TestScheduleLinesAndNext(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(true, &log)
	ag.AddSchedule(schedule.Request{Name: "soon", When: "in 5m", Instruction: "x"}, "agent")
	ag.AddSchedule(schedule.Request{Name: "later", When: "in 50m", Instruction: "x"}, "person")
	lines := strings.Join(ag.ScheduleLines(), "\n")
	for _, want := range []string{"soon", "later", "in 5m", "active", "model", "you"} {
		if !strings.Contains(lines, want) {
			t.Errorf("list lacks %q:\n%s", want, lines)
		}
	}
	if name, at, ok := ag.NextSchedule(); !ok || name != "soon" || !at.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("next %q %v %v", name, at, ok)
	}
}

// TestNextScheduleDoesNotBlockOnSchedulerMutex is the Task 8 review ruling:
// bottomLine calls NextSchedule on every render, and the scheduler's mutex
// is held across schedules.md/session saves — a slow disk write must not
// stall every terminal's render. NextSchedule must read a published
// snapshot instead of taking sched.mu.
func TestNextScheduleDoesNotBlockOnSchedulerMutex(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(true, &log)
	if _, err := ag.AddSchedule(schedule.Request{Name: "soon", When: "in 5m", Instruction: "x"}, "person"); err != nil {
		t.Fatal(err)
	}

	ag.sched.mu.Lock()
	defer ag.sched.mu.Unlock()
	var name string
	var at time.Time
	var ok bool
	done := make(chan struct{})
	go func() {
		name, at, ok = ag.NextSchedule()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("NextSchedule blocked while the scheduler mutex was held")
	}
	if !ok || name != "soon" || !at.Equal(t0.Add(5*time.Minute)) {
		t.Fatalf("next %q %v %v", name, at, ok)
	}
}

func TestRunOnAlreadyQueuedQueuesNothing(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(true, &log)
	ag.AddSchedule(schedule.Request{Name: "n", When: "daily 09:00", Instruction: "x"}, "person")
	if _, err := ag.ScheduleAction("run", "n", "person"); err != nil {
		t.Fatal(err)
	}
	msg, err := ag.ScheduleAction("run", "n", "person")
	if err != nil || msg != "n is already queued" {
		t.Fatalf("second run: %q %v", msg, err)
	}
	if ag.Pending() != 1 {
		t.Fatalf("queued once: %d", ag.Pending())
	}
	// Drained and armed for its turn, but not begun: still counts as queued.
	if got := ag.DrainForTurn(); len(got) != 1 {
		t.Fatalf("drain: %+v", got)
	}
	if msg, _ := ag.ScheduleAction("run", "n", "person"); msg != "n is already queued" {
		t.Fatalf("armed: %q", msg)
	}
	if ag.Pending() != 0 {
		t.Fatal("nothing queued while armed")
	}
}

func TestPauseAndCancelTakeQueuedEventOutOfTheQueue(t *testing.T) {
	for _, action := range []string{"pause", "cancel"} {
		t.Run(action, func(t *testing.T) {
			ag, _, _ := schedAgent(t, &scriptedProvider{})
			var log []string
			ag.Tools.Approve = approver(true, &log)
			ag.AddSchedule(schedule.Request{Name: "n", When: "daily 09:00", Instruction: "x"}, "person")
			ag.Enqueue("typed")
			if _, err := ag.ScheduleAction("run", "n", "person"); err != nil {
				t.Fatal(err)
			}
			if _, err := ag.ScheduleAction(action, "n", "person"); err != nil {
				t.Fatal(err)
			}
			items := ag.PeekItems()
			if len(items) != 1 || items[0].Text != "typed" {
				t.Fatalf("only the typed line is left: %+v", items)
			}
			ag.sched.mu.Lock()
			n := len(ag.sched.pending)
			ag.sched.unlock()
			if n != 0 {
				t.Fatal("no pending entry left behind")
			}
		})
	}
}

func TestModelPausesItsOwnWithoutAskingAndCannotResumeAfterWeb(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(true, &log)
	ag.AddSchedule(schedule.Request{Name: "its", When: "daily 10:00", Instruction: "x"}, "agent")
	log = nil
	if _, err := ag.ScheduleAction("pause", "its", "agent"); err != nil {
		t.Fatal(err)
	}
	if len(log) != 0 {
		t.Fatalf("narrowing its own never asks: %v", log)
	}
	ag.Tools.MarkUntrustedWeb()
	if _, err := ag.ScheduleAction("resume", "its", "agent"); err == nil || !strings.Contains(err.Error(), "/schedule resume") {
		t.Fatalf("refused after a web page: %v", err)
	}
	if len(log) != 0 {
		t.Fatal("refused, not asked")
	}
	if _, err := ag.ScheduleAction("run", "its", "agent"); err == nil {
		t.Fatal("only a person runs a schedule now")
	}
}

// editInstruction rewrites a stored schedule's instruction, as a hand edit
// of schedules.md would. Safe from inside an approver: it takes s.mu, so a
// prompt raised under the lock would deadlock here.
func editInstruction(t *testing.T, ag *Agent, name, text string) {
	t.Helper()
	s := ag.sched
	s.mu.Lock()
	defer s.unlock()
	s.reloadLocked()
	sc, ok, project := s.findLocked(name)
	if !ok {
		t.Fatalf("no %s", name)
	}
	sc.Instruction = text
	s.putLocked(sc, project)
}

func approvedNow(ag *Agent, name string) bool {
	s := ag.sched
	s.mu.Lock()
	defer s.unlock()
	s.reloadLocked()
	sc, _, _ := s.findLocked(name)
	return s.approvedLocked(sc)
}

func TestDoneOneOffDoesNotShadowNewSchedule(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{responses: []provider.ChatResponse{{Content: "done"}}})
	var log []string
	ag.Tools.Approve = approver(true, &log)
	if _, err := ag.AddSchedule(schedule.Request{Name: "x", When: "in 5m", Instruction: "first"}, "person"); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.ScheduleAction("run", "x", "person"); err != nil {
		t.Fatal(err)
	}
	ag.RunFull(context.Background(), ag.DrainForTurn()[0].Text)
	if sc, _, _ := ag.sched.lookup("x"); sc.State != schedule.Done {
		t.Fatalf("ran to done: %+v", sc)
	}
	if _, err := ag.AddSchedule(schedule.Request{Name: "x", When: "in 5m", Instruction: "second"}, "person"); err != nil {
		t.Fatal(err)
	}
	if sc, _, _ := ag.sched.lookup("x"); sc.State != schedule.Active || sc.Instruction != "second" {
		t.Fatalf("the name means the live one: %+v", sc)
	}
	if _, err := ag.ScheduleAction("cancel", "x", "person"); err != nil {
		t.Fatal(err)
	}
	if sc, ok, _ := ag.sched.lookup("x"); ok {
		t.Fatalf("the live one is gone (and the done one was replaced): %+v", sc)
	}
	ag.StartSchedules()
	clock.Advance(10 * time.Minute)
	ag.sched.kickLoop()
	time.Sleep(50 * time.Millisecond)
	if ag.Pending() != 0 {
		t.Fatal("nothing fires")
	}
}

func TestDescribeListsTimesAfterNow(t *testing.T) {
	old := mk("old", "every 30m")
	old.Created = t0.Add(-30 * 24 * time.Hour)
	sp, err := old.Spec()
	if err != nil {
		t.Fatal(err)
	}
	d := describe(old, sp, t0)
	i := strings.Index(d, "next: ")
	if i < 0 {
		t.Fatalf("no next line:\n%s", d)
	}
	first, err := time.ParseInLocation("Mon 2006-01-02 15:04", d[i+6:i+6+len("Mon 2006-01-02 15:04")], time.Local)
	if err != nil {
		t.Fatal(err)
	}
	if !first.After(t0) {
		t.Fatalf("first listed time %v is not after now:\n%s", first, d)
	}
}

func TestResumeRefusedPastMaxActive(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	ag.Cfg.Schedules.MaxActive = 1
	var log []string
	ag.Tools.Approve = approver(true, &log)
	ag.AddSchedule(schedule.Request{Name: "a", When: "daily 09:00", Instruction: "x"}, "person")
	ag.ScheduleAction("pause", "a", "person")
	if _, err := ag.AddSchedule(schedule.Request{Name: "b", When: "daily 10:00", Instruction: "x"}, "person"); err != nil {
		t.Fatal(err)
	}
	log = nil
	if _, err := ag.ScheduleAction("resume", "a", "person"); err == nil || !strings.Contains(err.Error(), "max_active") {
		t.Fatalf("err %v", err)
	}
	if len(log) != 0 {
		t.Fatal("a refusal never asks")
	}
	if sc, _, _ := ag.sched.lookup("a"); sc.State != schedule.Paused {
		t.Fatal("still paused")
	}
}

func TestResumeRefusedTooFrequent(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	sc := mk("f", "every 30m")
	sc.State = schedule.Paused
	seed(t, ag, sc, true)
	sc.When = "every 1m" // a hand edit
	ag.sched.mu.Lock()
	ag.sched.putLocked(sc, true)
	ag.sched.unlock()
	var log []string
	ag.Tools.Approve = approver(true, &log)
	if _, err := ag.ScheduleAction("resume", "f", "person"); err == nil || !strings.Contains(err.Error(), "min_interval") {
		t.Fatalf("err %v", err)
	}
}

func TestStartupYesLeavesTooFrequentPaused(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	notes := noteSink(ag)
	sc := mk("f", "every 30m")
	seed(t, ag, sc, true)
	sc.When = "every 1m" // a hand edit
	ag.sched.mu.Lock()
	ag.sched.putLocked(sc, true)
	ag.sched.unlock()
	var log []string
	ag.Tools.Approve = approver(true, &log)
	ag.StartSchedules()
	if got, _, _ := ag.sched.lookup("f"); got.State != schedule.Paused {
		t.Fatalf("yes does not arm a too-frequent schedule: %+v", got)
	}
	if n := notes(); !strings.Contains(n, `"f"`) || !strings.Contains(n, "min_interval") {
		t.Fatalf("notice names the reason: %q", n)
	}
}

func TestStartupYesPastMaxActivePausesTheRest(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	ag.Cfg.Schedules.MaxActive = 1
	notes := noteSink(ag)
	seed(t, ag, mk("a", "daily 09:00"), true)
	seed(t, ag, mk("b", "daily 10:00"), true)
	var log []string
	ag.Tools.Approve = approver(true, &log)
	ag.StartSchedules()
	a, _, _ := ag.sched.lookup("a")
	b, _, _ := ag.sched.lookup("b")
	if a.State != schedule.Active || b.State != schedule.Paused {
		t.Fatalf("a %s, b %s", a.State, b.State)
	}
	if !strings.Contains(notes(), "max_active") {
		t.Fatal("notice names the reason")
	}
}

func TestResumeChangedWhileAsked(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(true, &log)
	ag.AddSchedule(schedule.Request{Name: "n", When: "daily 09:00", Instruction: "x"}, "person")
	ag.ScheduleAction("pause", "n", "person")
	ag.Tools.Approve = func(string, string) bool {
		editInstruction(t, ag, "n", "something else")
		return true
	}
	if _, err := ag.ScheduleAction("resume", "n", "person"); err == nil ||
		!strings.Contains(err.Error(), "changed while you were asked; nothing was resumed") {
		t.Fatalf("err %v", err)
	}
	if sc, _, _ := ag.sched.lookup("n"); sc.State != schedule.Paused {
		t.Fatal("not resumed")
	}
	if approvedNow(ag, "n") {
		t.Fatal("the edited content was never approved")
	}
}

func TestStartupYesApprovesOnlyWhatWasShown(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	ag.sched.mu.Lock()
	ag.sched.reloadLocked()
	ag.sched.putLocked(mk("a", "daily 09:00"), true)
	ag.sched.putLocked(mk("b", "daily 10:00"), true)
	ag.sched.unlock()
	ag.Tools.Approve = func(string, string) bool {
		editInstruction(t, ag, "b", "something else")
		return true
	}
	ag.StartSchedules()
	if !approvedNow(ag, "a") {
		t.Fatal("a approved")
	}
	if approvedNow(ag, "b") {
		t.Fatal("b's edit was never shown, so never approved")
	}
}

func TestAddRefusedWhenNameTakenWhileAsked(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	ag.Tools.Approve = func(string, string) bool {
		seed(t, ag, mk("x", "daily 09:00"), true)
		return true
	}
	if _, err := ag.AddSchedule(schedule.Request{Name: "x", When: "daily 10:00", Instruction: "y"}, "person"); err == nil ||
		!strings.Contains(err.Error(), "already exists") {
		t.Fatalf("err %v", err)
	}
	if sc, _, _ := ag.sched.lookup("x"); sc.When != "daily 09:00" {
		t.Fatalf("the other one stands: %+v", sc)
	}
}

func TestByMustBePersonOrAgent(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(true, &log)
	if _, err := ag.AddSchedule(schedule.Request{Name: "a", When: "in 5m", Instruction: "x"}, "someone"); err == nil {
		t.Fatal("add refuses an unknown by")
	}
	ag.AddSchedule(schedule.Request{Name: "a", When: "in 5m", Instruction: "x"}, "person")
	if _, err := ag.ScheduleAction("cancel", "a", ""); err == nil {
		t.Fatal("an action refuses an unknown by")
	}
}

func TestStartupGateOnce(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	b := mk("b", "daily 09:00")
	seed(t, ag, b, true)
	b.Instruction = "edited"
	ag.sched.mu.Lock()
	ag.sched.putLocked(b, true)
	ag.sched.unlock()
	var log []string
	ag.Tools.Approve = approver(false, &log)
	ag.StartSchedules()
	ag.StartSchedules()
	if len(log) != 1 {
		t.Fatalf("one startup prompt: %v", log)
	}
	if !strings.Contains(log[0], "b   (changed since approved)\n  when:") {
		t.Fatalf("mark on the name line:\n%s", log[0])
	}
}

func TestResumePassedOneOffSaysSo(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(true, &log)
	ag.AddSchedule(schedule.Request{Name: "o", When: "in 5m", Instruction: "x"}, "person")
	ag.ScheduleAction("pause", "o", "person")
	clock.Advance(10 * time.Minute)
	log = nil
	if _, err := ag.ScheduleAction("resume", "o", "person"); err != nil {
		t.Fatal(err)
	}
	if len(log) != 1 || !strings.Contains(log[0], "its time has passed; it will run as soon as it is resumed") {
		t.Fatalf("%v", log)
	}
}

func TestSwitchedInTimersAreHeldUntilConfirmed(t *testing.T) {
	for _, answer := range []bool{false, true} {
		ag, clock, _ := schedAgent(t, &scriptedProvider{})
		asked := make(chan string, 4)
		reply := make(chan bool)
		ag.Tools.Approve = func(_, detail string) bool { asked <- detail; return <-reply }
		ag.StartSchedules() // nothing active: no prompt, loop running
		tm := mk("t", "in 1m")
		seed(t, ag, tm, false) // approved in an earlier process
		ag.SetSession(&store.Session{ID: "s0"})
		clock.Advance(2 * time.Minute) // due
		ag.SetSession(&store.Session{ID: "s2", Timers: []schedule.Schedule{tm}})
		ag.sched.kickLoop()
		time.Sleep(50 * time.Millisecond)
		select {
		case d := <-asked:
			t.Fatalf("loadTimers never asks: %s", d)
		default:
		}
		if ag.Pending() != 0 {
			t.Fatal("held: nothing queued")
		}
		go ag.ConfirmHeldTimers()
		var detail string
		select {
		case detail = <-asked:
		case <-time.After(2 * time.Second):
			t.Fatal("the switched-in timers were not confirmed")
		}
		if !strings.Contains(detail, "t\n  when:") {
			t.Fatalf("lists the timer: %s", detail)
		}
		ag.sched.kickLoop()
		time.Sleep(50 * time.Millisecond)
		if ag.Pending() != 0 {
			t.Fatal("nothing queued before the answer")
		}
		reply <- answer
		if answer {
			waitQueued(t, ag, 1)
		} else {
			deadline := time.Now().Add(2 * time.Second)
			for {
				if sc, _, _ := ag.sched.lookup("t"); sc.State == schedule.Paused {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("no pauses it")
				}
				time.Sleep(5 * time.Millisecond)
			}
			time.Sleep(50 * time.Millisecond)
			if ag.Pending() != 0 {
				t.Fatal("nothing queued after no")
			}
		}
		select {
		case d := <-asked:
			t.Fatalf("asked once: %s", d)
		default:
		}
		ag.StopSchedules()
	}
}

func TestConfirmHeldTimersNoopWithNothingHeld(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(true, &log)
	ag.StartSchedules()
	ag.ConfirmHeldTimers()
	ag.SetSession(&store.Session{ID: "s2"})
	ag.ConfirmHeldTimers()
	if len(log) != 0 {
		t.Fatalf("nothing held, nothing asked: %v", log)
	}
}

// TestConfirmHeldTimersPanicDoesNotPropagate: both UIs call ConfirmHeldTimers
// on a bare goroutine with no fence of their own, so a panicking approver
// must not take the session host down with it (controller ruling, task 7).
func TestConfirmHeldTimersPanicDoesNotPropagate(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	notes := noteSink(ag)
	ag.Tools.Approve = func(_, _ string) bool { panic("boom") }
	ag.StartSchedules()
	tm := mk("t", "in 1m")
	seed(t, ag, tm, false)
	ag.SetSession(&store.Session{ID: "s0"})
	clock.Advance(2 * time.Minute) // due
	ag.SetSession(&store.Session{ID: "s2", Timers: []schedule.Schedule{tm}})
	ag.sched.kickLoop()
	time.Sleep(50 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		defer close(done)
		ag.ConfirmHeldTimers()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ConfirmHeldTimers hung or the panic escaped")
	}
	if !strings.Contains(notes(), "confirming this session's timers failed") {
		t.Fatalf("expected a notice about the failed confirmation: %q", notes())
	}
	ag.StopSchedules()
}

func TestStaleHoldPromptAppliesToNothing(t *testing.T) {
	ag, clock, _ := schedAgent(t, &scriptedProvider{})
	notes := noteSink(ag)
	asked := make(chan string, 4)
	reply := make(chan bool)
	ag.Tools.Approve = func(_, detail string) bool { asked <- detail; return <-reply }
	ag.StartSchedules()
	tm := mk("t", "in 1m")
	seed(t, ag, tm, false)
	ag.SetSession(&store.Session{ID: "s0"})
	clock.Advance(2 * time.Minute)
	sessA := &store.Session{ID: "a", Timers: []schedule.Schedule{tm}}
	ag.SetSession(sessA) // A: held under generation 1
	done := make(chan struct{})
	go func() { ag.ConfirmHeldTimers(); close(done) }()
	<-asked
	ag.SetSession(&store.Session{ID: "b"})                                  // B
	ag.SetSession(&store.Session{ID: "a", Timers: []schedule.Schedule{tm}}) // A again: generation 2
	reply <- false                                                          // the stale prompt's no
	<-done
	if sc, _, _ := ag.sched.lookup("t"); sc.State != schedule.Active {
		t.Fatalf("a stale answer pauses nothing: %+v", sc)
	}
	if strings.Contains(notes(), "paused") {
		t.Fatalf("no notice for what it did not pause: %q", notes())
	}
	if ag.Pending() != 0 {
		t.Fatal("still held under the newer prompt")
	}
	go func() { ag.ConfirmHeldTimers() }()
	<-asked
	reply <- true
	waitQueued(t, ag, 1)
}

func TestAddNeverTouchesDoneProjectSections(t *testing.T) {
	ag, _, _ := schedAgent(t, &scriptedProvider{})
	var log []string
	ag.Tools.Approve = approver(true, &log)
	old := mk("x", "every 30m")
	old.State = schedule.Done // a hand edit
	seed(t, ag, old, true)
	if _, err := ag.AddSchedule(schedule.Request{Name: "x", When: "in 5m", Instruction: "new"}, "person"); err != nil {
		t.Fatal(err)
	}
	s := ag.sched
	s.mu.Lock()
	s.reloadLocked()
	var inFile []schedule.Schedule
	for _, sc := range s.doc.Schedules() {
		if sc.Name == "x" {
			inFile = append(inFile, sc)
		}
	}
	s.unlock()
	if len(inFile) != 1 || inFile[0].ID != old.ID {
		t.Fatalf("schedules.md keeps its done section: %+v", inFile)
	}
	if sc, _, _ := ag.sched.lookup("x"); sc.Instruction != "new" {
		t.Fatalf("the name means the live one: %+v", sc)
	}
}

func TestBeginRefusesHeldUnlessManual(t *testing.T) {
	for _, manual := range []bool{false, true} {
		calls := 0
		ag, _, _ := schedAgent(t, countingProvider(&calls))
		notes := noteSink(ag)
		tm := mk("t", "in 1m")
		seed(t, ag, tm, false)
		ag.sched.mu.Lock()
		ag.sched.held[tm.ID] = 1
		ag.sched.unlock()
		ag.fireNow(tm, manual)
		ag.RunFull(context.Background(), ag.DrainForTurn()[0].Text)
		if manual {
			if calls == 0 {
				t.Fatal("a person's /schedule run is consent")
			}
			continue
		}
		if calls != 0 || !strings.Contains(notes(), "it is waiting for a person to confirm it") {
			t.Fatalf("held: not run (calls %d) %q", calls, notes())
		}
	}
}
