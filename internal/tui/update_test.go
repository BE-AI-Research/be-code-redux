package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// update has no "always": "a" leaves the modal open and changes nothing.
func TestUpdateAskHasNoAlways(t *testing.T) {
	s, a, _ := twoViews(t)
	beforeShell, beforeBrowser := s.cfg.AutoApproveShell, s.cfg.AutoApproveBrowser
	decided := make(chan bool, 1)
	go func() { decided <- s.approveFromAgent("update", "Update BE-Code 1.0.0 → 9.9.9?") }()
	waitFor(t, func() bool { flush(a); return a.mode == modeAsk })
	view := a.View()
	if !strings.Contains(view, "Update BE-Code") || !strings.Contains(view, "y install · n not now") {
		t.Fatalf("title/hint:\n%s", view)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	flush(a)
	if a.mode != modeAsk || s.cfg.AutoApproveShell != beforeShell || s.cfg.AutoApproveBrowser != beforeBrowser {
		t.Fatal(`"a" must do nothing on an update prompt`)
	}
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	select {
	case ok := <-decided:
		if ok {
			t.Fatal("n declines")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("n never answered")
	}
}

// The notice is the session's, so every attached terminal shows it, and
// clearing it (after an update) clears it everywhere.
func TestNoticeOnEveryView(t *testing.T) {
	s, a, b := twoViews(t)
	s.SetUpdateAvailable("9.9.9")
	flush(a, b)
	for name, v := range map[string]*View{"a": a, "b": b} {
		s.mu.Lock()
		line := v.bottomLine()
		s.mu.Unlock()
		if !strings.Contains(line, "⬆ v9.9.9 available") {
			t.Fatalf("%s: %q", name, line)
		}
	}
	s.SetUpdateAvailable("")
	flush(a, b)
	s.mu.Lock()
	line := a.bottomLine()
	s.mu.Unlock()
	if strings.Contains(line, "available") {
		t.Fatalf("cleared notice still shown: %q", line)
	}
}

// The menu shows the check's state and flips it; turning it off clears a
// lit notice on every terminal.
func TestMenuUpdateCheckToggle(t *testing.T) {
	s, a, b := twoViews(t)
	label := func() string {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, e := range a.menuEntries() {
			if strings.HasPrefix(e.label, "Update check") {
				return e.label
			}
		}
		return ""
	}
	if got := label(); got != "Update check: on" {
		t.Fatalf("label %q", got)
	}
	s.SetUpdateAvailable("9.9.9")
	s.mu.Lock()
	a.slashCommand("/update check off")
	s.mu.Unlock()
	flush(a, b)
	if got := label(); got != "Update check: off" {
		t.Fatalf("label after off %q", got)
	}
	if s.UpdateAvailable() != "" {
		t.Fatal("notice still lit with the check off")
	}
}
