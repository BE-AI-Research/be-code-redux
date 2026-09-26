package agent

import (
	"fmt"
	"strings"
	"sync"

	"github.com/brown-enterprises/be-code/internal/provider"
)

// Inbox is the mid-task message queue. The UI enqueues whatever the user
// types while a run is in progress; the loop delivers it to the model
// before its next call, after the tool results that call was waiting on,
// so the user can steer, add requirements or ask questions without
// cancelling the work. Anything still queued when the run ends is left for
// the UI to start the next turn with.
type Inbox struct {
	mu    sync.Mutex
	items []InboxItem
	held  bool // the UI has the queue open for editing: delivery pauses
}

// InboxItem is a queued message and the client that queued it (0 = the
// local terminal). A shared session has one input line per attached
// terminal, so the UI needs to know whose message each one is to show each
// client only its own queue.
type InboxItem struct {
	Text string
	From int
	// Harness is true for a line nobody typed — a sub-agent's hand-back or
	// question. A drain holding one is not a person's request, so it never
	// clears the untrusted-web flag (browser spec §3.6).
	Harness bool
	// Scheduled is the ID of the schedule that queued this line (schedules
	// spec §2.1); empty for everything else. Such a line is never delivered
	// into a running turn and always runs as a turn of its own.
	Scheduled    string
	ScheduleName string
}

// Enqueue queues a user message typed at the local terminal.
func (a *Agent) Enqueue(text string) { a.EnqueueFrom(text, 0) }

// EnqueueHarness queues a line the harness wrote rather than a person — a
// sub-agent's hand-back or question — marked so a drain can tell.
func (a *Agent) EnqueueHarness(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	a.inbox.mu.Lock()
	a.inbox.items = append(a.inbox.items, InboxItem{Text: text, Harness: true})
	a.inbox.mu.Unlock()
}

// TypedByPerson reports whether every item of a drain was typed by a
// person: false for an empty drain and for any drain holding a harness line.
func TypedByPerson(items []InboxItem) bool {
	if len(items) == 0 {
		return false
	}
	for _, it := range items {
		if it.Harness {
			return false
		}
	}
	return true
}

// BeginTypedRequest is called where a request a person typed begins — the
// UIs' submit paths, headless run's own task, an approved plan — and only
// there. It ends the shell suspension an untrusted page started (browser
// spec §3.6): a hand-back re-entering RunFull while the page is still in
// history must not.
func (a *Agent) BeginTypedRequest() { a.Tools.ClearUntrustedWeb() }

// EnqueueFrom queues a user message for delivery at the next model call,
// remembering which attached terminal typed it.
func (a *Agent) EnqueueFrom(text string, from int) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	a.inbox.mu.Lock()
	a.inbox.items = append(a.inbox.items, InboxItem{Text: text, From: from})
	a.inbox.mu.Unlock()
}

// Items returns a copy of the queued messages, with their senders, in
// delivery order.
func (a *Agent) Items() []InboxItem {
	a.inbox.mu.Lock()
	defer a.inbox.mu.Unlock()
	return append([]InboxItem(nil), a.inbox.items...)
}

// Pending reports how many messages are queued.
func (a *Agent) Pending() int {
	a.inbox.mu.Lock()
	defer a.inbox.mu.Unlock()
	return len(a.inbox.items)
}

// DrainInbox removes and returns every queued message except scheduled
// events (DrainItems) — the mid-run delivery path.
func (a *Agent) DrainInbox() []string { return texts(a.DrainItems()) }

// DrainItems removes and returns every queued message, with its sender,
// except scheduled events, which wait for a turn of their own
// (DrainForTurn).
func (a *Agent) DrainItems() []InboxItem {
	a.inbox.mu.Lock()
	defer a.inbox.mu.Unlock()
	var out, keep []InboxItem
	for _, it := range a.inbox.items {
		if it.Scheduled != "" {
			keep = append(keep, it)
		} else {
			out = append(out, it)
		}
	}
	a.inbox.items = keep
	return out
}

// DrainForTurn is the leftover-queue rule's drain: every line that is not a
// scheduled event, as before — or, when there are none, exactly one
// scheduled event, armed so that the RunFull it starts runs under that
// schedule's allowance. A person's words and a scheduled event never share
// a turn, so exactly one set of rules governs each (schedules spec §2.1).
func (a *Agent) DrainForTurn() []InboxItem {
	if out := a.DrainItems(); len(out) > 0 {
		return out
	}
	a.inbox.mu.Lock()
	if len(a.inbox.items) == 0 {
		a.inbox.mu.Unlock()
		return nil
	}
	it := a.inbox.items[0]
	a.inbox.items = a.inbox.items[1:]
	a.inbox.mu.Unlock()
	a.fireMu.Lock()
	a.armed = &firing{id: it.Scheduled, text: it.Text}
	a.fireMu.Unlock()
	return []InboxItem{it}
}

// EnqueueScheduled queues a due event (harness-written: never a person's).
// Exported for the UIs' tests; production code reaches it through fireNow.
func (a *Agent) EnqueueScheduled(id, name, text string) {
	a.inbox.mu.Lock()
	a.inbox.items = append(a.inbox.items, InboxItem{Text: text, Harness: true, Scheduled: id, ScheduleName: name})
	a.inbox.mu.Unlock()
}

// scheduledQueued is the set of schedule IDs with an event still waiting.
func (a *Agent) scheduledQueued() map[string]bool {
	a.inbox.mu.Lock()
	defer a.inbox.mu.Unlock()
	m := map[string]bool{}
	for _, it := range a.inbox.items {
		if it.Scheduled != "" {
			m[it.Scheduled] = true
		}
	}
	return m
}

type firing struct{ id, text string }

// takeFiring spends the arm DrainForTurn set, returning it only when this
// request is the text it armed. A stale arm never leaks into a later request.
func (a *Agent) takeFiring(input string) *firing {
	a.fireMu.Lock()
	defer a.fireMu.Unlock()
	f := a.armed
	a.armed = nil
	if f == nil || strings.TrimSpace(f.text) != strings.TrimSpace(input) {
		return nil
	}
	return f
}

// Peek returns a copy of the queued messages in delivery order.
// PeekItems is the queue as it stands, with each message's sender, left in
// place: for a UI that must echo what is queued without taking it.
func (a *Agent) PeekItems() []InboxItem {
	a.inbox.mu.Lock()
	defer a.inbox.mu.Unlock()
	return append([]InboxItem(nil), a.inbox.items...)
}

func (a *Agent) Peek() []string {
	a.inbox.mu.Lock()
	defer a.inbox.mu.Unlock()
	return texts(a.inbox.items)
}

// texts projects queued items onto their message texts, in order.
func texts(items []InboxItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		out = append(out, it.Text)
	}
	return out
}

// Remove takes the i-th queued message out of the queue. ok is false when
// that message is gone (already delivered or dropped).
func (a *Agent) Remove(i int) (string, bool) {
	a.inbox.mu.Lock()
	defer a.inbox.mu.Unlock()
	if i < 0 || i >= len(a.inbox.items) {
		return "", false
	}
	text := a.inbox.items[i].Text
	a.inbox.items = append(a.inbox.items[:i], a.inbox.items[i+1:]...)
	return text, true
}

// RemoveWhere takes every queued message match reports out of the queue and
// returns how many went. It exists for a request the UI enqueued and then
// had to take back — an @agent mention whose run was cancelled before the
// model ever saw it, which left in the queue would be delivered later as
// ordinary text and answered where nobody who asked is looking.
func (a *Agent) RemoveWhere(match func(InboxItem) bool) int {
	if match == nil {
		return 0
	}
	a.inbox.mu.Lock()
	defer a.inbox.mu.Unlock()
	kept := a.inbox.items[:0]
	removed := 0
	for _, it := range a.inbox.items {
		if match(it) {
			removed++
			continue
		}
		kept = append(kept, it)
	}
	a.inbox.items = kept
	return removed
}

// Hold pauses (true) or resumes (false) delivery, so a queue the user is
// editing does not shift under them.
func (a *Agent) Hold(on bool) {
	a.inbox.mu.Lock()
	a.inbox.held = on
	a.inbox.mu.Unlock()
}

// Held reports whether delivery is paused.
func (a *Agent) Held() bool {
	a.inbox.mu.Lock()
	defer a.inbox.mu.Unlock()
	return a.inbox.held
}

// deliverInbox appends queued messages to the history as user turns,
// tagged so the model knows they arrived mid-task.
func (a *Agent) deliverInbox() {
	if a.Held() {
		return
	}
	msgs := a.DrainInbox()
	for _, m := range msgs {
		a.History.Add(provider.Message{Role: provider.RoleUser,
			Content: "[Message from the user while you were working — take it into account and continue]\n" + m})
		a.notice("delivered queued message: %s", firstLine(m, 80))
	}
}

func firstLine(s string, max int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i] + "…"
	}
	if len(s) > max {
		s = s[:max] + "…"
	}
	return fmt.Sprint(s)
}
