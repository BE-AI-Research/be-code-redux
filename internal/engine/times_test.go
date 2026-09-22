package engine

import (
	"strings"
	"testing"
	"time"
)

// fakeClock replaces the package clock for one test and hands back a way to
// move it.
func fakeClock(t *testing.T) func(time.Duration) {
	t.Helper()
	at := time.Date(2026, 9, 20, 14, 0, 0, 0, time.UTC)
	prev := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = prev })
	return func(d time.Duration) { at = at.Add(d) }
}

// A finished step says how long it took and how much work it was, so the
// model — and the person reading the report — can see which steps were small.
func TestAFinishedStepReportsItsTimeAndCalls(t *testing.T) {
	advance := fakeClock(t)
	s := testStore(t)
	id := s.Plan("build the editor", []string{"write picking", "write player"})
	s.SetStatus(id+".1", StatusDoing, "")
	observeN(s, 3)
	advance(18 * time.Minute)
	s.SetStatus(id+".1", StatusDone, "")
	out := s.Render(0, nil)
	if !strings.Contains(out, id+".1. write picking — done (18m, 3 tool calls)") {
		t.Fatalf("no time on the finished step:\n%s", out)
	}
	// A step nobody worked on says nothing about time.
	if strings.Contains(out, "write player — todo (") {
		t.Fatalf("a todo step grew a time:\n%s", out)
	}
}

func TestDoingClockNamesTheCurrentStepAndWhenItStarted(t *testing.T) {
	advance := fakeClock(t)
	s := testStore(t)
	if _, _, ok := s.DoingClock(); ok {
		t.Fatal("nothing is doing yet")
	}
	id := s.Plan("t", []string{"a", "b"})
	s.SetStatus(id+".1", StatusDoing, "")
	started := now()
	advance(5 * time.Minute)
	gotID, gotStart, ok := s.DoingClock()
	if !ok || gotID != id+".1" || !gotStart.Equal(started) {
		t.Fatalf("got %q %v %v", gotID, gotStart, ok)
	}
	// Going back to a step does not restart its clock.
	s.SetStatus(id+".2", StatusDoing, "")
	advance(time.Minute)
	s.SetStatus(id+".1", StatusDoing, "")
	if _, again, _ := s.DoingClock(); !again.Equal(started) {
		t.Fatalf("the clock restarted: %v, want %v", again, started)
	}
}

// The Markdown documents carry no times — they are the user's file — so the
// dotdir half does, and a reopened store still knows how long a step took.
func TestNodeTimesSurviveAReopen(t *testing.T) {
	advance := fakeClock(t)
	dir, root := t.TempDir(), t.TempDir()
	s, err := OpenAt(dir, root, "s1", false, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	id := s.Plan("t", []string{"a", "b"})
	s.SetStatus(id+".1", StatusDoing, "")
	observeN(s, 2)
	advance(7 * time.Minute)
	s.SetStatus(id+".1", StatusDone, "")
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	advance(24 * time.Hour)
	re, err := OpenAt(dir, root, "s2", false, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if out := re.Render(0, nil); !strings.Contains(out, "a — done (7m, 2 tool calls)") {
		t.Fatalf("times lost across a reopen:\n%s", out)
	}
}
