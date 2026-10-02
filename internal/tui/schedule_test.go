package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/schedule"
	"github.com/brown-enterprises/be-code/internal/tools"
)

func TestScheduledTurnShownAsSchedule(t *testing.T) {
	s := newTestSession(t)
	var ran string
	s.startTurnHook = func(text string) { ran = text }
	s.ag.EnqueueScheduled("id1", "nightly", "[Scheduled event \"nightly\" — daily 09:00, set by you 2026-09-26]\nrun tests")
	s.mu.Lock()
	s.startQueuedLocked()
	s.mu.Unlock()
	if !strings.Contains(ran, "run tests") {
		t.Fatalf("the scheduled event started a turn: %q", ran)
	}
	es := s.Entries()
	last := es[len(es)-1]
	if last.Kind != entrySchedule || last.Label != "nightly" {
		t.Fatalf("shown as the schedule, not the user: %+v", last)
	}
	st, _ := newStyles("dark")
	out := renderEntry(last, st, 100, false, true)
	if !strings.Contains(out, "⏰ nightly") || !strings.Contains(out, "run tests") || strings.Contains(out, "[Scheduled event") {
		t.Fatalf("rendered: %q", out)
	}
}

func TestPersonsLineRunsBeforeScheduledEvent(t *testing.T) {
	s := newTestSession(t)
	var ran []string
	s.startTurnHook = func(text string) { ran = append(ran, text) }
	s.ag.EnqueueScheduled("id1", "nightly", "scheduled")
	s.ag.EnqueueFrom("typed", 1)
	s.mu.Lock()
	s.startQueuedLocked()
	s.mu.Unlock()
	if len(ran) != 1 || ran[0] != "typed" || s.ag.Pending() != 1 {
		t.Fatalf("the typed line first, alone: %v pending=%d", ran, s.ag.Pending())
	}
}

func TestScheduleAskHasNoAlways(t *testing.T) {
	s, a, _ := twoViews(t)
	beforeCfg, beforeReg := s.cfg.ApproveFileWrites, s.ag.Tools.ApproveWrites
	decided := make(chan bool, 1)
	go func() { decided <- s.approveFromAgent("schedule", "Add this schedule?\n\n- not a diff line") }()
	waitFor(t, func() bool { flush(a); return a.mode == modeAsk })
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	flush(a)
	if a.mode != modeAsk || s.cfg.ApproveFileWrites != beforeCfg || s.ag.Tools.ApproveWrites != beforeReg {
		t.Fatal(`"a" must do nothing on a schedule prompt`)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	select {
	case ok := <-decided:
		if !ok {
			t.Fatal("y approves")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("y never answered")
	}
}

// TestOnScheduleFireDoesNotDeadlockUnderSessionLock is the Task 8 review
// ruling: /schedule run reaches fireNow (and so onScheduleFire) from a
// View's Update, which already holds the session lock. onScheduleFire must
// hop to its own goroutine before taking it, or every attached terminal
// deadlocks the moment a person runs a schedule by hand.
func TestOnScheduleFireDoesNotDeadlockUnderSessionLock(t *testing.T) {
	s := newTestSession(t)
	var ran string
	s.startTurnHook = func(text string) { ran = text }
	s.ag.EnqueueScheduled("id1", "nightly", "[Scheduled event \"nightly\" — daily 09:00, set by you 2026-09-26]\nrun tests")

	s.mu.Lock()
	done := make(chan struct{})
	go func() {
		s.onScheduleFire("nightly") // must return promptly even with s.mu held here
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		s.mu.Unlock()
		t.Fatal("onScheduleFire blocked while the session lock was held")
	}
	s.mu.Unlock()

	// Once the lock is free, onScheduleFire's own goroutine takes it and
	// starts the queued turn (startQueuedLocked is idempotent, same as the
	// existing startTurnHook tests use).
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return ran != ""
	})
	if !strings.Contains(ran, "run tests") {
		t.Fatalf("idle session did not start the queued turn: %q", ran)
	}
}

// TestOnScheduleFireNoOpWhileRunning is the other half of the same ruling:
// a busy session must not start a second turn — finishTurnLocked's own
// leftover-queue drain picks the event up when the run ends instead.
func TestOnScheduleFireNoOpWhileRunning(t *testing.T) {
	s := newTestSession(t)
	var ran string
	s.startTurnHook = func(text string) { ran = text }
	s.ag.EnqueueScheduled("id1", "nightly", "scheduled")
	s.mu.Lock()
	s.running = true
	s.mu.Unlock()

	before := s.fireHandled.Load()
	s.onScheduleFire("nightly")
	waitFor(t, func() bool { return s.fireHandled.Load() > before }) // the callback ran

	s.mu.Lock()
	got, pending := ran, s.ag.Pending()
	s.running = false
	s.mu.Unlock()
	if got != "" || pending == 0 {
		t.Fatalf("a running session must not start a second turn: ran=%q pending=%d", got, pending)
	}
}

func TestBottomLineShowsNextSchedule(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, a, _ := twoViews(t)
	s.ag.EnableSchedules(schedule.NewFakeClock(time.Date(2026, 9, 26, 10, 0, 0, 0, time.Local)))
	// The schedule prompt goes through ApproveCtx (so a withdrawn prompt
	// can be told from a "no"); stand in for both seams.
	s.ag.Tools.Approve = func(string, string) bool { return true }
	s.ag.Tools.ApproveCtx = func(context.Context, string, string) bool { return true }
	if _, err := s.ag.AddSchedule(schedule.Request{Name: "soon", When: "in 5m", Instruction: "x"}, "person"); err != nil {
		t.Fatal(err)
	}
	flush(a)
	a.mu.Lock()
	line := a.bottomLine()
	a.mu.Unlock()
	if !strings.Contains(line, "next: soon 10:05") {
		t.Fatalf("status: %q", line)
	}
}

// TestFiredTurnIgnoresSessionAutoApprovals is final review C1: "a" on a
// shell prompt (AutoApproveShell) and accept-all (ApproveFileWrites off)
// were given by a person who was watching. A fired turn's uncovered action
// must still reach the modal, whatever they say.
func TestFiredTurnIgnoresSessionAutoApprovals(t *testing.T) {
	s, a, _ := twoViews(t)
	s.cfg.AutoApproveShell = true
	s.cfg.ApproveFileWrites = false
	s.ag.Tools.ApproveWrites = false
	s.ag.Tools.SetAllowance(nil, time.Minute)
	defer s.ag.Tools.ClearAllowance()
	for _, c := range []struct{ tool, args string }{
		{"shell", `{"command":"echo hi"}`},
		{"write_file", `{"path":"x.txt","content":"x"}`},
	} {
		done := make(chan tools.Result, 1)
		go func() {
			done <- s.ag.Tools.Dispatch(context.Background(), provider.ToolCall{ID: "1", Name: c.tool, Arguments: c.args})
		}()
		asked := false
		deadline := time.Now().Add(2 * time.Second)
		for !asked && time.Now().Before(deadline) {
			select {
			case res := <-done:
				t.Fatalf("%s ran unasked during a fired turn: %+v", c.tool, res)
			default:
			}
			flush(a)
			asked = a.mode == modeAsk
			time.Sleep(5 * time.Millisecond)
		}
		if !asked {
			t.Fatalf("%s: no prompt", c.tool)
		}
		a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
		flush(a)
		select {
		case res := <-done:
			if !res.IsError {
				t.Fatalf("%s: refused: %+v", c.tool, res)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: never answered", c.tool)
		}
	}
	// Outside a fired turn the shortcut still answers at once.
	if !s.approveFromAgent("shell", "echo hi") {
		t.Fatal("AutoApproveShell still applies to an ordinary question")
	}
}

// TestWithdrawnAskIsNotAnAnswer is final review I2's TUI half: a question
// closed with nobody answering (quit, CancelAsk) is reported as withdrawn,
// an answered "n" is not.
func TestWithdrawnAskIsNotAnAnswer(t *testing.T) {
	s, a, _ := twoViews(t)
	for _, withdraw := range []bool{true, false} {
		ctx, out := tools.WithAskOutcome(context.Background())
		done := make(chan bool, 1)
		go func() { done <- s.approveFromAgentCtx(ctx, "schedule", "These scheduled events will run") }()
		waitFor(t, func() bool { flush(a); return a.mode == modeAsk })
		if withdraw {
			s.mu.Lock()
			gen := s.ask.Gen
			s.mu.Unlock()
			s.CancelAsk(gen, "")
		} else {
			a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
		}
		flush(a)
		select {
		case ok := <-done:
			if ok || out.Withdrawn() != withdraw {
				t.Fatalf("withdraw=%v: ok=%v withdrawn=%v", withdraw, ok, out.Withdrawn())
			}
		case <-time.After(2 * time.Second):
			t.Fatal("never returned")
		}
	}
}

// TestFiredTurnInheritsSessionApprovals: with
// schedules.inherit_session_approvals the session's "a" and accept-all
// answer a fired turn's questions again, with no modal raised.
func TestFiredTurnInheritsSessionApprovals(t *testing.T) {
	s, a, _ := twoViews(t)
	s.cfg.AutoApproveShell = true
	s.cfg.ApproveFileWrites = false
	s.ag.Tools.ApproveWrites = true // reach the approver: it is the shortcut under test
	s.ag.Tools.SetFiredPolicy(nil, time.Minute, true)
	defer s.ag.Tools.ClearAllowance()
	for _, c := range []struct{ tool, args string }{
		{"shell", `{"command":"echo hi"}`},
		{"write_file", `{"path":"x.txt","content":"x"}`},
	} {
		res := s.ag.Tools.Dispatch(context.Background(), provider.ToolCall{ID: "1", Name: c.tool, Arguments: c.args})
		if res.IsError {
			t.Fatalf("%s: the session's shortcut answers an inheriting fired turn: %+v", c.tool, res)
		}
		flush(a)
		if a.mode == modeAsk {
			t.Fatalf("%s raised a modal", c.tool)
		}
	}
}
