package tui

import (
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/agent"
)

func TestBottomLineShowsSpendWhenOnline(t *testing.T) {
	s, a, _ := twoViews(t)
	a.mu.Lock()
	local := a.bottomLine()
	a.mu.Unlock()
	if strings.Contains(local, "$") {
		t.Fatalf("local line shows spend: %q", local)
	}
	s.ag.SetOnline("openrouter", "K", agent.Pricing{Prompt: 1e-6, Completion: 1e-6, Known: true})
	a.mu.Lock()
	line := a.bottomLine()
	a.mu.Unlock()
	if !strings.Contains(line, " · $0.00") {
		t.Fatalf("online line: %q", line)
	}
}
