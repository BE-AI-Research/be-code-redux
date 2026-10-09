package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func writeSourcePack(t *testing.T, src, name, version, desc string) {
	t.Helper()
	dir := filepath.Join(src, "packs", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"schema":1,"name":"` + name + `","version":"` + version + `","description":"` + desc + `",
  "core":{"file":"core.md","budget_tokens":1500}}`
	if err := os.WriteFile(filepath.Join(dir, "pack.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "core.md"), []byte("Rules.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The menu's "Install a skill pack" row opens a picker of what the source
// offers. Spec §9 asked for this; a usage string in its place made the one
// surface built for discoverability teach syntax instead.
func TestPackPickerListsWhatTheSourceOffers(t *testing.T) {
	src := t.TempDir()
	writeSourcePack(t, src, "assist", "0.1.0", "a personal assistant")
	writeSourcePack(t, src, "ue5", "0.2.0", "unreal engine 5")

	s := newTestSession(t)
	s.cfg.Packs.Source = src
	v := s.NewView(0, "local")
	v.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	_, cmd := v.openPackPicker()
	if cmd == nil {
		t.Fatal("openPackPicker returned no loader")
	}
	if v.picker == nil {
		t.Fatal("no picker was opened")
	}
	if !strings.Contains(v.picker.title, "pack") {
		t.Errorf("picker title does not say what it is for: %q", v.picker.title)
	}

	// The loader runs off the Update goroutine, which is what makes a network
	// clone here safe; in the test it is a local directory.
	msg := cmd()
	got, ok := msg.(pickerItemsMsg)
	if !ok {
		t.Fatalf("loader returned %T, want pickerItemsMsg", msg)
	}
	if got.err != nil {
		t.Fatalf("loader failed: %v", got.err)
	}
	if len(got.items) != 2 {
		t.Fatalf("got %d rows, want 2: %+v", len(got.items), got.items)
	}
	byID := map[string]pickItem{}
	for _, it := range got.items {
		byID[it.id] = it
	}
	a, ok := byID["assist"]
	if !ok {
		t.Fatalf("no row for assist: %+v", got.items)
	}
	if !strings.Contains(a.desc, "0.1.0") || !strings.Contains(a.desc, "personal assistant") {
		t.Errorf("a row should carry the version and the description: %+v", a)
	}
}

// Picking a row must not install inside Update: Session.Ask relocks the mutex
// Update holds, and pack_install is raised by the install. The pick hands off
// to the same off-thread path /packs add uses, and the picker closes first —
// picker.go:299 does that before calling onPick, so this drives the real key
// path rather than onPick directly.
func TestPackPickerHandsTheInstallOffThread(t *testing.T) {
	src := t.TempDir()
	writeSourcePack(t, src, "assist", "0.1.0", "a personal assistant")

	s := newTestSession(t)
	s.cfg.Packs.Source = src
	v := s.NewView(0, "local")
	v.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	_, load := v.openPackPicker()
	v.Update(load())
	if v.picker == nil || len(v.picker.items) != 1 {
		t.Fatalf("the picker did not take the loaded rows: %+v", v.picker)
	}

	_, cmd := v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("picking a pack did nothing; the install must be handed off as a Cmd")
	}
	// Closed by the pick, not left over the transcript while an approval
	// modal is raised behind it.
	if v.picker != nil || v.mode == modePicker {
		t.Error("the picker is still open after a pick")
	}
}

// `/packs add` with no name opens the picker rather than printing a usage
// line — the whole reason the picker exists.
func TestSlashPacksAddWithNoNameOpensThePicker(t *testing.T) {
	src := t.TempDir()
	writeSourcePack(t, src, "assist", "0.1.0", "a personal assistant")

	s := newTestSession(t)
	s.cfg.Packs.Source = src
	v := s.NewView(0, "local")
	v.Update(tea.WindowSizeMsg{Width: 80, Height: 24})

	v.slashCommand("/packs add")
	if v.picker == nil {
		t.Fatalf("/packs add did not open the picker; mode=%v transcript:\n%s", v.mode, v.rendered.String())
	}
	if strings.Contains(v.rendered.String(), "usage:") {
		t.Errorf("the transcript shows a usage line:\n%s", v.rendered.String())
	}
}
