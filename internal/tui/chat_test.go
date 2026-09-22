package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/store"
)

func TestPostBroadcastsToEveryViewAndMirrorsToTheStore(t *testing.T) {
	s := newTestSession(t)
	s.ag.SetSession(&store.Session{ID: "s"})
	a, b := s.NewView(1, "vscode (pid 1)"), s.NewView(2, "ssh from 192.168.1.38 (pid 2)")
	for _, v := range []*View{a, b} {
		v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	}
	s.Post("alice", "hello room", "")
	drainAll(t, a, b)
	if len(a.room) != 1 || len(b.room) != 1 || b.room[0].Text != "hello room" {
		t.Fatalf("views: %+v / %+v", a.room, b.room)
	}
	if got := s.ag.Session.Chat; len(got) != 1 || got[0].User != "alice" {
		t.Fatalf("store: %+v", got)
	}
	if a.chatUnseen != 1 || b.chatUnseen != 1 {
		t.Fatalf("unseen: %d %d", a.chatUnseen, b.chatUnseen)
	}
	if !strings.Contains(a.bottomLine(), "chat (1 new)") {
		t.Fatalf("bottom line: %q", a.bottomLine())
	}
}

func TestChatModeLeavesTheRunStateAloneAndEscReturns(t *testing.T) {
	s := newTestSession(t)
	v := s.NewView(1, "local")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	// Task 8: /chat asks for a name first when this terminal has none; giving
	// it one up front keeps this test about the room, not the naming prompt
	// (identity_test.go covers that).
	s.mu.Lock()
	s.ids[1] = identity{ID: "you"}
	s.setRunStateLocked(true, "thinking")
	s.mu.Unlock()
	drainAll(t, v)
	v.slashCommand("/chat")
	if v.mode != modeChat || !s.running || v.chatUnseen != 0 {
		t.Fatalf("mode %v running %v unseen %d", v.mode, s.running, v.chatUnseen)
	}
	out := v.View()
	if !strings.Contains(out, "chat ·") || !strings.Contains(out, "Esc back") {
		t.Fatalf("chat view:\n%s", out)
	}
	v.input.SetValue("hi there")
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	drainAll(t, v)
	if r := s.Room(); len(r) != 2 || r[1].Text != "hi there" || r[0].Kind != "join" { // join line, then the post
		t.Fatalf("room %+v", r)
	}
	v.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if v.mode != modeBusy || !s.running {
		t.Fatalf("after Esc: mode %v running %v", v.mode, s.running)
	}
}

func TestSlashCommandsWorkInsideChat(t *testing.T) {
	s := newTestSession(t)
	v := s.NewView(1, "local")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	// Task 8: give this terminal a name up front so /chat opens the room
	// directly rather than the naming prompt (identity_test.go covers that).
	s.mu.Lock()
	s.ids[1] = identity{ID: "you"}
	s.mu.Unlock()
	v.slashCommand("/chat")
	v.input.SetValue("/back")
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if v.mode != modeInput {
		t.Fatalf("mode %v", v.mode)
	}
	if r := s.Room(); len(r) != 1 || r[0].Kind != "join" {
		t.Fatalf("/back was posted as text: %+v", r)
	}
}

func TestRoomIsCappedAndRestored(t *testing.T) {
	s := newTestSession(t)
	s.ag.SetSession(&store.Session{ID: "s"})
	for i := 0; i < roomCap+5; i++ {
		s.Post("alice", "line", "")
	}
	r := s.Room()
	if len(r) != roomCap || r[0].Text != "(older chat trimmed)" {
		t.Fatalf("len %d first %+v", len(r), r[0])
	}
	s2 := newTestSession(t)
	s2.restoreRoom(r)
	if len(s2.Room()) != roomCap {
		t.Fatal("restore lost lines")
	}
}
