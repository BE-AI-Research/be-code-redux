package agent

import "github.com/brown-enterprises/be-code/internal/store"

// UpdateSession runs fn on the current session so a UI can keep its own part
// of the session file (the chat room) beside the transcript the agent saves.
// It holds sessionMu for the pointer and saveMu against autosave, which
// marshals the same fields on the agent goroutine — a UI appending to Chat
// while autosave encoded it was a data race. Nothing happens without a
// session; fn must not call back into the agent.
func (a *Agent) UpdateSession(fn func(s *store.Session)) {
	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()
	if a.Session == nil {
		return
	}
	a.saveMu.Lock()
	defer a.saveMu.Unlock()
	fn(a.Session)
}
