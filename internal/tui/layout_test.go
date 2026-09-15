package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// The context wheel is one glyph: fill level when idle, a rotating
// quadrant when the model works (one turn per four frames).
func TestWheelGlyphs(t *testing.T) {
	cases := []struct {
		pct  int
		want string
	}{{0, "○"}, {10, "○"}, {25, "◔"}, {50, "◑"}, {75, "◕"}, {100, "●"}, {130, "●"}}
	for _, c := range cases {
		if got := wheelGlyph(c.pct, false, 0); got != c.want {
			t.Errorf("idle %d%%: got %q want %q", c.pct, got, c.want)
		}
	}
	frames := []string{"◴", "◵", "◶", "◷"}
	for f := 0; f < 8; f++ {
		if got := wheelGlyph(50, true, f); got != frames[f%4] {
			t.Errorf("busy frame %d: got %q want %q", f, got, frames[f%4])
		}
	}
}

// The header is drawn only on terminals with at least 30 rows.
func TestHeaderHiddenOnShortTerminals(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	if strings.Contains(m.View(), "BE-Code Redux") {
		t.Fatal("header shown on a 24-row terminal")
	}
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	v := m.View()
	for _, want := range []string{"BE-Code Redux", "2026 BE AI Research", "https://github.com/BE-AI-Research - v1.0"} {
		if !strings.Contains(v, want) {
			t.Fatalf("header lacks %q on a 40-row terminal:\n%s", want, v)
		}
	}
}

// One bottom line carries the menu hint, the model and the state; the
// wheel with its percentage sits on the input row.
func TestBottomLineAndWheelInView(t *testing.T) {
	m := newTestModel(t)
	v := m.View()
	for _, want := range []string{"/menu", "/help", "ready", "%"} {
		if !strings.Contains(v, want) {
			t.Fatalf("view lacks %q:\n%s", want, v)
		}
	}
	if !strings.Contains(v, "(>):") {
		t.Fatalf("prompt marker missing:\n%s", v)
	}
}

// Typing "/" into an empty input opens the palette; typing filters it; Esc
// closes it and leaves the typed text in the input.
func TestSlashOpensPaletteAndEscRestoresText(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if m.mode != modePalette {
		t.Fatalf("mode = %v, want palette", m.mode)
	}
	for _, r := range "hel" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	items := m.picker.filtered()
	if len(items) != 1 || items[0].id != "/help" {
		t.Fatalf("filter 'hel' gave %+v", items)
	}
	if !strings.Contains(m.View(), "/help") {
		t.Fatal("palette not rendered")
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != modeInput || m.input.Value() != "/hel" {
		t.Fatalf("after Esc: mode=%v input=%q", m.mode, m.input.Value())
	}
}

// Enter on a palette entry runs it; entries that take arguments are put
// into the input instead so the user can finish typing.
func TestPaletteEnterRunsOrFillsInput(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	for _, r := range "help" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modeInput || !strings.Contains(m.rendered.String(), "/sessions") {
		t.Fatalf("help not run from palette: mode=%v", m.mode)
	}

	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	for _, r := range "model" {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	// first match is /model (takes an argument)
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modeInput || m.input.Value() != "/model " {
		t.Fatalf("argument command not filled into input: mode=%v input=%q", m.mode, m.input.Value())
	}
}

// /menu opens the grouped full-screen menu with a status block; Esc returns.
func TestMenuOpensAndEscReturns(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	m.slashCommand("/menu")
	if m.mode != modeMenu {
		t.Fatalf("mode = %v, want menu", m.mode)
	}
	v := m.View()
	for _, want := range []string{"Sessions", "Models", "Context", "Settings", "provider", m.ag.Model} {
		if !strings.Contains(v, want) {
			t.Fatalf("menu lacks %q:\n%s", want, v)
		}
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if m.mode != modeInput {
		t.Fatalf("Esc did not return to input: %v", m.mode)
	}
}
