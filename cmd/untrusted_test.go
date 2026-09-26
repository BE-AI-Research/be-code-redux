package cmd

import (
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
)

// Final review I1: headless, the hand-back rounds after the task are
// requests nobody typed; they never clear the untrusted-web flag.
func TestHeadlessHandBackRoundKeepsUntrustedWeb(t *testing.T) {
	ag, reg := testAgentFor(t, config.Default(), nil, "m")
	reg.MarkUntrustedWeb()
	ag.EnqueueHarness("sub-agent big finished 3.2 (done): ported it")
	req, ok := nextHeadlessRequest(ag)
	if !ok || req == "" {
		t.Fatal("the hand-back was not taken as the next request")
	}
	if !reg.UntrustedWeb() {
		t.Fatal("a hand-back round cleared the untrusted-web flag")
	}
	if _, ok := nextHeadlessRequest(ag); ok {
		t.Fatal("an empty queue made a request")
	}
}
