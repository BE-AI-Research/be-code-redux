package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/store"
)

// On exit the agent writes a handoff briefing with the model; the request
// must carry the user's stated requirements so they survive into the next
// session.
func TestWriteHandoffUsesModelAndUserRequirements(t *testing.T) {
	var got provider.ChatRequest
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		got = req
		return &provider.ChatResponse{Content: "HANDOFF: widget must avoid cgo"}, nil
	}}
	ag, _ := newTestAgent(t, p, nil)
	ag.Session = store.NewSession("func", "test-model", ag.Tools.Root)
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "build the widget. REQUIREMENT: no cgo"})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "ok, done"})

	h, err := ag.WriteHandoff(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if h != "HANDOFF: widget must avoid cgo" || ag.Session.Handoff != h {
		t.Fatalf("handoff not stored: %q / %q", h, ag.Session.Handoff)
	}
	body := got.Messages[len(got.Messages)-1].Content
	if !strings.Contains(body, "REQUIREMENT: no cgo") {
		t.Fatalf("user requirement not sent to summarizer: %q", body)
	}
}

// If the model is unavailable, a heuristic handoff still records the task
// and the files that were written.
func TestWriteHandoffFallsBackWithoutModel(t *testing.T) {
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, errors.New("backend down")
	}}
	ag, _ := newTestAgent(t, p, nil)
	ag.Session = store.NewSession("func", "test-model", ag.Tools.Root)
	ag.History.Add(provider.Message{Role: provider.RoleUser, Content: "create hi.txt"})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{
		{ID: "1", Name: "write_file", Arguments: `{"path":"hi.txt","content":"hello"}`}}})
	ag.History.Add(provider.Message{Role: provider.RoleTool, ToolCallID: "1", Content: "wrote 5 bytes"})
	ag.History.Add(provider.Message{Role: provider.RoleAssistant, Content: "Created hi.txt."})

	h, err := ag.WriteHandoff(context.Background(), true)
	if err != nil {
		t.Fatalf("fallback should not error: %v", err)
	}
	for _, want := range []string{"create hi.txt", "hi.txt", "Created hi.txt."} {
		if !strings.Contains(h, want) {
			t.Fatalf("fallback handoff lacks %q:\n%s", want, h)
		}
	}
}

// Resuming a session with a handoff puts it in the system prompt, where it
// survives git refreshes, model switches and history trimming.
func TestResumeInjectsHandoffIntoSystemPrompt(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	s := store.NewSession("func", "test-model", ag.Tools.Root)
	s.Handoff = "REQ: never use cgo; sqlite chosen for storage"
	s.Messages = []provider.Message{{Role: provider.RoleUser, Content: "hi"}}
	ag.Resume(s)
	if !strings.Contains(ag.History.System.Content, "REQ: never use cgo") {
		t.Fatal("handoff missing from system prompt after Resume")
	}
	ag.SetModel("other-model")
	if !strings.Contains(ag.History.System.Content, "REQ: never use cgo") {
		t.Fatal("handoff lost after SetModel")
	}
	if !strings.Contains(ag.composeSystem("git: clean"), "REQ: never use cgo") {
		t.Fatal("handoff lost after git refresh")
	}
}

// The backend's real window overrides a larger configured budget and
// reserves generation headroom.
func TestApplyWindowClampsBudget(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	ag.History.Budget = 32768
	if !ag.ApplyWindow(8192) {
		t.Fatal("expected clamp")
	}
	if ag.History.Budget != 8192 || ag.History.Reserve < 1024 || ag.History.Limit() > 8192-1024 {
		t.Fatalf("budget=%d reserve=%d limit=%d", ag.History.Budget, ag.History.Reserve, ag.History.Limit())
	}
	if ag.ApplyWindow(65536) {
		t.Fatal("larger window must not raise the configured budget")
	}
	if ag.ApplyWindow(0) {
		t.Fatal("unknown window must be a no-op")
	}
}

// Repeated exits without the model must not nest "Previous briefing" inside
// itself; only the innermost real briefing is carried forward, once.
func TestHeuristicHandoffDoesNotNestPreviousBriefing(t *testing.T) {
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return nil, errors.New("backend down")
	}}
	ag, _ := newTestAgent(t, p, nil)
	s := store.NewSession("func", "test-model", ag.Tools.Root)
	// Doubly nested, as produced by the pre-fix code after two exits.
	s.Handoff = "Previous briefing:\nPrevious briefing:\nORIGINAL MODEL BRIEFING\n\nTask: t0\nChanges: c0\nLast assistant reply: r0\n\nTask: t\nChanges: c\nLast assistant reply: r"
	s.Messages = []provider.Message{{Role: provider.RoleUser, Content: "continue"}, {Role: provider.RoleAssistant, Content: "ok"}}
	ag.Resume(s)
	h, err := ag.WriteHandoff(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(h, "ORIGINAL MODEL BRIEFING") != 1 || strings.Count(h, "Previous briefing:") != 1 {
		t.Fatalf("briefing nested or lost:\n%s", h)
	}
}
