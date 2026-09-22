package tui

import (
	"strings"
	"testing"
	"time"

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
