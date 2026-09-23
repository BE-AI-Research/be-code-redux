package tui

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/subagent"
)

func TestSubAgentEventsReachEveryTerminal(t *testing.T) {
	s, a, b := twoViews(t)
	s.onSubAgentStart(subagent.Dispatch{Node: "3.2", Owner: "big", Text: "port internal/scan"})
	s.onSubAgentAsk(subagent.Ask{Node: "3.2", Owner: "big", Question: "which tokenizer?"})
	s.onSubAgentEnd(subagent.HandBack{Node: "3.2", Owner: "big", Status: "done", Summary: "ported it",
		Files: []string{"internal/scan/token.go"}, Elapsed: 14 * time.Minute, Calls: 22})
	s.onSubAgentEnd(subagent.HandBack{Node: "3.3", Owner: "big", Status: "blocked", Reason: "turn cap of 40 reached"})
	for _, v := range []*View{a, b} {
		flush(v)
		out := v.View()
		for _, want := range []string{"big started 3.2 port internal/scan", "big (3.2)? which tokenizer?", "big (3.2)> ", "ported it",
			"done in 14m0s, 22 tool calls; wrote internal/scan/token.go", "big (3.3) blocked: turn cap of 40 reached"} {
			if !strings.Contains(out, want) {
				t.Fatalf("terminal lacks %q:\n%s", want, out)
			}
		}
	}
}

func TestBottomLineShowsRunningSubAgents(t *testing.T) {
	s, a, _ := twoViews(t)
	s.runningSubs = func() []subAgentGlyph { return []subAgentGlyph{{Name: "big", At: "3.2.2"}} }
	if line := a.bottomLine(); !strings.Contains(line, "⚙ big 3.2.2") {
		t.Fatalf("bottom line: %q", line)
	}
	s.runningSubs = func() []subAgentGlyph {
		return []subAgentGlyph{{Name: "big", At: "3.2.2"}, {Name: "claude", At: "3.3"}}
	}
	if line := a.bottomLine(); !strings.Contains(line, "⚙ 2 lanes") {
		t.Fatalf("bottom line: %q", line)
	}
}

// TestBottomLineUnchangedWithNoSubAgent pins the global constraint: a
// session with no sub-agent configured shows no ⚙ element, in either
// layout. twoViews's session has no coworkers at all, so runningSubs (set
// in NewSession from the real ag.RunningSubAgents, not the test seam above)
// must report none.
func TestBottomLineUnchangedWithNoSubAgent(t *testing.T) {
	s, a, _ := twoViews(t)
	if s.ag.SubAgentsEnabled() {
		t.Fatal("test session must start with no sub-agents configured")
	}
	if line := a.bottomLine(); strings.Contains(line, "⚙") {
		t.Fatalf("bottom line shows a sub-agent glyph with none configured: %q", line)
	}
	a.Update(tea.WindowSizeMsg{Width: 60, Height: 18})
	if !a.compact() {
		t.Fatal("expected compact layout at 60x18")
	}
	if line := a.compactBottomLine(); strings.Contains(line, "⚙") {
		t.Fatalf("compact bottom line shows a sub-agent glyph with none configured: %q", line)
	}
}

// scriptedSubProvider is a provider.Provider whose replies are scripted in
// order, tool calls included — cowork_test's scriptedCoworker only ever
// answers with Content, but a sub-agent needs to call ask_main. Guarded by
// its own mutex: the scratch agent's goroutine and StopAllSubAgents' wait
// touch it from outside the test goroutine.
type scriptedSubProvider struct {
	nullProvider
	mu        sync.Mutex
	responses []provider.ChatResponse
	i         int
}

func (s *scriptedSubProvider) Chat(_ context.Context, req provider.ChatRequest, onDelta provider.StreamFunc) (*provider.ChatResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.i >= len(s.responses) {
		return &provider.ChatResponse{Content: "done"}, nil
	}
	r := s.responses[s.i]
	s.i++
	return &r, nil
}

// TestSubAgentReplyNoteReachesEveryTerminal exercises the real path Task 8
// added to view.go's /task case: a sub-agent genuinely parked on ask_main,
// answered with /task reply from one terminal, and the confirmation note
// ("reply to <id>: <text>") must land on every attached terminal — it is a
// shared broadcast, like the ask and hand-back events themselves, not a
// local echo to the terminal that typed it.
func TestSubAgentReplyNoteReachesEveryTerminal(t *testing.T) {
	s, a, b := twoViews(t)
	s.cfg.Coworkers = []config.CoworkerConfig{{Name: "big", Provider: "ollama", Model: "m", SubAgent: true}}
	s.cfg.Providers["ollama"] = config.ProviderConfig{Type: "ollama", BaseURL: "http://sub:11434"}
	s.ag = agent.New(s.cfg, nullProvider{}, "m", s.ag.Tools, "")
	wireEvents(s) // re-point Events/Approve at the session for the new agent

	st, err := engine.OpenAt(filepath.Join(t.TempDir(), "e"), s.ag.Tools.Root, "sess", false, engine.Limits{NotesCap: 4096})
	if err != nil {
		t.Fatal(err)
	}
	s.ag.SetEngine(st)

	agent.CoworkerFactory = func(context.Context, *config.Config, config.CoworkerConfig) (provider.Provider, int, error) {
		return &scriptedSubProvider{responses: []provider.ChatResponse{
			{ToolCalls: []provider.ToolCall{{ID: "c1", Name: "ask_main", Arguments: `{"question":"which tokenizer?"}`}}},
		}}, 0, nil
	}
	t.Cleanup(func() { agent.CoworkerFactory = nil; s.ag.StopAllSubAgents("test over") })
	cws, _ := s.cfg.ValidCoworkers()
	// EnableSubAgents before Plan/SetOwner/SetScope: SetOwner checks the
	// card's SubAgent bit, which only exists once EnableSubAgents has
	// written the cards to the store.
	s.ag.EnableSubAgents(cws, "http://primary:11434")

	root := st.Plan("port", []string{"port internal/scan"})
	id := root + ".1"
	if err := st.SetOwner(id, "big", false); err != nil {
		t.Fatal(err)
	}
	if err := st.SetScope(id, []string{"internal/scan"}); err != nil {
		t.Fatal(err)
	}
	s.ag.ScheduleSubAgents()

	waitFor(t, func() bool { flush(a, b); return strings.Contains(a.wrapped, "which tokenizer?") })
	if !strings.Contains(b.wrapped, "which tokenizer?") {
		t.Fatalf("the ask did not reach the second terminal:\n%s", b.wrapped)
	}

	// slashCommand is always reached through Update, which holds the
	// session's own mutex for its whole body (see view.go); calling it
	// directly here — the only way a test can drive it without a real
	// keystroke — must take the same lock itself, or its appendEntryLocked
	// races the sub-agent goroutine's own onSubAgentAsk/onSubAgentEnd
	// handlers, which lock correctly.
	s.mu.Lock()
	a.slashCommand("/task reply " + id + " use the old one")
	s.mu.Unlock()
	flush(a, b)

	for _, v := range []*View{a, b} {
		if !strings.Contains(v.wrapped, "reply to "+id+": use the old one") {
			t.Fatalf("terminal %d lacks the reply note:\n%s", v.id, v.wrapped)
		}
	}
}

// TestAHandBackToAnIdleMainModelStartsATurn is spec §2.6: "if the main
// model is idle the existing leftover-queue rule starts a turn with it".
// That rule only fires at the end of a run, and a sub-agent working a long
// step almost always hands back after the main model's turn has ended — so
// the hand-back sat in the queue until the operator typed something, and
// the work was never verified, the parent never closed.
func TestAHandBackToAnIdleMainModelStartsATurn(t *testing.T) {
	s, a, _ := twoViews(t)
	var started []string
	s.startTurnHook = func(text string) { started = append(started, text) }

	// The runner enqueues before it fires the event, exactly as
	// agent.runSub does.
	s.ag.Enqueue("sub-agent big finished 3.2 (done, 14m, 22 tool calls): ported it")
	s.onSubAgentEnd(subagent.HandBack{Node: "3.2", Owner: "big", Status: "done", Summary: "ported it"})
	if len(started) != 1 || !strings.Contains(started[0], "sub-agent big finished 3.2") {
		t.Fatalf("no turn was started for the hand-back: %q", started)
	}
	if s.ag.Pending() != 0 {
		t.Fatal("the queue was not drained into the turn")
	}
	flush(a)
	if !strings.Contains(a.View(), "sub-agent big finished 3.2") {
		t.Fatalf("the request was not echoed:\n%s", a.View())
	}

	// A question the sub-agent is parked on starts one too — nothing else
	// will, and its ask would otherwise time out.
	started = nil
	s.setRunState(false, "")
	s.ag.Enqueue("sub-agent big asks about 3.3: which tokenizer?")
	s.onSubAgentAsk(subagent.Ask{Node: "3.3", Owner: "big", Question: "which tokenizer?"})
	if len(started) != 1 || !strings.Contains(started[0], "asks about 3.3") {
		t.Fatalf("no turn was started for the ask: %q", started)
	}
}

// TestAHandBackMidRunDoesNotStartASecondTurn: the leftover-queue rule
// already delivers it, and starting one here would clobber the running
// turn's cancelFn.
func TestAHandBackMidRunDoesNotStartASecondTurn(t *testing.T) {
	s, _, _ := twoViews(t)
	var started []string
	s.startTurnHook = func(text string) { started = append(started, text) }
	s.Submit("do the thing", 1) // starts a turn: s.running is now true
	if len(started) != 1 {
		t.Fatalf("the typed request did not start a turn: %q", started)
	}
	s.ag.Enqueue("sub-agent big finished 3.2 (done): ported it")
	s.onSubAgentEnd(subagent.HandBack{Node: "3.2", Owner: "big", Status: "done", Summary: "ported it"})
	if len(started) != 1 {
		t.Fatalf("a second turn was started under the running one: %q", started)
	}
	if s.ag.Pending() != 1 {
		t.Fatal("the hand-back must stay queued for the running turn to deliver")
	}
	// And the run's own end takes it up, exactly as it did before.
	s.finishTurn(nil, nil)
	if len(started) != 2 || !strings.Contains(started[1], "sub-agent big finished 3.2") {
		t.Fatalf("the leftover-queue rule no longer picks it up: %q", started)
	}
}

// TestAnInterruptedHandBackStartsNothing: an interrupted run enqueues
// nothing (a session end is not a hand-back), so the idle session must stay
// idle rather than start an empty turn.
func TestAnInterruptedHandBackStartsNothing(t *testing.T) {
	s, _, _ := twoViews(t)
	var started []string
	s.startTurnHook = func(text string) { started = append(started, text) }
	s.onSubAgentEnd(subagent.HandBack{Node: "3.2", Owner: "big", Status: "interrupted"})
	if len(started) != 0 {
		t.Fatalf("an empty queue started a turn: %q", started)
	}
}
