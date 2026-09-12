package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// Every theme in the table resolves, and each has its own accent colour.
func TestThemeTableResolvesDistinctAccents(t *testing.T) {
	names := ThemeNames()
	if len(names) != 13 {
		t.Fatalf("expected 13 themes, got %d: %v", len(names), names)
	}
	seen := map[string]string{}
	for _, n := range names {
		p, ok := lookupTheme(n)
		if !ok {
			t.Fatalf("theme %q missing from the table", n)
		}
		if n == "mono" {
			continue
		}
		key := p.Accent + "/" + p.StatusBG // Solarized dark/light share hues, not backgrounds
		if prev, dup := seen[key]; dup {
			t.Fatalf("themes %q and %q are identical (%s)", prev, n, key)
		}
		seen[key] = n
	}
}

// Unknown names fall back to dark and report it.
func TestUnknownThemeFallsBackToDark(t *testing.T) {
	applied := SetTheme("no-such-theme")
	if applied != "dark" {
		t.Fatalf("applied = %q, want dark", applied)
	}
	if applied := SetTheme("dracula"); applied != "dracula" {
		t.Fatalf("applied = %q", applied)
	}
	SetTheme("dark")
}

// /theme <name> changes the live palette and persists the choice.
func TestThemeCommandAppliesAndPersists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	m := newTestModel(t)
	SetTheme("dark")
	before := stAccent.GetForeground()
	m.slashCommand("/theme nord", 0)
	after := stAccent.GetForeground()
	if before == after {
		t.Fatal("accent did not change")
	}
	if m.cfg.Theme != "nord" {
		t.Fatalf("cfg.Theme = %q", m.cfg.Theme)
	}
	if !strings.Contains(m.transcript.String(), "theme set to nord") {
		t.Fatal("no confirmation line")
	}
	_ = lipgloss.Color("")
	SetTheme("dark")
}

// /theme with no argument opens a picker with every theme.
func TestThemePickerListsAll(t *testing.T) {
	m := newTestModel(t)
	m.slashCommand("/theme", 0)
	if m.mode != modePicker || m.picker == nil {
		t.Fatalf("picker not opened: mode=%v", m.mode)
	}
	m.pickerUpdate(pickerItemsMsg{items: m.themeItems()})
	if n := len(m.picker.filtered()); n != 13 {
		t.Fatalf("picker lists %d themes, want 13", n)
	}
}
