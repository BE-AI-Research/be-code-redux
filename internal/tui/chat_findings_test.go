package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/store"
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
