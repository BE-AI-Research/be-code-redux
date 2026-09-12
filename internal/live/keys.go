package live

import (
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/input"
)

// ClientKeyMsg is a key press from one attached terminal.
type ClientKeyMsg struct {
	Client int
	Key    tea.KeyMsg
}

// ClientMouseMsg is a mouse event from one attached terminal.
type ClientMouseMsg struct {
	Client int
	Mouse  tea.MouseMsg
}

var specialKeys = map[rune]tea.KeyType{
	input.KeyEnter: tea.KeyEnter, input.KeyTab: tea.KeyTab, input.KeyBackspace: tea.KeyBackspace,
	input.KeyEscape: tea.KeyEsc, input.KeyUp: tea.KeyUp, input.KeyDown: tea.KeyDown,
	input.KeyLeft: tea.KeyLeft, input.KeyRight: tea.KeyRight, input.KeyHome: tea.KeyHome,
	input.KeyEnd: tea.KeyEnd, input.KeyPgUp: tea.KeyPgUp, input.KeyPgDown: tea.KeyPgDown,
	input.KeyDelete: tea.KeyDelete, input.KeyInsert: tea.KeyInsert,
	input.KeyF1: tea.KeyF1, input.KeyF2: tea.KeyF2, input.KeyF3: tea.KeyF3, input.KeyF4: tea.KeyF4,
	input.KeyF5: tea.KeyF5, input.KeyF6: tea.KeyF6, input.KeyF7: tea.KeyF7, input.KeyF8: tea.KeyF8,
	input.KeyF9: tea.KeyF9, input.KeyF10: tea.KeyF10, input.KeyF11: tea.KeyF11, input.KeyF12: tea.KeyF12,
	input.KeyF13: tea.KeyF13, input.KeyF14: tea.KeyF14, input.KeyF15: tea.KeyF15, input.KeyF16: tea.KeyF16,
	input.KeyF17: tea.KeyF17, input.KeyF18: tea.KeyF18, input.KeyF19: tea.KeyF19, input.KeyF20: tea.KeyF20,
}

// modKey identifies a special key plus the (alt/ctrl-stripped) modifier
// combination that Bubble Tea v1 names as its own KeyType (e.g. shift+tab,
// ctrl+left).
type modKey struct {
	code rune
	mod  input.KeyMod
}

var modifiedKeys = map[modKey]tea.KeyType{
	{input.KeyTab, input.ModShift}: tea.KeyShiftTab,
	{input.KeyUp, input.ModShift}:  tea.KeyShiftUp, {input.KeyDown, input.ModShift}: tea.KeyShiftDown,
	{input.KeyLeft, input.ModShift}: tea.KeyShiftLeft, {input.KeyRight, input.ModShift}: tea.KeyShiftRight,
	{input.KeyHome, input.ModShift}: tea.KeyShiftHome, {input.KeyEnd, input.ModShift}: tea.KeyShiftEnd,
	{input.KeyUp, input.ModCtrl}: tea.KeyCtrlUp, {input.KeyDown, input.ModCtrl}: tea.KeyCtrlDown,
	{input.KeyLeft, input.ModCtrl}: tea.KeyCtrlLeft, {input.KeyRight, input.ModCtrl}: tea.KeyCtrlRight,
	{input.KeyHome, input.ModCtrl}: tea.KeyCtrlHome, {input.KeyEnd, input.ModCtrl}: tea.KeyCtrlEnd,
	{input.KeyPgUp, input.ModCtrl}: tea.KeyCtrlPgUp, {input.KeyPgDown, input.ModCtrl}: tea.KeyCtrlPgDown,
	{input.KeyUp, input.ModCtrl | input.ModShift}: tea.KeyCtrlShiftUp, {input.KeyDown, input.ModCtrl | input.ModShift}: tea.KeyCtrlShiftDown,
	{input.KeyLeft, input.ModCtrl | input.ModShift}: tea.KeyCtrlShiftLeft, {input.KeyRight, input.ModCtrl | input.ModShift}: tea.KeyCtrlShiftRight,
	{input.KeyHome, input.ModCtrl | input.ModShift}: tea.KeyCtrlShiftHome, {input.KeyEnd, input.ModCtrl | input.ModShift}: tea.KeyCtrlShiftEnd,
}

// ctrlPunct maps the lower-cased code of a ctrl-chord over punctuation to the
// KeyType Bubble Tea v1 uses for it (these are the C0 control codes that
// aren't letters).
var ctrlPunct = map[rune]tea.KeyType{
	'@': tea.KeyCtrlAt, ' ': tea.KeyCtrlAt, '[': tea.KeyEsc, '\\': tea.KeyCtrlBackslash,
	']': tea.KeyCtrlCloseBracket, '^': tea.KeyCtrlCaret, '_': tea.KeyCtrlUnderscore, '?': tea.KeyCtrlQuestionMark,
}

func convertKey(k input.Key) (tea.KeyMsg, bool) {
	alt := k.Mod&input.ModAlt != 0
	mod := k.Mod &^ (input.ModAlt | input.ModCapsLock | input.ModNumLock)
	if t, ok := modifiedKeys[modKey{k.Code, mod}]; ok {
		return tea.KeyMsg{Type: t, Alt: alt}, true
	}
	if mod&input.ModCtrl != 0 {
		c := unicode.ToLower(k.Code)
		if c >= 'a' && c <= 'z' {
			return tea.KeyMsg{Type: tea.KeyType(c - 'a' + 1), Alt: alt}, true // KeyCtrlA == 1
		}
		if t, ok := ctrlPunct[c]; ok {
			return tea.KeyMsg{Type: t, Alt: alt}, true
		}
	}
	if t, ok := specialKeys[k.Code]; ok {
		return tea.KeyMsg{Type: t, Alt: alt}, true
	}
	if k.Code == input.KeySpace {
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}, Alt: alt}, true
	}
	if k.Text != "" {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k.Text), Alt: alt}, true
	}
	if k.Code > 0 && k.Code < input.KeyExtended && unicode.IsPrint(k.Code) {
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{k.Code}, Alt: alt}, true
	}
	return tea.KeyMsg{}, false
}

var mouseButtons = map[ansi.MouseButton]tea.MouseButton{
	ansi.MouseNone: tea.MouseButtonNone, ansi.MouseLeft: tea.MouseButtonLeft, ansi.MouseMiddle: tea.MouseButtonMiddle,
	ansi.MouseRight: tea.MouseButtonRight, ansi.MouseWheelUp: tea.MouseButtonWheelUp, ansi.MouseWheelDown: tea.MouseButtonWheelDown,
	ansi.MouseWheelLeft: tea.MouseButtonWheelLeft, ansi.MouseWheelRight: tea.MouseButtonWheelRight,
	ansi.MouseBackward: tea.MouseButtonBackward, ansi.MouseForward: tea.MouseButtonForward,
}

func convertMouse(m input.Mouse, action tea.MouseAction) tea.MouseMsg {
	return tea.MouseMsg{X: m.X, Y: m.Y, Action: action, Button: mouseButtons[m.Button],
		Shift: m.Mod&input.ModShift != 0, Alt: m.Mod&input.ModAlt != 0, Ctrl: m.Mod&input.ModCtrl != 0}
}

// ConvertEvent maps an x/input event onto the Bubble Tea v1 message the
// program would have received from its own input reader. ok is false for
// events with no v1 equivalent (e.g. UnknownEvent, focus/window-size/paste
// boundary events).
func ConvertEvent(ev input.Event) (tea.Msg, bool) {
	switch e := ev.(type) {
	case input.KeyPressEvent:
		return convertKey(input.Key(e))
	case input.PasteEvent:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(string(e)), Paste: true}, true
	case input.MouseClickEvent:
		return convertMouse(input.Mouse(e), tea.MouseActionPress), true
	case input.MouseReleaseEvent:
		return convertMouse(input.Mouse(e), tea.MouseActionRelease), true
	case input.MouseMotionEvent:
		return convertMouse(input.Mouse(e), tea.MouseActionMotion), true
	case input.MouseWheelEvent:
		return convertMouse(input.Mouse(e), tea.MouseActionPress), true
	}
	return nil, false
}
