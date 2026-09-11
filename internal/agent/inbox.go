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

// deliverInbox appends queued messages to the history as user turns,
// tagged so the model knows they arrived mid-task.
func (a *Agent) deliverInbox() {
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
