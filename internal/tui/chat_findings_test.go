package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/inbox"
	"github.com/brown-enterprises/be-code/internal/live"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/tools"
	"github.com/brown-enterprises/be-code/internal/ui"
)

// Task 7 review findings, fixed here. Each test names the finding it covers.

// Finding 1: a view's room copy must drift from the session's after the cap
// never — including the trim marker.
func TestViewRoomMatchesSessionRoomAfterCap(t *testing.T) {
	s := newTestSession(t)
	s.ag.SetSession(&store.Session{ID: "s"})
	v := s.NewView(1, "local")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	for i := 0; i < roomCap+5; i++ {
		s.Post("alice", "line", "")
		drainAll(t, v)
	}
	want := s.Room()
	if len(v.room) != len(want) {
		t.Fatalf("len(v.room) = %d, len(s.Room()) = %d", len(v.room), len(want))
	}
	for i := range want {
		if v.room[i] != want[i] {
			t.Fatalf("room[%d]: view %+v != session %+v", i, v.room[i], want[i])
		}
	}
	if v.room[0].Text != "(older chat trimmed)" {
		t.Fatalf("view room missing the trim marker: %+v", v.room[0])
	}
}

// Finding 2: /clear must reset every attached view's own room copy, not just
// the session's, and a view sitting in modeChat must render the empty room.
func TestClearResetsEveryAttachedViewsRoom(t *testing.T) {
	s := newTestSession(t)
	s.ag.SetSession(&store.Session{ID: "s"})
	a, b := s.NewView(1, "vscode (pid 1)"), s.NewView(2, "ssh from 192.168.1.38 (pid 2)")
	for _, v := range []*View{a, b} {
		v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	}
	s.mu.Lock()
	s.ids[1] = identity{ID: "you"}
	s.mu.Unlock()
	s.Post("alice", "hello room", "")
	drainAll(t, a, b)
	a.slashCommand("/chat")
	if a.mode != modeChat {
		t.Fatalf("setup: a.mode = %v, want modeChat", a.mode)
	}
	if len(a.room) == 0 || len(b.room) == 0 {
		t.Fatalf("setup: rooms should be non-empty before /clear: a=%d b=%d", len(a.room), len(b.room))
	}
	a.slashCommand("/clear")
	waitFor(t, func() bool {
		drainAll(t, a, b)
		return len(a.room) == 0 && len(b.room) == 0
	})
	if len(a.room) != 0 {
		t.Fatalf("a.room not reset: %+v", a.room)
	}
	if len(b.room) != 0 {
		t.Fatalf("b.room not reset: %+v", b.room)
	}
	if a.mode != modeChat {
		t.Fatalf("a.mode = %v, want modeChat still (unaffected by /clear)", a.mode)
	}
	if got := a.viewChat(); got == "" || strings.Contains(got, "hello room") {
		t.Fatalf("a's chat view still shows the old room:\n%s", got)
	}
}

// Finding 3(i): a shared ask arriving while a terminal is in the room must
// not strand it in the "message the room…" placeholder outside modeChat —
// answering the ask returns it to modeChat with its own placeholder.
func TestAskInterruptingChatReturnsToChatAfterAnswer(t *testing.T) {
	s := newTestSession(t)
	v := s.NewView(1, "local")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	s.mu.Lock()
	s.ids[1] = identity{ID: "you"}
	s.mu.Unlock()
	v.slashCommand("/chat")
	if v.mode != modeChat {
		t.Fatalf("setup: mode = %v, want modeChat", v.mode)
	}
	chatPlaceholder := v.input.Placeholder

	go s.approveFromAgent("shell", "ls")
	waitFor(t, func() bool { drainAll(t, v); return v.mode == modeAsk })

	v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	drainAll(t, v)
	if v.mode != modeChat {
		t.Fatalf("mode after answering the ask = %v, want modeChat", v.mode)
	}
	if v.input.Placeholder != chatPlaceholder {
		t.Fatalf("placeholder = %q, want the chat one %q", v.input.Placeholder, chatPlaceholder)
	}
}

// Finding 3(ii): a run starting and ending while a terminal is in the room
// must never touch that terminal's mode or placeholder.
func TestRunStateNeverTouchesAModeInTheRoom(t *testing.T) {
	s := newTestSession(t)
	v := s.NewView(1, "local")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	s.mu.Lock()
	s.ids[1] = identity{ID: "you"}
	s.mu.Unlock()
	v.slashCommand("/chat")
	want := v.input.Placeholder

	s.mu.Lock()
	s.setRunStateLocked(true, "thinking")
	s.mu.Unlock()
	drainAll(t, v)
	if v.mode != modeChat || v.input.Placeholder != want {
		t.Fatalf("after running=true: mode=%v placeholder=%q, want modeChat/%q", v.mode, v.input.Placeholder, want)
	}

	s.mu.Lock()
	s.setRunStateLocked(false, "")
	s.mu.Unlock()
	drainAll(t, v)
	if v.mode != modeChat || v.input.Placeholder != want {
		t.Fatalf("after running=false: mode=%v placeholder=%q, want modeChat/%q", v.mode, v.input.Placeholder, want)
	}
}

// Finding 3(iii): /menu typed inside the room, then Esc, must return to the
// room rather than the ordinary transcript.
func TestMenuInterruptingChatReturnsToChat(t *testing.T) {
	s := newTestSession(t)
	v := s.NewView(1, "local")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	s.mu.Lock()
	s.ids[1] = identity{ID: "you"}
	s.mu.Unlock()
	v.slashCommand("/chat")
	want := v.input.Placeholder

	v.input.SetValue("/menu")
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if v.mode != modeMenu {
		t.Fatalf("mode after /menu = %v, want modeMenu", v.mode)
	}

	v.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if v.mode != modeChat {
		t.Fatalf("mode after Esc = %v, want modeChat", v.mode)
	}
	if v.input.Placeholder != want {
		t.Fatalf("placeholder = %q, want %q", v.input.Placeholder, want)
	}
}

// I2: a view that fell behind and had broadcasts dropped repairs its
// transcript from the session's entries. Its copy of the room has to be
// repaired the same way — nothing else ever tells it a chatMsg went missing,
// so the hole would be permanent.
func TestRebuildResyncsTheViewsRoomAfterADroppedBroadcast(t *testing.T) {
	s := newTestSession(t)
	v := s.NewView(1, "x")
	v.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	s.mu.Lock()
	s.ids[1] = identity{ID: "you"}
	s.mu.Unlock()
	v.slashCommand("/chat")
	drainAll(t, v)
	for i := 0; i < mailboxDepth+50; i++ {
		s.Post("alice", fmt.Sprintf("line %d", i), "")
	}
	v.mb.mu.Lock()
	lost := v.mb.lost
	v.mb.mu.Unlock()
	if !lost {
		t.Fatal("the test never actually overflowed the mailbox")
	}
	flush(v)
	want := s.Room()
	if len(v.room) != len(want) {
		t.Fatalf("view room has %d lines, the session's has %d", len(v.room), len(want))
	}
	for i := range want {
		if v.room[i] != want[i] {
			t.Fatalf("room[%d]: view %+v != session %+v", i, v.room[i], want[i])
		}
	}
	if out := v.viewChat(); !strings.Contains(out, fmt.Sprintf("line %d", mailboxDepth+49)) {
		t.Fatalf("the room on screen was not re-laid out:\n%s", out)
	}
}

// M2: with chat switched off, all four commands say so in the same words —
// and none of them asks an unnamed terminal who it is first.
func TestChatDisabledSaysSoBeforeAskingForAName(t *testing.T) {
	s := newTestSession(t)
	s.cfg.Chat.Enabled = false
	s.resolveFn = func(inbox.Terminal) (inbox.Resolution, error) { return inbox.Resolution{Ask: true}, nil }
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "l", IP: "10.0.0.5"}})
	v := s.NewView(1, "l")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	for _, cmd := range []string{"/chat", "/inbox", "/dm bob", "/whoami"} {
		v.slashCommand(cmd)
		if got := lastEntryText(s); got != "chat is disabled in config" {
			t.Fatalf("%s said %q", cmd, got)
		}
		if v.mode == modeName {
			t.Fatalf("%s asked for a name with chat disabled", cmd)
		}
	}
}

// M1: the four commands that take no argument run straight from the palette
// rather than filling the input line; only /dm takes one.
func TestChatSlashCommandArgs(t *testing.T) {
	want := map[string]bool{"/chat": false, "/inbox": false, "/back": false, "/whoami": false, "/dm": true}
	seen := 0
	for _, c := range ui.SlashCommandTable {
		if args, ok := want[c.Name]; ok {
			seen++
			if c.Args != args {
				t.Errorf("%s: Args = %v, want %v", c.Name, c.Args, args)
			}
		}
	}
	if seen != len(want) {
		t.Fatalf("found %d of %d commands in the table", seen, len(want))
	}
}

// M8: a session with chat switched off creates no mailbox directory.
func TestChatDisabledCreatesNoInboxDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfg := config.Default()
	cfg.RepoMap = false
	cfg.Chat.Enabled = false
	reg, err := tools.NewRegistry(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	s := NewSession(cfg, agent.New(cfg, nullProvider{}, "m", reg, ""), nullProvider{})
	if s.inboxDir != "" {
		t.Fatalf("inboxDir = %q with chat disabled", s.inboxDir)
	}
	if _, err := os.Stat(filepath.Join(home, ".be-code", "inbox")); !os.IsNotExist(err) {
		t.Fatalf("the mailbox directory was created anyway: %v", err)
	}
}
