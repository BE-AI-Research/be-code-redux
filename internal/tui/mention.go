package tui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/brown-enterprises/be-code/internal/store"
)

// @agent (spec §6): a room line that names the model becomes a request —
// the line and a few before it — through the same path typed input takes,
// and the turn's final answer is copied back to the room. One at a time;
// later mentions wait in order.

var mentionRe = regexp.MustCompile(`(?i)(^|[^\w.@])@agent\b`)

// IsMention reports whether text addresses the model.
func IsMention(text string) bool { return mentionRe.MatchString(text) }

type mentionItem struct {
	from string
	line store.ChatLine
}

// mentionRequest is the text sent to the model for a mention by from, built
// from the room as it stands (the mention is its last line).
func (s *Session) mentionRequest(from string, at store.ChatLine) string {
	n := s.cfg.Chat.MentionContext
	var ctx []store.ChatLine
	for i := len(s.room) - 1; i >= 0 && len(ctx) < n; i-- {
		l := s.room[i]
		if l.User == "" {
			continue
		}
		ctx = append([]store.ChatLine{l}, ctx...)
	}
	if n <= 0 {
		ctx = []store.ChatLine{at}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Chat mention from %s (room, %s). Recent chat:\n", from, at.TS.Format("15:04"))
	for _, l := range ctx {
		fmt.Fprintf(&b, "  %s %s: %s\n", l.TS.Format("15:04"), l.User, strings.TrimSuffix(l.Text, " (queued)"))
	}
	fmt.Fprintf(&b, "\nReply to %s in the chat.", from)
	return b.String()
}

// noteMentionLocked is called by PostLocked for a person's line that
// mentions the model. Caller holds mu.
func (s *Session) noteMentionLocked(line store.ChatLine) {
	item := mentionItem{from: line.User, line: line}
	if s.mentionActive != nil {
		s.mentionQueue = append(s.mentionQueue, item)
		s.markQueuedLocked(len(s.room)-1, true)
		return
	}
	s.startMentionLocked(item)
}

// queuedSuffix is what a mention waiting its turn shows in the room.
const queuedSuffix = " (queued)"

// chatEditMsg replaces one line of the room in every view: the "(queued)"
// marker coming and going. Views hold a copy of the room, kept by the same
// rule as the session's, so an index means the same line in each — and the
// TS is checked before the swap, in case a cap trim has moved things.
type chatEditMsg struct {
	index int
	line  store.ChatLine
}

// markQueuedLocked adds or removes the marker on the room line at i, in the
// saved room and in every view.
func (s *Session) markQueuedLocked(i int, queued bool) {
	if i < 0 || i >= len(s.room) {
		return
	}
	l := &s.room[i]
	l.Text = strings.TrimSuffix(l.Text, queuedSuffix)
	if queued {
		l.Text += queuedSuffix
	}
	room := s.room
	s.ag.UpdateSession(func(ss *store.Session) { ss.Chat = append([]store.ChatLine(nil), room...) })
	s.broadcast(chatEditMsg{index: i, line: *l})
}

// unmarkQueuedLocked finds item's line in the room and clears its marker.
func (s *Session) unmarkQueuedLocked(item mentionItem) {
	for i := len(s.room) - 1; i >= 0; i-- {
		l := s.room[i]
		if l.TS.Equal(item.line.TS) && l.User == item.line.User && l.Kind == "mention" {
			s.markQueuedLocked(i, false)
			return
		}
	}
}

// startMentionLocked makes item the active mention: builds its request and
// either starts it as a fresh turn (nobody else is running) or, arriving
// mid an ordinary typed turn, enqueues it for delivery inside that turn —
// its answer is then that turn's own final answer, which finishTurnLocked
// already sees. Reports whether it started a turn here and now, so a caller
// chaining several mentions knows not to also start one of its own.
func (s *Session) startMentionLocked(item mentionItem) bool {
	s.mentionActive = &item
	req := s.mentionRequest(item.from, item.line)
	s.broadcast(mentionBusyMsg(item.from))
	if s.running {
		s.ag.EnqueueFrom(req, 0) // lands after the current tool results
		return false
	}
	s.appendEntryLocked(entry{Kind: entryUser, Label: "chat> ", Text: item.from + " mentioned the agent"})
	s.startTurnLocked(req)
	return true
}

// mentionBusyMsg names whose question the model is on ("" when none).
type mentionBusyMsg string

// finishMentionLocked runs from finishTurnLocked with the turn's answer:
// posts it to the room (once, only for a turn a mention actually started —
// mentionActive is nil otherwise, so a plain typed request posts nothing)
// and starts the next queued mention, if any. It reports whether that
// dequeue started a new turn, so finishTurnLocked's own leftover-queue
// restart does not also start one on top of it.
func (s *Session) finishMentionLocked(answer string) bool {
	if s.mentionActive == nil {
		return false
	}
	if answer != "" {
		// The room is a chat, not the transcript: a long reply is cut, with
		// what follows the first line indented so it reads as one message.
		lines := strings.Split(strings.TrimRight(answer, "\n"), "\n")
		if len(lines) > 40 {
			lines = append(lines[:40], "(full reply in the transcript)")
		}
		for i := 1; i < len(lines); i++ {
			lines[i] = "  " + lines[i]
		}
		s.PostLocked("agent", strings.Join(lines, "\n"), "reply")
	}
	s.mentionActive = nil
	s.broadcast(mentionBusyMsg(""))
	if len(s.mentionQueue) > 0 {
		next := s.mentionQueue[0]
		s.mentionQueue = s.mentionQueue[1:]
		s.unmarkQueuedLocked(next)
		return s.startMentionLocked(next)
	}
	return false
}
