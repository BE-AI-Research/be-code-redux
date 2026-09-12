package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func busyWithQueue(t *testing.T, msgs ...string) *Model {
	t.Helper()
	m := newTestModel(t)
	m.mode = modeBusy
	m.running = true
	m.cancelFn = func() {}
	for _, s := range msgs {
		m.ag.Enqueue(s)
	}
	return m
}

// Up on an empty input during a run opens the queue popup and holds delivery.
func TestUpOnEmptyInputOpensQueue(t *testing.T) {
	m := busyWithQueue(t, "also add tests", "rename output")
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.mode != modeQueue {
		t.Fatalf("mode = %v, want queue", m.mode)
	}
	if !m.ag.Held() {
		t.Fatal("queue not held while the popup is open")
	}
	v := m.View()
	for _, want := range []string{"also add tests", "rename output", "queued messages"} {
		if !strings.Contains(v, want) {
			t.Fatalf("popup lacks %q:\n%s", want, v)
		}
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != modeBusy || m.ag.Held() {
		t.Fatalf("Esc did not close/release: mode=%v held=%v", m.mode, m.ag.Held())
	}
}

// Up with an empty queue does nothing but a hint; Up with text in the input
// edits the text as before.
func TestUpWithoutQueueDoesNotOpen(t *testing.T) {
	m := busyWithQueue(t)
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	if m.mode != modeBusy {
		t.Fatalf("mode = %v", m.mode)
	}
}

// Enter pulls the highlighted message into the input (paused, no longer
// pending); Enter again re-queues it.
func TestEnterEditsAndRequeues(t *testing.T) {
	m := busyWithQueue(t, "also add tests", "rename output")
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m.Update(tea.KeyMsg{Type: tea.KeyDown}) // highlight the second
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modeBusy || m.inputFor(0).Value() != "rename output" {
		t.Fatalf("mode=%v input=%q", m.mode, m.inputFor(0).Value())
	}
	if m.ag.Pending() != 1 || m.ag.Held() {
		t.Fatalf("pending=%d held=%v; the edited message must be out of the queue and the hold released", m.ag.Pending(), m.ag.Held())
	}
	m.inputFor(0).SetValue("rename output to result.txt")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if got := m.ag.Peek(); len(got) != 2 || got[1] != "rename output to result.txt" {
		t.Fatalf("re-queue failed: %v", got)
	}
}

// d drops the highlighted message.
func TestDropRemovesMessage(t *testing.T) {
	m := busyWithQueue(t, "keep me", "drop me")
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	if got := m.ag.Peek(); len(got) != 1 || got[0] != "keep me" {
		t.Fatalf("after drop: %v", got)
	}
	if !strings.Contains(m.transcript.String(), "dropped") {
		t.Fatal("no confirmation line")
	}
	// Dropping the last one closes the popup.
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	if m.mode != modeBusy || m.ag.Held() {
		t.Fatalf("popup not closed after the queue emptied: mode=%v held=%v", m.mode, m.ag.Held())
	}
}

// The bottom line advertises the queue while a run is in progress.
func TestBottomLineShowsQueuedCount(t *testing.T) {
	m := busyWithQueue(t, "one", "two")
	if !strings.Contains(m.View(), "2 queued") {
		t.Fatalf("bottom line lacks the queue count:\n%s", m.View())
	}
}

// If the run finishes while the popup is open, it closes, the hold is
// released, and the leftover queue starts the next turn as usual.
func TestRunFinishingClosesQueuePopup(t *testing.T) {
	m := busyWithQueue(t, "next thing")
	m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m.Update(turnDoneMsg{})
	if m.mode == modeQueue || m.ag.Held() {
		t.Fatalf("popup survived the run: mode=%v held=%v", m.mode, m.ag.Held())
	}
	if !strings.Contains(m.transcript.String(), "next thing") {
		t.Fatal("leftover queue did not start the next turn")
	}
}
