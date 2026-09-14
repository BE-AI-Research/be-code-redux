package tui

import (
	"encoding/base64"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/tools"
)

func toolResult(s string) tools.Result { return tools.Result{Content: s} }

func mouse(x, y int, btn tea.MouseButton, act tea.MouseAction) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Button: btn, Action: act}
}

func captureClipboard(m *View) *[]string {
	var got []string
	m.clipboardWrite = func(s string) error { got = append(got, s); return nil }
	return &got
}

// Dragging across the transcript selects exactly the visible text between
// the anchor and the release point, across lines.
func TestDragSelectsTextAcrossLines(t *testing.T) {
	m := newTestModel(t) // 80x24, no header: transcript starts at row 0
	m.appendEntry(entry{Kind: entryPlain, Text: "alpha beta gamma"})
	m.appendEntry(entry{Kind: entryPlain, Text: "delta epsilon"})
	m.appendEntry(entry{Kind: entryPlain, Text: "zeta"})
	flush(m)
	m.Update(mouse(6, 0, tea.MouseButtonLeft, tea.MouseActionPress))
	m.Update(mouse(4, 1, tea.MouseButtonLeft, tea.MouseActionMotion))
	m.Update(mouse(4, 1, tea.MouseButtonLeft, tea.MouseActionRelease))
	if m.sel == nil {
		t.Fatal("no selection after drag")
	}
	if got := m.selectionText(); got != "beta gamma\ndelta" {
		t.Fatalf("selection text = %q", got)
	}
	// A plain click (no drag) clears it.
	m.Update(mouse(1, 2, tea.MouseButtonLeft, tea.MouseActionPress))
	m.Update(mouse(1, 2, tea.MouseButtonLeft, tea.MouseActionRelease))
	if m.sel != nil {
		t.Fatal("click without drag did not clear the selection")
	}
}

// Ctrl+C with a selection copies it (and clears it) instead of arming quit.
func TestCtrlCCopiesSelection(t *testing.T) {
	m := newTestModel(t)
	got := captureClipboard(m)
	m.appendEntry(entry{Kind: entryPlain, Text: "copy me please"})
	flush(m)
	m.Update(mouse(0, 0, tea.MouseButtonLeft, tea.MouseActionPress))
	m.Update(mouse(6, 0, tea.MouseButtonLeft, tea.MouseActionRelease))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd != nil {
		t.Fatal("Ctrl+C with a selection must not quit")
	}
	if len(*got) != 1 || (*got)[0] != "copy me" {
		t.Fatalf("clipboard = %v", *got)
	}
	if m.sel != nil || m.quitHint[0] {
		t.Fatal("selection not cleared or quit armed")
	}
}

// Right-click opens the context menu with the copy/paste entries.
func TestRightClickOpensContextMenu(t *testing.T) {
	m := newTestModel(t)
	m.appendEntry(entry{Kind: entryPlain, Text: "something"})
	flush(m)
	m.Update(mouse(2, 0, tea.MouseButtonRight, tea.MouseActionPress))
	if m.mode != modeContextMenu {
		t.Fatalf("mode = %v, want context menu", m.mode)
	}
	v := m.View()
	for _, want := range []string{"Copy last reply", "Copy last tool output", "Select all", "Paste into input"} {
		if !strings.Contains(v, want) {
			t.Fatalf("menu lacks %q:\n%s", want, v)
		}
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != modeInput {
		t.Fatal("Esc did not close the context menu")
	}
}

// /copy targets: the last assistant reply and the last tool output.
func TestCopyCommandTargets(t *testing.T) {
	m := newTestModel(t)
	got := captureClipboard(m)
	m.Update(deltaMsg("The answer is 42."))
	m.flushStreaming()
	m.Update(toolEndMsg{name: "shell", res: toolResult("total 3\nfile a\nfile b")})
	m.slashCommand("/copy reply", 0)
	m.slashCommand("/copy tool", 0)
	flush(m)
	if len(*got) != 2 || (*got)[0] != "The answer is 42." || (*got)[1] != "total 3\nfile a\nfile b" {
		t.Fatalf("clipboard = %q", *got)
	}
	if !strings.Contains(m.rendered.String(), "copied") {
		t.Fatal("no confirmation in transcript")
	}
}

// The OSC 52 sequence carries base64 of the text and terminates properly.
func TestOSC52Sequence(t *testing.T) {
	seq := osc52("hello\nworld")
	if !strings.HasPrefix(seq, "\x1b]52;c;") || !strings.HasSuffix(seq, "\x07") {
		t.Fatalf("bad framing: %q", seq)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(seq, "\x1b]52;c;"), "\x07")
	dec, err := base64.StdEncoding.DecodeString(payload)
	if err != nil || string(dec) != "hello\nworld" {
		t.Fatalf("payload = %q err=%v", dec, err)
	}
}
