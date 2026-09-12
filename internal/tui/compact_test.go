package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestCompactEngagesOnSmallSizes(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if m.compact() {
		t.Fatal("compact at 100x30")
	}
	m.Update(tea.WindowSizeMsg{Width: 60, Height: 18})
	if !m.compact() {
		t.Fatal("not compact at 60x18")
	}
	v := m.View()
	if strings.Contains(v, "BE-Code Redux") || strings.Contains(v, "BE AI Research") {
		t.Fatal("header/attribution shown in compact")
	}
	if !strings.Contains(v, "\n>") && !strings.HasPrefix(v, ">") {
		t.Fatalf("prompt not shortened:\n%s", v)
	}
	if strings.Contains(v, "(>):") {
		t.Fatal("full prompt in compact")
	}
	m.cfg.Layout = "full"
	if m.compact() {
		t.Fatal("layout=full must disable compact")
	}
	m.cfg.Layout = "compact"
	m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	if !m.compact() {
		t.Fatal("layout=compact must force compact")
	}
}

func TestCompactBottomLineAndPopups(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 56, Height: 18})
	m.ag.IDEName = "vscode"
	m.mode = modeBusy
	m.running = true
	m.ag.Enqueue("one")
	m.ag.Enqueue("two")
	v := m.View()
	for _, want := range []string{"/menu", "q2", "⌘"} {
		if !strings.Contains(v, want) {
			t.Fatalf("compact bottom line lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "Enter queues") {
		t.Fatal("long hint shown in compact")
	}
	m.mode = modeInput
	m.running = false
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	v = m.View()
	if strings.Contains(v, "command reference") {
		t.Fatal("palette descriptions shown under 60 columns")
	}
}

// omitPopupDesc must be gated on compact() first: "layout: full" forces
// compact() false, so descriptions come back even under 60 columns; the
// default "auto" still omits them at the same size.
func TestCompactPopupDescRespectsLayoutOverride(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 56, Height: 18})

	m.cfg.Layout = "full"
	m.openPalette("")
	v := m.View()
	if !strings.Contains(v, "command reference") {
		t.Fatalf("layout=full must show palette descriptions even under 60 columns:\n%s", v)
	}

	m.cfg.Layout = "auto"
	m.openPalette("")
	v = m.View()
	if strings.Contains(v, "command reference") {
		t.Fatalf("layout=auto must omit palette descriptions under 60 columns:\n%s", v)
	}
}

func TestASCIIFallbacks(t *testing.T) {
	m := newTestModel(t)
	m.ascii = true
	m.ag.IDEName = "vscode"
	v := m.View()
	if strings.Contains(v, "◑") || strings.Contains(v, "⌘") {
		t.Fatalf("non-ASCII glyphs with an ASCII client:\n%s", v)
	}
	if !strings.Contains(v, "IDE") {
		t.Fatal("ASCII IDE marker missing")
	}
}
