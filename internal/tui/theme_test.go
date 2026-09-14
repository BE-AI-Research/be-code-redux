package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

func TestNewStylesResolvesEveryThemeAndRejectsUnknown(t *testing.T) {
	for _, name := range ThemeNames() {
		st, ok := newStyles(name)
		if !ok {
			t.Fatalf("theme %q not resolved", name)
		}
		if st.Name() != name {
			t.Fatalf("styles for %q report name %q", name, st.Name())
		}
	}
	if _, ok := newStyles("no-such-theme"); ok {
		t.Fatal("unknown theme resolved")
	}
	// Render actually differs only under a colour-capable profile; outside a
	// real terminal (as in this test run) lipgloss degrades to no colour and
	// every Render call would look identical regardless of style.
	defer pinColorProfile()()
	dark, _ := newStyles("dark")
	nord, _ := newStyles("nord")
	if dark.Accent.Render("x") == nord.Accent.Render("x") {
		t.Fatal("two themes render the accent identically; styles are not per value")
	}
}

func TestModelCarriesItsOwnStyles(t *testing.T) {
	m := newTestModel(t)
	if m.st.Name() != "dark" {
		t.Fatalf("default styles %q, want dark", m.st.Name())
	}
	m.applyTheme("nord")
	if m.st.Name() != "nord" {
		t.Fatalf("applyTheme left styles at %q", m.st.Name())
	}
	other := newTestModel(t)
	if other.st.Name() != "dark" {
		t.Fatalf("a second model inherited the first one's theme: %q — styles are still global", other.st.Name())
	}
}

// /theme <name> changes the live palette and persists the choice.
func TestThemeCommandAppliesAndPersists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	m := newTestModel(t)
	before := m.st.Accent.GetForeground()
	m.slashCommand("/theme nord", 0)
	after := m.st.Accent.GetForeground()
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
