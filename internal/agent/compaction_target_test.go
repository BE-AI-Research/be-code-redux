package agent

import (
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
)

// Compaction can only shrink the conversation; the system prompt and the
// tools schema are sent whole every time. A target below that fixed floor is
// unreachable, which is what "compacted to 18569 tokens (target 10982)" was:
// a 36 KB system prompt against a target of half the usable context.
func TestTheTargetIsAlwaysReachable(t *testing.T) {
	h := NewHistory(strings.Repeat("x", 36000), 32768)
	h.Reserve = 10922
	h.Extra = 500
	floor := h.MessageTokens(h.System) + h.Extra
	if floor <= h.Limit()/2 {
		t.Fatalf("test setup: floor %d must exceed half the limit %d to reproduce the report", floor, h.Limit())
	}
	if got := h.Target(); got <= floor {
		t.Fatalf("target %d is at or below the fixed floor %d; no compaction can reach it", got, floor)
	}
	if got := h.Target(); got >= h.Limit() {
		t.Fatalf("target %d leaves no runway under the limit %d", got, h.Limit())
	}
}

// With a small fixed prompt the target is what it always was in spirit: the
// floor plus half of what can actually shrink.
func TestTheTargetIsTheFloorPlusHalfTheRoom(t *testing.T) {
	h := NewHistory("short system prompt", 32768)
	h.Reserve = 10922
	floor := h.MessageTokens(h.System) + h.Extra
	want := floor + (h.Limit()-floor)/2
	if got := h.Target(); got != want {
		t.Fatalf("target %d, want floor %d + half the room = %d", got, floor, want)
	}
}

// A fixed prompt that fills the whole limit cannot be compacted away; the
// target degrades to the limit rather than to something below the floor.
func TestAFloorAtTheLimitDoesNotProduceANonsenseTarget(t *testing.T) {
	h := NewHistory(strings.Repeat("x", 200000), 32768)
	h.Reserve = 10922
	if got := h.Target(); got != h.Limit() {
		t.Fatalf("target %d, want the limit %d when the floor already fills it", got, h.Limit())
	}
}

// The repo map follows the window like the working-memory block does. A
// repo_map_budget of 32 KB at a 32k window took 70%% of the usable context
// before the conversation began; configured is a ceiling, not a guarantee.
func TestTheRepoMapBudgetFollowsTheWindow(t *testing.T) {
	ag, _ := newTestAgent(t, nil, nil)
	ag.Cfg.RepoMapBudget = 32586
	ag.ApplyWindow(32768)
	got := ag.repoMapBudget()
	if got >= 32586 {
		t.Fatalf("repo map budget %d was not capped from the window", got)
	}
	if got < repoMapFloor {
		t.Fatalf("repo map budget %d fell below the floor %d", got, repoMapFloor)
	}
	ag.Cfg.RepoMapBudget = 3000
	if got := ag.repoMapBudget(); got != 3000 {
		t.Fatalf("a configured budget under the cap must be honoured, got %d", got)
	}
	ag.ApplyWindow(200000)
	ag.Cfg.RepoMapBudget = 32586
	if got := ag.repoMapBudget(); got != 32586 {
		t.Fatalf("a large window should allow the full configured budget, got %d", got)
	}
}

// The user is told once when the fixed prompt alone takes more than half the
// usable context: compaction will fire often and there is a knob to turn.
func TestAHeavyFixedPromptIsReportedOnce(t *testing.T) {
	ag, _ := newTestAgent(t, nil, nil)
	var notices []string
	ag.Events.OnNotice = func(m string) { notices = append(notices, m) }
	ag.ApplyWindow(8192)
	ag.History.System = provider.Message{Role: provider.RoleSystem, Content: strings.Repeat("x", 30000)}
	ag.warnHeavyPrompt()
	ag.warnHeavyPrompt()
	n := 0
	for _, m := range notices {
		if strings.Contains(m, "fixed prompt") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("want exactly one heavy-prompt notice, got %d: %v", n, notices)
	}
}
