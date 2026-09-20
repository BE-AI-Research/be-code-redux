package engine

import (
	"fmt"
	"strings"
	"testing"
)

func nudgeStore(t *testing.T, nudge int) *Store {
	t.Helper()
	s, err := OpenAt(t.TempDir(), t.TempDir(), "s1", false,
		Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768, StepNudge: nudge})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func observeN(s *Store, n int) {
	for i := 0; i < n; i++ {
		s.Observe(Event{Tool: "shell", Args: map[string]any{"command": fmt.Sprintf("echo %d", i)}, Content: "ok\n"})
	}
}

// A step that stays open through many tool calls is the shape of a model that
// has taken on too much or is going round in circles. The block says so, in
// the one place that survives a compaction.
func TestALongOpenStepIsNudged(t *testing.T) {
	s := nudgeStore(t, 5)
	id := s.Plan("build the editor", []string{"write picking", "write player"})
	s.SetStatus(id+".1", StatusDoing, "")
	observeN(s, 4)
	if out := s.Render(0, nil); strings.Contains(out, "has been open for") {
		t.Fatalf("nudged below the threshold:\n%s", out)
	}
	observeN(s, 2)
	out := s.Render(0, nil)
	want := "step " + id + ".1 has been open for 6 tool calls: finish it, split it into smaller steps, or note why"
	if !strings.Contains(out, want) {
		t.Fatalf("no nudge %q in:\n%s", want, out)
	}
	// Closing the step ends it; the next one starts from nothing.
	s.SetStatus(id+".1", StatusDone, "")
	s.SetStatus(id+".2", StatusDoing, "")
	if out := s.Render(0, nil); strings.Contains(out, "has been open for") {
		t.Fatalf("the nudge outlived its step:\n%s", out)
	}
}

// The count survives the verbatim buffer's cap: dropped items were calls too.
func TestTheNudgeCountsCallsTheBufferDropped(t *testing.T) {
	s, err := OpenAt(t.TempDir(), t.TempDir(), "s1", false,
		Limits{NotesCap: 4096, ItemCap: 64, NodeCap: 256, StepNudge: 10})
	if err != nil {
		t.Fatal(err)
	}
	id := s.Plan("t", []string{"a"})
	s.SetStatus(id+".1", StatusDoing, "")
	observeN(s, 30)
	if out := s.Render(0, nil); !strings.Contains(out, "has been open for 30 tool calls") {
		t.Fatalf("dropped calls were not counted:\n%s", out)
	}
}

func TestTheNudgeDefaultsToTwentyAndCanBeTurnedOff(t *testing.T) {
	s := nudgeStore(t, 0) // unset: the default
	id := s.Plan("t", []string{"a"})
	s.SetStatus(id+".1", StatusDoing, "")
	observeN(s, 21)
	if out := s.Render(0, nil); !strings.Contains(out, "has been open for 21 tool calls") {
		t.Fatalf("default threshold is not 20:\n%s", out)
	}
	off := nudgeStore(t, -1)
	id = off.Plan("t", []string{"a"})
	off.SetStatus(id+".1", StatusDoing, "")
	observeN(off, 40)
	if out := off.Render(0, nil); strings.Contains(out, "has been open for") {
		t.Fatalf("a negative step_nudge must turn it off:\n%s", out)
	}
}
