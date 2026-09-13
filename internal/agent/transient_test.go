package agent

import (
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
)

// Transient notices go to OnTransient when a UI provides it, and fall back
// to the ordinary notice channel (plain mode, tests) when it does not.
func TestTransientNoticeFallsBackToOnNotice(t *testing.T) {
	ag, _ := newTestAgent(t, &funcProvider{}, nil)
	var plain, transient []string
	ag.Events.OnNotice = func(s string) { plain = append(plain, s) }
	ag.transient("waiting %d", 1)
	if len(plain) != 1 || plain[0] != "waiting 1" {
		t.Fatalf("fallback: %v", plain)
	}
	ag.Events.OnTransient = func(s string) { transient = append(transient, s) }
	ag.transient("waiting %d", 2)
	if len(transient) != 1 || len(plain) != 1 {
		t.Fatalf("transient hook: %v / %v", transient, plain)
	}
}

// The stall threshold comes from config (seconds), defaulting to 45 s, and
// the second notice fires at four times the first.
func TestStallThresholdFromConfig(t *testing.T) {
	ag, _ := newTestAgent(t, &funcProvider{}, nil)
	if ag.stallAfter != 45*time.Second {
		t.Fatalf("default stallAfter %s", ag.stallAfter)
	}
	ag, _ = newTestAgent(t, &funcProvider{}, func(c *config.Config) { c.StallNoticeSeconds = 7 })
	if ag.stallAfter != 7*time.Second {
		t.Fatalf("configured stallAfter %s", ag.stallAfter)
	}
	if stallSecondStage(ag.stallAfter) != 28*time.Second {
		t.Fatalf("second stage %s", stallSecondStage(ag.stallAfter))
	}
}
