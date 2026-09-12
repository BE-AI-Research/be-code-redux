package live

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/input"
)

func key(t *testing.T, ev input.Event) tea.KeyMsg {
	t.Helper()
	m, ok := ConvertEvent(ev)
	if !ok {
		t.Fatalf("event %v not converted", ev)
	}
	k, isKey := m.(tea.KeyMsg)
	if !isKey {
		t.Fatalf("event %v converted to %T, want KeyMsg", ev, m)
	}
	return k
}

func TestConvertRunesAndModifiers(t *testing.T) {
	k := key(t, input.KeyPressEvent{Code: 'a', Text: "a"})
	if k.Type != tea.KeyRunes || string(k.Runes) != "a" || k.Alt {
		t.Fatalf("plain rune: %+v", k)
	}
	k = key(t, input.KeyPressEvent{Code: 'x', Text: "x", Mod: input.ModAlt})
	if k.Type != tea.KeyRunes || string(k.Runes) != "x" || !k.Alt {
		t.Fatalf("alt rune: %+v", k)
	}
	k = key(t, input.KeyPressEvent{Code: 'c', Mod: input.ModCtrl})
	if k.Type != tea.KeyCtrlC {
		t.Fatalf("ctrl+c: %+v", k)
	}
	k = key(t, input.KeyPressEvent{Code: 'q', Mod: input.ModCtrl})
	if k.Type != tea.KeyCtrlQ {
		t.Fatalf("ctrl+q: %+v", k)
	}
	k = key(t, input.KeyPressEvent{Code: input.KeySpace, Text: " "})
	if k.Type != tea.KeySpace || string(k.Runes) != " " {
		t.Fatalf("space: %+v", k)
	}
	k = key(t, input.KeyPressEvent{Code: ']', Mod: input.ModCtrl})
	if k.Type != tea.KeyCtrlCloseBracket {
		t.Fatalf("ctrl+]: %+v", k)
	}
}

func TestConvertSpecialKeys(t *testing.T) {
	cases := map[rune]tea.KeyType{
		input.KeyEnter: tea.KeyEnter, input.KeyTab: tea.KeyTab, input.KeyBackspace: tea.KeyBackspace,
		input.KeyEscape: tea.KeyEsc, input.KeyUp: tea.KeyUp, input.KeyDown: tea.KeyDown,
		input.KeyLeft: tea.KeyLeft, input.KeyRight: tea.KeyRight, input.KeyHome: tea.KeyHome,
		input.KeyEnd: tea.KeyEnd, input.KeyPgUp: tea.KeyPgUp, input.KeyPgDown: tea.KeyPgDown,
		input.KeyDelete: tea.KeyDelete, input.KeyInsert: tea.KeyInsert, input.KeyF1: tea.KeyF1, input.KeyF12: tea.KeyF12,
	}
	for code, want := range cases {
		if got := key(t, input.KeyPressEvent{Code: code}).Type; got != want {
			t.Errorf("code %d: got %v want %v", code, got, want)
		}
	}
	k := key(t, input.KeyPressEvent{Code: input.KeyTab, Mod: input.ModShift})
	if k.Type != tea.KeyShiftTab {
		t.Fatalf("shift+tab: %+v", k)
	}
	k = key(t, input.KeyPressEvent{Code: input.KeyUp, Mod: input.ModShift})
	if k.Type != tea.KeyShiftUp {
		t.Fatalf("shift+up: %+v", k)
	}
	k = key(t, input.KeyPressEvent{Code: input.KeyLeft, Mod: input.ModCtrl})
	if k.Type != tea.KeyCtrlLeft {
		t.Fatalf("ctrl+left: %+v", k)
	}
	k = key(t, input.KeyPressEvent{Code: input.KeyEnter, Mod: input.ModAlt})
	if k.Type != tea.KeyEnter || !k.Alt {
		t.Fatalf("alt+enter: %+v", k)
	}
}

func TestConvertPasteAndMouse(t *testing.T) {
	m, ok := ConvertEvent(input.PasteEvent("two\nlines"))
	k := m.(tea.KeyMsg)
	if !ok || k.Type != tea.KeyRunes || !k.Paste || string(k.Runes) != "two\nlines" {
		t.Fatalf("paste: %+v %v", k, ok)
	}
	m, ok = ConvertEvent(input.MouseClickEvent{X: 3, Y: 4, Button: ansi.MouseLeft})
	mm := m.(tea.MouseMsg)
	if !ok || mm.X != 3 || mm.Y != 4 || mm.Action != tea.MouseActionPress || mm.Button != tea.MouseButtonLeft {
		t.Fatalf("click: %+v", mm)
	}
	m, _ = ConvertEvent(input.MouseReleaseEvent{X: 3, Y: 4, Button: ansi.MouseLeft})
	if m.(tea.MouseMsg).Action != tea.MouseActionRelease {
		t.Fatal("release")
	}
	m, _ = ConvertEvent(input.MouseMotionEvent{X: 5, Y: 6, Button: ansi.MouseLeft})
	if mm := m.(tea.MouseMsg); mm.Action != tea.MouseActionMotion || mm.Button != tea.MouseButtonLeft {
		t.Fatalf("drag: %+v", mm)
	}
	m, _ = ConvertEvent(input.MouseWheelEvent{X: 1, Y: 1, Button: ansi.MouseWheelUp, Mod: input.ModShift})
	if mm := m.(tea.MouseMsg); mm.Button != tea.MouseButtonWheelUp || mm.Action != tea.MouseActionPress || !mm.Shift {
		t.Fatalf("wheel: %+v", mm)
	}
	if _, ok := ConvertEvent(input.UnknownEvent("x")); ok {
		t.Fatal("unknown events must be dropped")
	}
}
