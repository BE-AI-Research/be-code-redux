package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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
