package ui

import (
	"context"
	"testing"
)

// Final review I1: in plain mode too, only a person's typed request clears
// the untrusted-web flag.
func TestReplTypedTurnClearsUntrustedWeb(t *testing.T) {
	r := newTestREPL(t)
	r.Agent.Tools.MarkUntrustedWeb()
	capture(t, func() { r.turn(context.Background(), "next thing", true) })
	if r.Agent.Tools.UntrustedWeb() {
		t.Fatal("a typed request kept the untrusted-web flag")
	}
}

func TestReplHandBackTurnKeepsUntrustedWeb(t *testing.T) {
	r := newTestREPL(t)
	r.Agent.Tools.MarkUntrustedWeb()
	r.Agent.EnqueueHarness("sub-agent big finished 3.2 (done): ported it")
	capture(t, func() {
		if !r.startQueuedTurn(context.Background()) {
			t.Error("the hand-back did not start a turn")
		}
	})
	if !r.Agent.Tools.UntrustedWeb() {
		t.Fatal("a hand-back cleared the untrusted-web flag")
	}
}

func TestReplTypedQueueClearsUntrustedWeb(t *testing.T) {
	r := newTestREPL(t)
	r.Agent.Tools.MarkUntrustedWeb()
	r.Agent.Enqueue("typed while busy")
	capture(t, func() { r.startQueuedTurn(context.Background()) })
	if r.Agent.Tools.UntrustedWeb() {
		t.Fatal("a drain of typed lines kept the untrusted-web flag")
	}
}
