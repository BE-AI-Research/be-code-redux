package agent

import (
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/schedule"
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
