package ide

import (
	"context"
	"strings"
	"testing"
)

func TestContextNoteFormats(t *testing.T) {
	n := ContextNote(Context{File: "internal/agent/loop.go", Line: 214, SelStart: 210, SelEnd: 231, Selection: "func x() {}"})
	if !strings.HasPrefix(n, "[editor: internal/agent/loop.go, cursor line 214, selection lines 210–231]") {
		t.Fatalf("note = %q", n)
	}
	if !strings.Contains(n, "func x() {}") {
		t.Fatal("selection text missing")
	}
	if ContextNote(Context{File: "a.go", Line: 3}) != "[editor: a.go, cursor line 3]" {
		t.Fatalf("no-selection note = %q", ContextNote(Context{File: "a.go", Line: 3}))
	}
	if ContextNote(Context{}) != "" {
		t.Fatal("no active file must produce no note")
	}
	long := strings.Repeat("x", 3000)
	if n := ContextNote(Context{File: "a.go", Line: 1, SelStart: 1, SelEnd: 9, Selection: long}); strings.Contains(n, long) {
		t.Fatal("selection over 2KB must be omitted from the note")
	}
}

func TestSessionContextNoteNilSafe(t *testing.T) {
	var s *Session
	if s.ContextNote(context.Background()) != "" {
		t.Fatal("nil session must yield empty note")
	}
	s = &Session{}
	if s.ContextNote(context.Background()) != "" {
		t.Fatal("session with nil client must yield empty note")
	}
}
