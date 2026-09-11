package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
)

// The context provider's note is prepended to a new user request, shown as
// a notice, and NOT added to repair prompts (run with newTurn=false).
func TestContextProviderPrependsNoteOnNewTurnsOnly(t *testing.T) {
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{Content: "ok"}, nil
	}}
	ag, _ := newTestAgent(t, p, nil)
	ag.ContextProvider = func(context.Context) string { return "[editor: a.go, cursor line 3]" }
	notes := collectNotices(ag)
	ag.Run(context.Background(), "explain this")
	first := p.reqs[0].Messages[len(p.reqs[0].Messages)-1]
	if !strings.HasPrefix(first.Content, "[editor: a.go, cursor line 3]\n\nexplain this") {
		t.Fatalf("note not prepended: %q", first.Content)
	}
	if !hasNotice(*notes, "editor:") {
		t.Fatalf("no notice: %v", *notes)
	}
	ag.run(context.Background(), "fix the failing check", false)
	last := p.reqs[1].Messages[len(p.reqs[1].Messages)-1]
	if strings.Contains(last.Content, "[editor:") {
		t.Fatal("note must not be added to repair prompts")
	}
}

func TestGuidanceAppendedToSystemPrompt(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	ag.Guidance = IDEGuidance
	if !strings.Contains(ag.composeSystem(""), "ide_diagnostics") {
		t.Fatal("guidance missing from system prompt")
	}
}
