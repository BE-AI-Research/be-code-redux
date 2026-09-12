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
	items []string
	held  bool // the UI has the queue open for editing: delivery pauses
}

// Enqueue queues a user message for delivery at the next model call.
func (a *Agent) Enqueue(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	a.inbox.mu.Lock()
	a.inbox.items = append(a.inbox.items, text)
	a.inbox.mu.Unlock()
}

// Pending reports how many messages are queued.
func (a *Agent) Pending() int {
	a.inbox.mu.Lock()
	defer a.inbox.mu.Unlock()
	return len(a.inbox.items)
}

// DrainInbox removes and returns every queued message.
func (a *Agent) DrainInbox() []string {
	a.inbox.mu.Lock()
	defer a.inbox.mu.Unlock()
	out := a.inbox.items
	a.inbox.items = nil
	return out
}

// Peek returns a copy of the queued messages in delivery order.
func (a *Agent) Peek() []string {
	a.inbox.mu.Lock()
	defer a.inbox.mu.Unlock()
	return append([]string(nil), a.inbox.items...)
}

// Remove takes the i-th queued message out of the queue. ok is false when
// that message is gone (already delivered or dropped).
func (a *Agent) Remove(i int) (string, bool) {
	a.inbox.mu.Lock()
	defer a.inbox.mu.Unlock()
	if i < 0 || i >= len(a.inbox.items) {
		return "", false
	}
	text := a.inbox.items[i]
	a.inbox.items = append(a.inbox.items[:i], a.inbox.items[i+1:]...)
	return text, true
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
