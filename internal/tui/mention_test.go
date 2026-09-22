package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/store"
)

func TestIsMention(t *testing.T) {
	for _, yes := range []string{"@agent look", "hey @Agent", "(@agent)", "@AGENT?"} {
		if !IsMention(yes) {
			t.Errorf("%q should match", yes)
		}
	}
	for _, no := range []string{"@agents assemble", "mail x@agent.com", "@@agent", "agent", "email@agent"} {
		if IsMention(no) {
			t.Errorf("%q should not match", no)
		}
	}
}

func TestMentionBuildsTheRequestAndStartsATurn(t *testing.T) {
	s := newTestSession(t)
	s.cfg.Chat.MentionContext = 3
	var got string
	s.startTurnHook = func(text string) { got = text }
	s.Post("dave", "an older line that mention_context=3 leaves out", "")
	s.Post("bob", "the picking tests are red again", "")
	s.Post("", "carol joined", "join") // system lines are omitted from the context
	s.Post("alice", "I think it's the max_dist check", "")
	s.Post("alice", "@agent can you look at tests/test_picking.py?", "")
	if !strings.HasPrefix(got, "Chat mention from alice (room, ") {
		t.Fatalf("request:\n%s", got)
	}
	for _, want := range []string{"Recent chat:", "bob: the picking tests are red again", "alice: @agent can you look", "Reply to alice in the chat."} {
		if !strings.Contains(got, want) {
			t.Fatalf("request lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "carol joined") {
		t.Fatalf("system line in the request:\n%s", got)
	}
	if strings.Contains(got, "older line") {
		t.Fatalf("the window is %d lines including the mention, not %d+1:\n%s", 3, 3, got)
	}
	if lines := strings.Count(got, "\n  "); lines != 3 {
		t.Fatalf("expected 3 context lines, got %d:\n%s", lines, got)
	}
	if r := s.Room(); r[len(r)-1].Kind != "mention" {
		t.Fatalf("kind %q", r[len(r)-1].Kind)
	}
}

func TestMentionMidRunIsEnqueuedAndReplyIsPostedOnce(t *testing.T) {
	s := newTestSession(t)
	s.startTurnHook = func(string) {}
	s.mu.Lock()
	s.setRunStateLocked(true, "thinking")
	s.mu.Unlock()
	s.Post("alice", "@agent status?", "")
	if s.ag.Pending() != 1 {
		t.Fatalf("pending %d", s.ag.Pending())
	}
	// The run ends with an answer: it is posted to the room as agent, once.
	s.mu.Lock()
	s.streaming.WriteString("All green.\nDetails:\n" + strings.Repeat("line\n", 50))
	s.finishTurnLocked(nil, nil)
	s.mu.Unlock()
	r := s.Room()
	last := r[len(r)-1]
	if last.User != "agent" || last.Kind != "reply" || !strings.HasPrefix(last.Text, "All green.") {
		t.Fatalf("reply line %+v", last)
	}
	if !strings.Contains(last.Text, "(full reply in the transcript)") || strings.Count(last.Text, "\n") > 41 {
		t.Fatalf("not cut at 40 lines: %d newlines", strings.Count(last.Text, "\n"))
	}
	s.mu.Lock()
	s.streaming.WriteString("unrelated later answer")
	s.finishTurnLocked(nil, nil)
	s.mu.Unlock()
	if r := s.Room(); r[len(r)-1].User == "agent" && strings.Contains(r[len(r)-1].Text, "unrelated") {
		t.Fatal("a turn nobody mentioned was posted to the room")
	}
}

func TestSecondMentionQueuesBehindTheFirst(t *testing.T) {
	s := newTestSession(t)
	starts := 0
	s.startTurnHook = func(text string) { starts++ }
	s.Post("alice", "@agent one", "")
	s.Post("bob", "@agent two", "")
	if starts != 1 || len(s.mentionQueue) != 1 {
		t.Fatalf("starts %d queued %d", starts, len(s.mentionQueue))
	}
	if r := s.Room(); !strings.HasSuffix(r[len(r)-1].Text, "(queued)") {
		t.Fatalf("second mention not marked: %+v", r[len(r)-1])
	}
	s.mu.Lock()
	s.streaming.WriteString("answer one")
	s.finishTurnLocked(nil, nil)
	s.mu.Unlock()
	if starts != 2 || len(s.mentionQueue) != 0 {
		t.Fatalf("second mention did not start: starts %d queued %d", starts, len(s.mentionQueue))
	}
	s.ids[1] = identity{ID: "l"} // so /chat doesn't stop to ask this terminal's name first
	v := s.NewView(1, "l")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	v.slashCommand("/chat")
	if !strings.Contains(v.View(), "agent is working on bob's question") {
		t.Fatalf("footer:\n%s", v.View())
	}
}

// The "(queued)" marker is something people watching the room see: it is
// broadcast to every attached view when a mention queues, and cleared when
// that mention's turn begins.
func TestQueuedMarkerReachesViewsAndClears(t *testing.T) {
	s := newTestSession(t)
	s.startTurnHook = func(string) {}
	v := s.NewView(1, "l")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	s.Post("alice", "@agent one", "")
	s.Post("bob", "@agent two", "")
	drainAll(t, v)
	if last := v.room[len(v.room)-1]; !strings.HasSuffix(last.Text, "(queued)") {
		t.Fatalf("the view never saw the marker: %+v", last)
	}
	s.mu.Lock()
	s.streaming.WriteString("answer one")
	s.finishTurnLocked(nil, nil)
	s.mu.Unlock()
	drainAll(t, v)
	var bobs store.ChatLine
	for _, l := range v.room {
		if l.User == "bob" {
			bobs = l
		}
	}
	if strings.HasSuffix(bobs.Text, "(queued)") {
		t.Fatalf("the marker was not cleared once bob's turn began: %+v", bobs)
	}
	if r := s.Room(); strings.Contains(r[len(r)-1].Text, "(queued)") && r[len(r)-1].User == "bob" {
		t.Fatalf("session room still marked: %+v", r[len(r)-1])
	}
}

// Text somebody typed while a run was in progress is echoed to the transcript
// even when a queued mention, not that text, starts the next turn.
func TestTypedQueueIsEchoedWhenAMentionStartsTheNextTurn(t *testing.T) {
	s := newTestSession(t)
	s.startTurnHook = func(string) {}
	s.Post("alice", "@agent one", "")
	s.Post("bob", "@agent two", "")
	s.ag.EnqueueFrom("also do this", 1)
	s.mu.Lock()
	s.streaming.WriteString("answer one")
	s.finishTurnLocked(nil, nil)
	s.mu.Unlock()
	if !strings.Contains(transcriptText(s), "also do this") {
		t.Fatal("the typed queue lost its transcript echo")
	}
	if s.ag.Pending() != 1 {
		t.Fatalf("the typed text should still be queued for the mention's turn: pending %d", s.ag.Pending())
	}
}

// I1: a mention whose turn fails is answered in the room all the same, and
// the request it enqueued into that run is taken back out of the queue —
// left there it would be drained as ordinary text and answered in the
// transcript alone, with nobody in the room any the wiser.
func TestFailedMentionTurnSaysSoInTheRoomAndStartsTheNext(t *testing.T) {
	s := newTestSession(t)
	var starts []string
	s.startTurnHook = func(text string) { starts = append(starts, text) }
	s.Post("alice", "@agent one", "")
	s.Post("bob", "@agent two", "")
	s.mu.Lock()
	s.finishTurnLocked(nil, errors.New("backend unreachable"))
	s.mu.Unlock()
	if !strings.Contains(roomText(s), "agent could not answer alice: backend unreachable") {
		t.Fatalf("the room was never told:\n%s", roomText(s))
	}
	if len(starts) != 2 || !strings.Contains(starts[1], "@agent two") {
		t.Fatalf("the queued mention did not start: %+v", starts)
	}
	// And the last one in the queue leaves nothing active behind.
	s.mu.Lock()
	s.finishTurnLocked(nil, errors.New("backend unreachable"))
	active := s.mentionActive
	s.mu.Unlock()
	if active != nil {
		t.Fatalf("mentionActive left set: %+v", active)
	}
}

// I1, the mid-run case: the request went into the agent's queue and the run
// was cancelled before it was delivered. It must not survive to be answered
// as ordinary text later.
func TestCancelledMidRunMentionDropsItsQueuedRequest(t *testing.T) {
	s := newTestSession(t)
	var starts []string
	s.startTurnHook = func(text string) { starts = append(starts, text) }
	s.mu.Lock()
	s.setRunStateLocked(true, "thinking")
	s.mu.Unlock()
	s.Post("alice", "@agent status?", "")
	if s.ag.Pending() != 1 {
		t.Fatalf("setup: pending %d", s.ag.Pending())
	}
	s.mu.Lock()
	s.finishTurnLocked(nil, context.Canceled)
	s.mu.Unlock()
	if s.ag.Pending() != 0 {
		t.Fatalf("the mention's request is still queued: %+v", s.ag.PeekItems())
	}
	for _, text := range starts {
		if strings.Contains(text, "Chat mention from alice") {
			t.Fatalf("the cancelled request was run as ordinary text: %q", text)
		}
	}
	if !strings.Contains(roomText(s), "agent could not answer alice: cancelled") {
		t.Fatalf("the room was never told:\n%s", roomText(s))
	}
}

// roomText is the session's room as a terminal would read it, one line per
// room line, for a test that cares what was said rather than by whom.
func roomText(s *Session) string {
	var b strings.Builder
	for _, l := range s.Room() {
		b.WriteString(l.User)
		b.WriteString(" ")
		b.WriteString(l.Text)
		b.WriteString("\n")
	}
	return b.String()
}
