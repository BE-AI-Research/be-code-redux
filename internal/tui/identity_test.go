package tui

import (
	"context"
	"errors"
	"os"
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
	bindEnter(t, v)
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
	bindEnter(t, v2)
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
	bindEnter(t, v)
	if v.userID() != "bob" {
		t.Fatalf("id %q", v.userID())
	}
}

// Resolution runs off the session lock: it can read a file and, elsewhere,
// run `arp -a` for up to 3 s, which under mu would freeze every terminal.
func TestResolutionRunsOffTheSessionLock(t *testing.T) {
	s := newTestSession(t)
	lockedDuring := false
	s.resolveFn = func(inbox.Terminal) (inbox.Resolution, error) {
		if !s.mu.TryLock() {
			lockedDuring = true
		} else {
			s.mu.Unlock()
		}
		return inbox.Resolution{ID: "alice", How: "ip"}, nil
	}
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "l", IP: "10.0.0.1"}})
	if lockedDuring {
		t.Fatal("resolveFn ran with mu held")
	}
	if s.identityOf(1).ID != "alice" {
		t.Fatalf("identity %+v", s.identityOf(1))
	}
	// Resolved once: a repeated roster does not ask again.
	calls := 0
	s.resolveFn = func(inbox.Terminal) (inbox.Resolution, error) { calls++; return inbox.Resolution{}, nil }
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "l", IP: "10.0.0.1"}})
	if calls != 0 {
		t.Fatalf("resolved again: %d", calls)
	}
}

// The room's leave line uses the name the terminal resolved to, which may
// have been asked for after it attached — not the device label.
func TestLeaveLineUsesTheResolvedName(t *testing.T) {
	s := newTestSession(t)
	s.resolveFn = func(inbox.Terminal) (inbox.Resolution, error) { return inbox.Resolution{Ask: true}, nil }
	s.bindFn = func(string, inbox.Terminal) error { return nil }
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "ssh from 10.0.0.5 (pid 1)", IP: "10.0.0.5"}})
	v := s.NewView(1, "ssh from 10.0.0.5 (pid 1)")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	v.slashCommand("/chat")
	v.input.SetValue("bob")
	bindEnter(t, v)
	s.SetClients(nil)
	r := s.Room()
	last := r[len(r)-1]
	if last.Kind != "leave" || last.Text != "bob left" {
		t.Fatalf("leave line %+v", last)
	}
}

// I4: an unhosted session (RunLocal) is wired like a served one — a roster
// of one terminal, so identity resolves and a name can be bound at all.
func TestLocalSessionWiresItsOwnTerminal(t *testing.T) {
	s := newTestSession(t)
	s.cfg.Chat.Name = "alice"
	var seen inbox.Terminal
	s.resolveFn = func(tm inbox.Terminal) (inbox.Resolution, error) {
		seen = tm
		return inbox.Resolution{ID: tm.User, How: "config"}, nil
	}
	s.wireLocalClient(context.Background())
	v := s.NewView(0, "local")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if v.userID() != "alice" {
		t.Fatalf("id %q", v.userID())
	}
	if seen.IP != "127.0.0.1" || seen.PID != os.Getpid() || seen.User != "alice" || seen.Login == "" {
		t.Fatalf("terminal %+v", seen)
	}
	v.slashCommand("/whoami")
	if !strings.Contains(lastEntryText(s), "you are alice (from config)") {
		t.Fatalf("%q", lastEntryText(s))
	}
}

// I4: a client the roster does not know has no address to bind a name to.
// Binding one anyway wrote an empty Terminal — an ID usable from nowhere.
func TestBindRefusesAClientWithNoTerminal(t *testing.T) {
	s := newTestSession(t)
	bound := false
	s.bindFn = func(string, inbox.Terminal) error { bound = true; return nil }
	s.mu.Lock()
	cmd, err := s.bindCmdLocked(7, "alice")
	s.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "no terminal to bind") {
		t.Fatalf("err %v", err)
	}
	if cmd != nil || bound {
		t.Fatalf("bound anyway: cmd %v bound %v", cmd != nil, bound)
	}
	if s.identityOf(7).ID != "" {
		t.Fatalf("identity written: %+v", s.identityOf(7))
	}
}

// I5: inbox.Bind takes a cross-process file lock and may wait on another
// host. It must not run under the session lock, which every terminal's
// Update holds for its whole body.
func TestBindRunsOffTheSessionLock(t *testing.T) {
	s := newTestSession(t)
	s.resolveFn = func(inbox.Terminal) (inbox.Resolution, error) { return inbox.Resolution{Ask: true}, nil }
	lockedDuring := false
	s.bindFn = func(string, inbox.Terminal) error {
		if !s.mu.TryLock() {
			lockedDuring = true
		} else {
			s.mu.Unlock()
		}
		return nil
	}
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "l", IP: "10.0.0.5"}})
	v := s.NewView(1, "l")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	v.slashCommand("/chat")
	v.input.SetValue("bob")
	bindEnter(t, v)
	if lockedDuring {
		t.Fatal("bindFn ran with mu held")
	}
	if v.userID() != "bob" || v.mode != modeChat {
		t.Fatalf("id %q mode %v", v.userID(), v.mode)
	}
}

// I5: a bind that fails leaves the terminal in the prompt with the reason.
func TestFailedBindStaysInThePrompt(t *testing.T) {
	s := newTestSession(t)
	s.resolveFn = func(inbox.Terminal) (inbox.Resolution, error) { return inbox.Resolution{Ask: true}, nil }
	s.bindFn = func(string, inbox.Terminal) error { return errors.New("users.json is read-only") }
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "l", IP: "10.0.0.5"}})
	v := s.NewView(1, "l")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	v.slashCommand("/chat")
	v.input.SetValue("bob")
	bindEnter(t, v)
	if v.mode != modeName || v.userID() != "" {
		t.Fatalf("mode %v id %q", v.mode, v.userID())
	}
	if !strings.Contains(v.View(), "read-only") {
		t.Fatalf("the reason was not shown:\n%s", v.View())
	}
}

// I6(b), spec §9: a name is exclusive. A second client claiming it is never
// handed the bare name; it gets the next free suffix and its own inbox thread,
// on the same Enter — no share prompt.
func TestDuplicateNameGetsItsOwnSuffixedID(t *testing.T) {
	s := newTestSession(t)
	s.resolveFn = func(inbox.Terminal) (inbox.Resolution, error) { return inbox.Resolution{Ask: true}, nil }
	s.usedFromFn = func(id string) string {
		if id == "alice" {
			return "192.168.1.40"
		}
		return ""
	}
	bound := ""
	s.bindFn = func(id string, tm inbox.Terminal) error { bound = id; return nil }
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "l", IP: "10.0.0.5"}})
	v := s.NewView(1, "l")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	v.slashCommand("/chat")
	v.input.SetValue("alice")
	bindEnter(t, v)
	if bound != "alice01" || v.userID() != "alice01" {
		t.Fatalf("second claimant should take the free suffix: bound %q id %q", bound, v.userID())
	}
	if v.mode != modeChat {
		t.Fatalf("a suffixed bind goes straight through, mode %v", v.mode)
	}
	if !strings.Contains(v.View(), "alice is already in use; you are alice01 (a separate thread)") {
		t.Fatalf("notice:\n%s", v.View())
	}
	// A different, free name still binds at once, unsuffixed.
	s.mu.Lock()
	delete(s.ids, 1)
	s.mu.Unlock()
	v2 := s.NewView(1, "l")
	v2.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	v2.slashCommand("/chat")
	v2.input.SetValue("bob")
	bindEnter(t, v2)
	if bound != "bob" || v2.userID() != "bob" {
		t.Fatalf("a free name should bind at once: bound %q id %q", bound, v2.userID())
	}
}
