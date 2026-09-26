package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/inbox"
	"github.com/brown-enterprises/be-code/internal/live"
)

// attachUnnamed is a session whose one terminal resolves with no name,
// offered choices, binding whatever is chosen into *bound.
func attachUnnamed(t *testing.T, choices []string, bound *string) (*Session, *View) {
	t.Helper()
	s := newTestSession(t)
	s.resolveFn = func(inbox.Terminal) (inbox.Resolution, error) {
		return inbox.Resolution{Ask: true, Choices: choices}, nil
	}
	s.bindFn = func(id string, _ inbox.Terminal) error { *bound = id; return nil }
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "ssh from 10.0.0.5 (pid 1)", IP: "10.0.0.5"}})
	v := s.NewView(1, "ssh from 10.0.0.5 (pid 1)")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return s, v
}

// A terminal that attaches with no name is asked at once — not first at
// /chat — and can choose a name already seen from its address.
func TestUnnamedTerminalIsAskedOnAttach(t *testing.T) {
	var bound string
	_, v := attachUnnamed(t, []string{"alice"}, &bound)
	if v.mode != modeName {
		t.Fatalf("no naming prompt on attach, mode %v", v.mode)
	}
	out := v.View()
	if !strings.Contains(out, "alice") || !strings.Contains(out, "Esc skip for now") {
		t.Fatalf("prompt:\n%s", out)
	}
	bindEnter(t, v) // the highlighted choice: alice
	if bound != "alice" || v.userID() != "alice" || v.mode != modeInput {
		t.Fatalf("bound %q id %q mode %v", bound, v.userID(), v.mode)
	}
}

// The prompt's other tool: typing a new name instead of choosing one.
func TestAttachPromptTakesATypedName(t *testing.T) {
	var bound string
	_, v := attachUnnamed(t, []string{"alice"}, &bound)
	v.Update(tea.KeyMsg{Type: tea.KeyDown}) // the "type a name below" row
	v.input.SetValue("Carol")
	bindEnter(t, v)
	if bound != "carol" || v.userID() != "carol" || v.mode != modeInput {
		t.Fatalf("bound %q id %q mode %v", bound, v.userID(), v.mode)
	}
}

// An invalid name keeps the prompt open with the reason, as it does at /chat.
func TestAttachPromptRejectsAnInvalidName(t *testing.T) {
	var bound string
	_, v := attachUnnamed(t, nil, &bound)
	v.input.SetValue("agent")
	bindEnter(t, v)
	if bound != "" || v.mode != modeName || !strings.Contains(v.View(), "reserved") {
		t.Fatalf("bound %q mode %v:\n%s", bound, v.mode, v.View())
	}
}

// Esc skips it: not asked again on this attachment, and /chat still asks.
func TestAttachPromptSkippedWithEscIsNotAskedAgain(t *testing.T) {
	var bound string
	s, v := attachUnnamed(t, nil, &bound)
	v.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if v.mode != modeInput {
		t.Fatalf("Esc left mode %v", v.mode)
	}
	v.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "ssh from 10.0.0.5 (pid 1)", IP: "10.0.0.5"}})
	flush(v)
	if v.mode != modeInput || v.userID() != "" {
		t.Fatalf("asked again after Esc: mode %v id %q", v.mode, v.userID())
	}
	v.slashCommand("/chat")
	if v.mode != modeName {
		t.Fatalf("/chat did not ask after a skip, mode %v", v.mode)
	}
}

// /chat on the open startup prompt carries on into the room once a name is
// chosen — the same prompt, now with somewhere to go.
func TestChatOnTheStartupPromptEntersTheRoom(t *testing.T) {
	var bound string
	_, v := attachUnnamed(t, nil, &bound)
	v.slashCommand("/chat")
	v.input.SetValue("dave")
	bindEnter(t, v)
	if v.userID() != "dave" || v.mode != modeChat {
		t.Fatalf("id %q mode %v", v.userID(), v.mode)
	}
}

func TestNamedTerminalIsNotAskedOnAttach(t *testing.T) {
	s := newTestSession(t)
	s.resolveFn = func(inbox.Terminal) (inbox.Resolution, error) { return inbox.Resolution{ID: "alice", How: "ip"}, nil }
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "l", IP: "10.0.0.1"}})
	v := s.NewView(1, "l")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if v.mode != modeInput {
		t.Fatalf("a named terminal was prompted, mode %v", v.mode)
	}
}

func TestAttachPromptOffWithChatOff(t *testing.T) {
	var bound string
	s := newTestSession(t)
	s.cfg.Chat.Enabled = false
	s.resolveFn = func(inbox.Terminal) (inbox.Resolution, error) { return inbox.Resolution{Ask: true}, nil }
	s.bindFn = func(id string, _ inbox.Terminal) error { bound = id; return nil }
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "l", IP: "10.0.0.1"}})
	v := s.NewView(1, "l")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if v.mode != modeInput || bound != "" {
		t.Fatalf("asked for a chat name with chat off, mode %v bound %q", v.mode, bound)
	}
}

// Opening the prompt resets the input line: a terminal that is already
// typing is not asked until its line is empty.
func TestAttachPromptKeepsADraft(t *testing.T) {
	s := newTestSession(t)
	s.resolveFn = func(inbox.Terminal) (inbox.Resolution, error) { return inbox.Resolution{Ask: true}, nil }
	v := s.NewView(1, "l")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	v.input.SetValue("half a thought")
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "l", IP: "10.0.0.1"}})
	flush(v)
	if v.mode != modeInput || v.input.Value() != "half a thought" {
		t.Fatalf("mode %v input %q", v.mode, v.input.Value())
	}
	v.input.Reset()
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if v.mode != modeName {
		t.Fatalf("not asked once the line was empty, mode %v", v.mode)
	}
}

// A startup question already open (the sub-agent resume, a model reload) is
// never covered: the name prompt waits for it to be answered.
func TestAttachPromptWaitsForAnOpenAsk(t *testing.T) {
	s := newTestSession(t)
	s.resolveFn = func(inbox.Terminal) (inbox.Resolution, error) { return inbox.Resolution{Ask: true}, nil }
	v := s.NewView(1, "l")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	decided := make(chan bool, 1)
	go func() { decided <- s.approveFromAgent("sub_agent_resume", "resume sub-agent work from the previous session?") }()
	waitFor(t, func() bool { flush(v); return v.mode == modeAsk })
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "l", IP: "10.0.0.1"}})
	flush(v)
	if v.mode != modeAsk {
		t.Fatalf("the name prompt covered an open question, mode %v", v.mode)
	}
	v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	select {
	case <-decided:
	case <-time.After(2 * time.Second):
		t.Fatal("the question was never answered")
	}
	waitFor(t, func() bool { flush(v); return v.mode == modeName })
}

// namedSession is one terminal already named alice (how says where from),
// recording rebinds and binds.
func namedSession(t *testing.T, how string, rebound, bound *[2]string) (*Session, *View) {
	t.Helper()
	s := newTestSession(t)
	s.resolveFn = func(inbox.Terminal) (inbox.Resolution, error) { return inbox.Resolution{ID: "alice", How: how}, nil }
	s.rebindFn = func(oldID, newID string, _ inbox.Terminal) error { *rebound = [2]string{oldID, newID}; return nil }
	s.bindFn = func(id string, _ inbox.Terminal) error { *bound = [2]string{"", id}; return nil }
	s.usedFromFn = func(string) string { return "" }
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "l", IP: "10.0.0.1"}})
	v := s.NewView(1, "l")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return s, v
}

func TestWhoamiSetChangesTheName(t *testing.T) {
	var rebound, bound [2]string
	s, v := namedSession(t, "ip", &rebound, &bound)
	v.slashCommand("/whoami set")
	if v.mode != modeName || !strings.Contains(v.View(), "you are alice") {
		t.Fatalf("mode %v:\n%s", v.mode, v.View())
	}
	v.input.SetValue("Carol")
	bindEnter(t, v)
	if rebound != [2]string{"alice", "carol"} || bound != [2]string{} {
		t.Fatalf("rebound %v bound %v", rebound, bound)
	}
	if v.userID() != "carol" || v.mode != modeInput || !strings.Contains(lastEntryText(s), "you are now carol") {
		t.Fatalf("id %q mode %v last %q", v.userID(), v.mode, lastEntryText(s))
	}
}

func TestWhoamiSetSameNameChangesNothing(t *testing.T) {
	var rebound, bound [2]string
	s, v := namedSession(t, "ip", &rebound, &bound)
	v.slashCommand("/whoami set")
	v.input.SetValue("alice")
	bindEnter(t, v)
	if rebound != [2]string{} || v.userID() != "alice" || v.mode != modeInput || !strings.Contains(lastEntryText(s), "you are already alice") {
		t.Fatalf("rebound %v id %q mode %v last %q", rebound, v.userID(), v.mode, lastEntryText(s))
	}
}

func TestWhoamiSetEscKeepsTheName(t *testing.T) {
	var rebound, bound [2]string
	_, v := namedSession(t, "ip", &rebound, &bound)
	v.slashCommand("/whoami set")
	v.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if v.mode != modeInput || v.userID() != "alice" || rebound != [2]string{} {
		t.Fatalf("mode %v id %q rebound %v", v.mode, v.userID(), rebound)
	}
}

func TestWhoamiSetWithAConfigNameSaysEditTheConfig(t *testing.T) {
	var rebound, bound [2]string
	s, v := namedSession(t, "config", &rebound, &bound)
	v.slashCommand("/whoami set")
	if v.mode != modeInput || !strings.Contains(lastEntryText(s), "chat.name") {
		t.Fatalf("mode %v last %q", v.mode, lastEntryText(s))
	}
}

// With no name yet, /whoami set is just the ordinary first naming: a bind.
func TestWhoamiSetWithNoNameBinds(t *testing.T) {
	var bound string
	_, v := attachUnnamed(t, nil, &bound)
	v.Update(tea.KeyMsg{Type: tea.KeyEsc}) // skip the startup prompt
	v.slashCommand("/whoami set")
	v.input.SetValue("erin")
	bindEnter(t, v)
	if bound != "erin" || v.userID() != "erin" {
		t.Fatalf("bound %q id %q", bound, v.userID())
	}
}

func TestWhoamiMentionsSet(t *testing.T) {
	var rebound, bound [2]string
	s, v := namedSession(t, "ip", &rebound, &bound)
	v.slashCommand("/whoami")
	if !strings.Contains(lastEntryText(s), "/whoami set changes it") {
		t.Fatalf("%q", lastEntryText(s))
	}
}
