package ui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

func withSubAgent(t *testing.T) (*REPL, string) {
	t.Helper()
	r := newTestREPL(t)
	r.Cfg.Coworkers = []config.CoworkerConfig{{Name: "big", Provider: "ollama", Model: "m", SubAgent: true, MaxScope: []string{"internal"}}}
	r.Cfg.Providers["ollama"] = config.ProviderConfig{Type: "ollama", BaseURL: "http://localhost:11434"}
	st := testStoreFor(t, r)
	agent.CoworkerFactory = func(context.Context, *config.Config, config.CoworkerConfig) (provider.Provider, int, error) {
		return nullProvider{}, 0, nil
	}
	t.Cleanup(func() { agent.CoworkerFactory = nil; r.Agent.StopAllSubAgents("test") })
	cws, _ := r.Cfg.ValidCoworkers()
	r.Agent.EnableSubAgents(cws, "http://localhost:11434")
	root := st.Plan("port", []string{"port internal/scan"})
	return r, root + ".1"
}

// waitForIdle blocks until the named sub-agent has no dispatched run in
// flight. A valid /task scope on an owned, empty-scope node makes it ready
// at once, and SetScope schedules unconditionally: with the nullProvider
// stub the dispatched scratch agent settles in well under a millisecond
// (one turn, no tool calls), but on its own goroutine — so a test that
// immediately asserts "no open ask" or exercises /task assign right after
// setting scope must wait for that settle rather than race it.
func waitForIdle(t *testing.T, r *REPL, name string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		busy := false
		for _, s := range r.Agent.SubAgentStates() {
			if s.Name == name && s.Node != "" && s.State != "idle" && !strings.HasPrefix(s.State, "waiting") {
				busy = true
			}
		}
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("sub-agent %s never went idle", name)
}

func TestTaskVerbs(t *testing.T) {
	r, id := withSubAgent(t)
	lines, ok := TaskVerb(r.Agent, []string{"assign", id, "big"})
	if !ok || len(lines) != 1 || lines[0] != id+" assigned to big (pinned); set a scope with /task scope" {
		t.Fatalf("assign: %v %q", ok, lines)
	}
	lines, _ = TaskVerb(r.Agent, []string{"scope", id, "cmd"})
	if !strings.Contains(lines[0], "may only own paths under internal") {
		t.Fatalf("max_scope not enforced: %q", lines)
	}
	lines, _ = TaskVerb(r.Agent, []string{"scope", id, "internal/scan,", "internal/scan_test.go"})
	if lines[0] != id+" scope: internal/scan, internal/scan_test.go" {
		t.Fatalf("scope: %q", lines)
	}
	// A valid scope on a ready node dispatches at once (SetScope schedules
	// unconditionally); let the stubbed sub-agent settle before asserting
	// on the ask/assign state below, or this races its own goroutine.
	waitForIdle(t, r, "big")
	lines, _ = TaskVerb(r.Agent, []string{"reply", id, "use", "the", "old", "one"})
	if !strings.Contains(lines[0], "no sub-agent is asking about "+id) {
		t.Fatalf("reply with no ask: %q", lines)
	}
	if _, ok := TaskVerb(r.Agent, []string{"show", id}); ok {
		t.Fatal("show is not a verb TaskVerb handles")
	}
	lines, _ = TaskVerb(r.Agent, []string{"assign"})
	if lines[0] != "usage: /task assign <id> <owner|main>" {
		t.Fatalf("usage: %q", lines)
	}
	lines, _ = TaskVerb(r.Agent, []string{"assign", id, "main"})
	if lines[0] != id+" is the main model's again" {
		t.Fatalf("unassign: %q", lines)
	}
}

func TestAgentLines(t *testing.T) {
	r, id := withSubAgent(t)
	lines := AgentLines(r.Agent, nil)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "big") || !strings.Contains(joined, "ollama/m") || !strings.Contains(joined, "local") || !strings.Contains(joined, "max_scope: internal") || !strings.Contains(joined, "idle") {
		t.Fatalf("cards:\n%s", joined)
	}
	_, _ = TaskVerb(r.Agent, []string{"assign", id, "big"})
	joined = strings.Join(AgentLines(r.Agent, nil), "\n")
	if !strings.Contains(joined, id+"  waiting: no scope") {
		t.Fatalf("waiting row:\n%s", joined)
	}
	lines = AgentLines(r.Agent, []string{"stop", "nobody"})
	if !strings.Contains(lines[0], "nobody is not running anything") {
		t.Fatalf("stop unknown: %q", lines)
	}
	plain := newTestREPL(t)
	if got := AgentLines(plain.Agent, nil); len(got) != 1 || !strings.Contains(got[0], "no sub-agents configured") {
		t.Fatalf("no sub-agents: %q", got)
	}
}
