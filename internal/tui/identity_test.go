package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/inbox"
	"github.com/brown-enterprises/be-code/internal/live"
)

func TestConfigNameResolvesOnAttach(t *testing.T) {
	s := newTestSession(t)
	s.resolveFn = func(tm inbox.Terminal) (inbox.Resolution, error) {
		if tm.User == "alice" {
			return inbox.Resolution{ID: "alice", How: "config"}, nil
		}
		return inbox.Resolution{Ask: true}, nil
	}
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "vscode (pid 1)", IP: "192.168.1.38", User: "alice"}, {ID: 2, Label: "ssh from 192.168.1.40 (pid 2)", IP: "192.168.1.40"}})
	v1, v2 := s.NewView(1, "vscode (pid 1)"), s.NewView(2, "ssh from 192.168.1.40 (pid 2)")
	if v1.userID() != "alice" || v2.userID() != "" {
		t.Fatalf("%q %q", v1.userID(), v2.userID())
	}
	v1.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	v1.slashCommand("/whoami")
	if !strings.Contains(lastEntryText(s), "you are alice (from config)") {
		t.Fatalf("%q", lastEntryText(s))
	}
	v1.slashCommand("/clients")
	if !strings.Contains(lastEntryText(s), "alice") {
		t.Fatalf("/clients lacks the ID: %q", lastEntryText(s))
	}
}

func TestUnknownTerminalIsAskedOnceThenBound(t *testing.T) {
	s := newTestSession(t)
	bound := ""
	s.resolveFn = func(tm inbox.Terminal) (inbox.Resolution, error) { return inbox.Resolution{Ask: true}, nil }
	s.bindFn = func(id string, tm inbox.Terminal) error { bound = id; return nil }
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "ssh from 10.0.0.5 (pid 1)", IP: "10.0.0.5"}})
	v := s.NewView(1, "ssh from 10.0.0.5 (pid 1)")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	v.slashCommand("/chat")
	if v.mode != modeName {
		t.Fatalf("expected the naming prompt, mode %v", v.mode)
	}
	v.input.SetValue("Bob")
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if bound != "bob" || v.userID() != "bob" || v.mode != modeChat {
		t.Fatalf("bound %q id %q mode %v", bound, v.userID(), v.mode)
	}
	// A bad name re-prompts with the reason.
	v2 := s.NewView(1, "x")
	s.mu.Lock()
	delete(s.ids, 1)
	s.mu.Unlock()
	v2.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	v2.slashCommand("/chat")
	v2.input.SetValue("agent")
	v2.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if v2.mode != modeName || !strings.Contains(v2.View(), "reserved") {
		t.Fatalf("mode %v view %q", v2.mode, v2.View())
	}
}

func TestChoicesAreOfferedForASharedIP(t *testing.T) {
	s := newTestSession(t)
	s.resolveFn = func(tm inbox.Terminal) (inbox.Resolution, error) {
		return inbox.Resolution{Ask: true, Choices: []string{"alice", "bob"}}, nil
	}
	s.bindFn = func(string, inbox.Terminal) error { return nil }
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "l", IP: "10.0.0.9"}})
	v := s.NewView(1, "l")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	v.slashCommand("/chat")
	out := v.View()
	if v.mode != modeName || !strings.Contains(out, "alice") || !strings.Contains(out, "bob") {
		t.Fatalf("mode %v:\n%s", v.mode, out)
	}
	v.Update(tea.KeyMsg{Type: tea.KeyDown}) // bob
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if v.userID() != "bob" {
		t.Fatalf("id %q", v.userID())
	}
}
