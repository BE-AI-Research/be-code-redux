package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/schedule"
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

func TestBottomLineShowsNextSchedule(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, a, _ := twoViews(t)
	s.ag.EnableSchedules(schedule.NewFakeClock(time.Date(2026, 9, 26, 10, 0, 0, 0, time.Local)))
	s.ag.Tools.Approve = func(string, string) bool { return true }
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
