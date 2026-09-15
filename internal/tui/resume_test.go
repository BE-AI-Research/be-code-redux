package tui

import (
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/ui"
)

func savedSession(ag *agent.Agent) {
	s := store.NewSession("null", "m", ag.Tools.Root)
	s.Title = "fix the build"
	s.Messages = []provider.Message{
		{Role: provider.RoleUser, Content: "read a.go"},
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "1", Name: "read_file", Arguments: `{"path":"a.go"}`}}},
		{Role: provider.RoleTool, ToolCallID: "1", Name: "read_file", Content: "    1\tpackage a\n"},
		{Role: provider.RoleAssistant, Content: "A is defined."},
	}
	ag.Resume(s)
}

func entryTexts(s *Session) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.entries {
		out = append(out, e.Label+e.Text)
	}
	return out
}

func TestResumedSessionReplaysItsTranscript(t *testing.T) {
	tempHome(t)
	s := newTestSession(t, savedSession)
	got := strings.Join(entryTexts(s), "\n")
	for _, want := range []string{"you> read a.go", "read_file{\"path\":\"a.go\"}", "1\tpackage a", "A is defined.", ui.ReplayDivider, "resumed "} {
		if !strings.Contains(got, want) {
			t.Fatalf("replay lacks %q:\n%s", want, got)
		}
	}
	if strings.Index(got, ui.ReplayDivider) < strings.Index(got, "A is defined.") {
		t.Fatal("divider must follow the replayed transcript")
	}
}

func TestResumeReplayCanBeTurnedOff(t *testing.T) {
	tempHome(t)
	s := newTestSession(t, func(ag *agent.Agent) { ag.Cfg.ResumeReplay = false; savedSession(ag) })
	got := strings.Join(entryTexts(s), "\n")
	if strings.Contains(got, "you> read a.go") || strings.Contains(got, ui.ReplayDivider) {
		t.Fatalf("replayed although resume_replay is off:\n%s", got)
	}
	if !strings.Contains(got, "resumed ") {
		t.Fatalf("resume line missing:\n%s", got)
	}
}
