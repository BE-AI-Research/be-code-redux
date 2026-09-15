package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
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
	m.applyTheme("nord", false)
	if m.st.Name() != "nord" {
		t.Fatalf("applyTheme left styles at %q", m.st.Name())
	}
	other := newTestModel(t)
	if other.st.Name() != "dark" {
		t.Fatalf("a second model inherited the first one's theme: %q — styles are still global", other.st.Name())
	}
}

// /theme <name> changes the live palette for this device only: it is
// remembered in client_themes under this view's label, and cfg.Theme (the
// config default new devices get) is untouched.
func TestThemeCommandAppliesAndPersists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	m := newTestModel(t)
	before := m.st.Accent.GetForeground()
	m.slashCommand("/theme nord")
	after := m.st.Accent.GetForeground()
	if before == after {
		t.Fatal("accent did not change")
	}
	if m.cfg.Theme != "dark" {
		t.Fatalf("cfg.Theme changed to %q by a per-device pick", m.cfg.Theme)
	}
	if got := m.cfg.ClientThemes["local"]; got != "nord" {
		t.Fatalf(`client_themes["local"] = %q`, got)
	}
	if !strings.Contains(m.rendered.String(), "theme set to nord") {
		t.Fatal("no confirmation line")
	}
	_ = lipgloss.Color("")
}

// The theme picker (bare /theme, or Settings → Theme in the menu) lists
// every theme plus the "default: …" row.
func TestThemePickerListsAll(t *testing.T) {
	m := newTestModel(t)
	m.openThemePicker()
	if m.mode != modePicker || m.picker == nil {
		t.Fatalf("picker not opened: mode=%v", m.mode)
	}
	m.pickerUpdate(pickerItemsMsg{items: m.themeItems()})
	if n := len(m.picker.filtered()); n != 14 {
		t.Fatalf("picker lists %d themes, want 14 (13 themes + default row)", n)
	}
}

func TestThemeIsPerViewAndRememberedByDevice(t *testing.T) {
	s, a, b := twoViews(t)
	a.slashCommand("/theme nord")
	if a.st.Name() != "nord" || b.st.Name() != "dark" {
		t.Fatalf("themes: a=%s b=%s", a.st.Name(), b.st.Name())
	}
	if got := s.cfg.ClientThemes["desk"]; got != "nord" {
		t.Fatalf("client_themes[desk] = %q", got)
	}
	if s.cfg.Theme != "dark" {
		t.Fatalf("config theme changed to %q by a per-device pick", s.cfg.Theme)
	}
	c := s.NewView(3, "desk (pid 99)")
	if c.st.Name() != "nord" {
		t.Fatalf("a new view from the same device got %q", c.st.Name())
	}
	if name, origin := s.themeFor("desk (pid 99)"); name != "nord" || origin != `remembered for "desk"` {
		t.Fatalf("themeFor = %q, %q", name, origin)
	}
	b.slashCommand("/theme default gruvbox")
	if s.cfg.Theme != "gruvbox" || b.st.Name() != "gruvbox" || a.st.Name() != "nord" {
		t.Fatalf("default: cfg=%s a=%s b=%s", s.cfg.Theme, a.st.Name(), b.st.Name())
	}
	d := s.NewView(4, "new device (pid 5)")
	if d.st.Name() != "gruvbox" {
		t.Fatalf("a new device did not get the config default: %s", d.st.Name())
	}
	// Bare /theme opens this terminal's picker, whose title says which theme
	// is in use and where it came from; nothing goes to the transcript.
	before := b.wrapped
	b.slashCommand("/theme")
	if b.mode != modePicker || b.picker == nil {
		t.Fatalf("bare /theme did not open the picker: mode=%v", b.mode)
	}
	if !strings.Contains(b.picker.title, "gruvbox (config default)") {
		t.Fatalf("picker title %q does not report the theme and its origin", b.picker.title)
	}
	if b.wrapped != before {
		t.Fatal("bare /theme wrote to the transcript")
	}
	if a.mode == modePicker {
		t.Fatal("the picker opened on another terminal")
	}
}

// In the "/" palette, Enter on /theme opens the picker directly instead of
// filling the input with "/theme " and waiting for a second Enter.
func TestPaletteEnterOnThemeOpensThePicker(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("theme")})
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modePicker || m.picker == nil || !strings.HasPrefix(m.picker.title, "Theme") {
		t.Fatalf("palette Enter on /theme: mode=%v picker=%v", m.mode, m.picker)
	}
}

func TestUnknownRememberedThemeFallsThroughWithAWarning(t *testing.T) {
	s := newTestSession(t)
	s.cfg.ClientThemes["phone"] = "no-such"
	v := s.NewView(7, "phone (pid 1)")
	v.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if v.st.Name() != "dark" {
		t.Fatalf("fallback theme %s", v.st.Name())
	}
	if !strings.Contains(v.wrapped, `theme "no-such" is not known; using dark`) {
		t.Fatalf("no warning:\n%s", v.wrapped)
	}
}

// The title and the input field must never depend on the terminal's own
// default colours: under a theme that recolours the window background,
// Termux showed neither the title nor the typed text. Every theme carries a
// body-text colour, the title and the textarea use it, and the textarea's
// cursor line has no forced ANSI-black background.
func TestTitleAndInputUseTheThemesTextColour(t *testing.T) {
	defer pinColorProfile()()
	for _, name := range ThemeNames() {
		st, _ := newStyles(name)
		if name != "mono" && st.Text.GetForeground() == nil {
			t.Errorf("theme %s has no body-text colour", name)
		}
	}
	m := newTestModel(t)
	m.applyTheme("solarized-dark", false)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40}) // header visible
	frame := m.View()
	title := m.st.Text.Bold(true).Render("BE-Code Redux")
	if !strings.Contains(frame, title) {
		t.Fatalf("header title is not rendered in the theme's text colour:\n%q", frame[:200])
	}
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hello")})
	frame = m.View()
	if strings.Contains(frame, "\x1b[40m") {
		t.Fatal("the input still forces an ANSI-black cursor-line background")
	}
	if !strings.Contains(frame, m.st.Text.Render("hello")) {
		t.Fatalf("typed text is not rendered in the theme's text colour")
	}
	// A theme change restyles the input field too.
	m.applyTheme("nord", false)
	if !strings.Contains(m.View(), m.st.Text.Render("hello")) {
		t.Fatal("typed text kept the old theme's colour after /theme")
	}
}
