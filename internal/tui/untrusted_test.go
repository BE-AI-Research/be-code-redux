package tui

import "testing"

// Final review I1: only a request a person typed clears the untrusted-web
// flag; a sub-agent's hand-back starting the next turn never does.
func TestTypedSubmitClearsUntrustedWeb(t *testing.T) {
	s := newTestSession(t)
	s.startTurnHook = func(string) {}
	s.ag.Tools.MarkUntrustedWeb()
	s.mu.Lock()
	s.Submit("do the next thing", 0)
	s.mu.Unlock()
	if s.ag.Tools.UntrustedWeb() {
		t.Fatal("a typed request kept the untrusted-web flag")
	}
}

func TestHandBackTurnKeepsUntrustedWeb(t *testing.T) {
	s := newTestSession(t)
	var ran string
	s.startTurnHook = func(text string) { ran = text }
	s.ag.Tools.MarkUntrustedWeb()
	s.ag.EnqueueFrom("typed while busy", 1)
	s.ag.EnqueueHarness("sub-agent big finished 3.2 (done): ported it")
	s.mu.Lock()
	s.startQueuedLocked()
	s.mu.Unlock()
	if ran == "" {
		t.Fatal("the queued hand-back did not start a turn")
	}
	if !s.ag.Tools.UntrustedWeb() {
		t.Fatal("a drain holding a hand-back cleared the untrusted-web flag")
	}
}

func TestTypedQueueDrainClearsUntrustedWeb(t *testing.T) {
	s := newTestSession(t)
	s.startTurnHook = func(string) {}
	s.ag.Tools.MarkUntrustedWeb()
	s.ag.EnqueueFrom("typed while busy", 1)
	s.mu.Lock()
	s.startQueuedLocked()
	s.mu.Unlock()
	if s.ag.Tools.UntrustedWeb() {
		t.Fatal("a drain of typed lines kept the untrusted-web flag")
	}
}
