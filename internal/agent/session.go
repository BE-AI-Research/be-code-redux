package agent

import "github.com/brown-enterprises/be-code/internal/store"

// UpdateSession runs fn on the current session under the session lock, so a
// UI can keep its own part of the session file (the chat room) beside the
// transcript the agent saves. Nothing happens without a session.
func (a *Agent) UpdateSession(fn func(s *store.Session)) {
	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()
	if a.Session != nil {
		fn(a.Session)
	}
}
