package ui

import (
	"context"
	"fmt"
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

// withAssignedSubAgent is withSubAgent plus a step already owned and scoped
// directly on the store — the shape a previous session's work is in when
// this one opens, before StartSubAgents has run at all. Going through
// AssignOwner/SetScope instead would dispatch it immediately, which is
// exactly what these tests must not have happen yet.
func withAssignedSubAgent(t *testing.T) (*REPL, string) {
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
	id := root + ".1"
	if err := st.SetOwner(id, "big", false); err != nil {
		t.Fatal(err)
	}
	if err := st.SetScope(id, []string{"internal/scan"}); err != nil {
		t.Fatal(err)
	}
	return r, id
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

// TestAgentsStartReportsNothingWaiting: /agents start with nothing declined
// and nothing assigned says so rather than a bare empty listing.
func TestAgentsStartReportsNothingWaiting(t *testing.T) {
	r, _ := withSubAgent(t)
	lines := AgentLines(r.Agent, []string{"start"})
	if len(lines) != 1 || lines[0] != "no sub-agent work is waiting" {
		t.Fatalf("start with nothing waiting: %v", lines)
	}
}

// TestAgentsStartDispatchesAfterADecline: a decline at startup leaves the
// step dormant; /agents start (AgentLines "start") dispatches it and
// reports how many.
func TestAgentsStartDispatchesAfterADecline(t *testing.T) {
	r, id := withAssignedSubAgent(t)
	r.Agent.Tools.Approve = func(string, string) bool { return false }
	r.Agent.StartSubAgents()
	lines := AgentLines(r.Agent, []string{"start"})
	if len(lines) != 1 || !strings.Contains(lines[0], "started 1 sub-agent step") || !strings.Contains(lines[0], id) {
		t.Fatalf("start after decline: %v", lines)
	}
	// Let the dispatched scratch agent settle before the test's own cleanup
	// clears agent.CoworkerFactory — the same race waitForIdle's own comment
	// describes for a plain SetScope dispatch.
	waitForIdle(t, r, "big")
}

// TestIsAgentsBlockingAgreesWithAgentLines: the TUI decides whether to run
// an /agents verb off its Update goroutine by asking IsAgentsBlocking
// (ScheduleSubAgents, which "start" reaches, can raise a notice that takes
// the session lock — see taskVerbCmd's comment). Unlike a first pass at
// this test, it does not restate IsAgentsBlocking's own condition and
// compare it to itself — that passes for any internally consistent but
// wrong predicate. It cross-checks against AgentLines' real dispatching
// behaviour instead, the way TestIsTaskVerbAgreesWithTaskVerb cross-checks
// against TaskVerb's own `handled`: for each form, leave one declined,
// dormant step behind (withAssignedSubAgent plus a decline), run
// AgentLines with that form, and observe whether it actually dispatched.
func TestIsAgentsBlockingAgreesWithAgentLines(t *testing.T) {
	forms := [][]string{nil, {}, {"stop", "big"}, {"start"}, {"start", "extra"}, {"bogus"}}
	for _, args := range forms {
		args := args
		t.Run(fmt.Sprintf("%v", args), func(t *testing.T) {
			r, _ := withAssignedSubAgent(t)
			r.Agent.Tools.Approve = func(string, string) bool { return false }
			r.Agent.StartSubAgents() // declines; the step stays assigned, todo and dormant
			AgentLines(r.Agent, args)
			dispatched := len(r.Agent.RunningSubAgents()) > 0
			if dispatched {
				waitForIdle(t, r, "big")
			}
			if got := IsAgentsBlocking(args); got != dispatched {
				t.Fatalf("%q: IsAgentsBlocking=%v but AgentLines actually dispatched=%v", args, got, dispatched)
			}
		})
	}
}

// TestIsTaskVerbAgreesWithTaskVerb: the TUI decides whether to run a /task
// verb off its Update goroutine by asking IsTaskVerb, and then never asks
// TaskVerb's own `handled`. A verb in one and not the other would either
// deadlock the TUI again (handled here, run inline) or fall through to the
// plain /task listing (claimed here, ignored there).
func TestIsTaskVerbAgreesWithTaskVerb(t *testing.T) {
	// Forms that reach the verbs with too few arguments still answer with a
	// usage line, so TaskVerb handles them; anything else must not be.
	// One argument reaches each switch case and returns its usage line
	// without touching the agent, so every case of both switches is
	// compared without needing a live one.
	for _, args := range [][]string{
		nil, {}, {"assign"}, {"scope"}, {"reply"},
		{"show"}, {"open"}, {"clear"}, {"nonsense"},
	} {
		_, handled := TaskVerb(nil, args)
		if got := IsTaskVerb(args); got != handled {
			t.Fatalf("%q: IsTaskVerb=%v but TaskVerb handled=%v", args, got, handled)
		}
	}
	// And the full forms the TUI actually routes are claimed.
	for _, args := range [][]string{
		{"assign", "1.1", "big"}, {"scope", "1.1", "a"}, {"reply", "1.1", "hello"},
	} {
		if !IsTaskVerb(args) {
			t.Fatalf("%q would be run inline on the Update goroutine", args)
		}
	}
}
