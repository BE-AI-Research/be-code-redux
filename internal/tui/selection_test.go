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

// screenRow is the terminal row on which the viewport currently shows the
// first line containing needle (a short transcript is anchored to the
// bottom of the viewport, so text is not at row 0).
func screenRow(t *testing.T, m *View, needle string) int {
	t.Helper()
	for i, l := range strings.Split(m.vp.View(), "\n") {
		if strings.Contains(l, needle) {
			return m.headerHeight() + i
		}
	}
	t.Fatalf("%q not on screen:\n%s", needle, m.vp.View())
	return 0
}

// Dragging across the transcript selects exactly the visible text between
// the anchor and the release point, across lines.
func TestDragSelectsTextAcrossLines(t *testing.T) {
	m := newTestModel(t) // 80x24, no header
	m.appendEntry(entry{Kind: entryPlain, Text: "alpha beta gamma"})
	m.appendEntry(entry{Kind: entryPlain, Text: "delta epsilon"})
	m.appendEntry(entry{Kind: entryPlain, Text: "zeta"})
	flush(m)
	r0, r1 := screenRow(t, m, "alpha"), screenRow(t, m, "delta")
	m.Update(mouse(6, r0, tea.MouseButtonLeft, tea.MouseActionPress))
	m.Update(mouse(4, r1, tea.MouseButtonLeft, tea.MouseActionMotion))
	m.Update(mouse(4, r1, tea.MouseButtonLeft, tea.MouseActionRelease))
	if m.sel == nil {
		t.Fatal("no selection after drag")
	}
	if got := m.selectionText(); got != "beta gamma\ndelta" {
		t.Fatalf("selection text = %q", got)
	}
	// A plain click (no drag) clears it.
	r2 := screenRow(t, m, "zeta")
	m.Update(mouse(1, r2, tea.MouseButtonLeft, tea.MouseActionPress))
	m.Update(mouse(1, r2, tea.MouseButtonLeft, tea.MouseActionRelease))
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
	r := screenRow(t, m, "copy me")
	m.Update(mouse(0, r, tea.MouseButtonLeft, tea.MouseActionPress))
	m.Update(mouse(6, r, tea.MouseButtonLeft, tea.MouseActionRelease))
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd != nil {
		t.Fatal("Ctrl+C with a selection must not quit")
	}
	if len(*got) != 1 || (*got)[0] != "copy me" {
		t.Fatalf("clipboard = %v", *got)
	}
	if m.sel != nil || m.quitHint {
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
	// The agent's own path: the session records the reply and the tool
	// output, and every view renders what it broadcasts.
	m.onDelta("The answer is 42.")
	m.mu.Lock()
	m.flushLocked()
	m.mu.Unlock()
	m.onToolEnd("shell", toolResult("total 3\nfile a\nfile b"))
	flush(m)
	m.slashCommand("/copy reply")
	m.slashCommand("/copy tool")
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

// The repaint request is a WindowSizeMsg at the size the view already has:
// the host asks for one through onClientSize after evicting output for a
// terminal that had fallen behind (live.Host.writer). It must redraw the
// frame and nothing else — a selection survives it, because no column has
// moved. A genuine resize still drops it.
func TestRepaintAtTheSameSizeKeepsTheSelection(t *testing.T) {
	m := newTestModel(t)
	m.appendEntry(entry{Kind: entryPlain, Text: "select me"})
	flush(m)
	m.Update(mouse(0, 0, tea.MouseButtonLeft, tea.MouseActionPress))
	m.Update(mouse(6, 0, tea.MouseButtonLeft, tea.MouseActionRelease))
	if m.sel == nil {
		t.Fatal("no selection to begin with")
	}
	m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if m.sel == nil {
		t.Fatal("a repaint at the same size dropped the selection")
	}
	m.Update(tea.WindowSizeMsg{Width: 70, Height: 24})
	if m.sel != nil {
		t.Fatal("a real resize must drop the selection: the columns have moved")
	}
}
