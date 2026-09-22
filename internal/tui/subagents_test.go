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
