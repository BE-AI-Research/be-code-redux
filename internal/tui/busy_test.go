package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/store"
)

func busyModel(t *testing.T) *View {
	t.Helper()
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	m.mode = modeBusy
	m.running = true
	return m
}

func TestHeaderShowsSessionCode(t *testing.T) {
	m := newTestModel(t)
	m.ag.SetSession(store.NewSession("p", "m", t.TempDir()))
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	v := m.View()
	if !strings.Contains(v, "session "+m.ag.Session.ResumeCode()) {
		t.Fatalf("header lacks the session code:\n%s", v)
	}
	if strings.Contains(v, "⚛") {
		t.Fatal("the old glyph is still drawn")
	}
}

func TestSafeCommandsRunWhileBusyOthersWait(t *testing.T) {
	m := busyModel(t)
	m.input.SetValue("/clients")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(m.rendered.String(), "not served") {
		t.Fatalf("/clients did not run during the turn:\n%s", m.rendered.String())
	}
	m.input.SetValue("/verify")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(m.rendered.String(), "commands wait until the agent is done") {
		t.Fatalf("/verify must wait:\n%s", m.rendered.String())
	}
	if m.ag.Pending() != 0 {
		t.Fatal("a refused command must not be queued as text")
	}
}

func TestPaletteAndMenuOpenWhileBusy(t *testing.T) {
	m := busyModel(t)
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'/'}})
	if m.mode != modePalette {
		t.Fatalf("palette did not open while busy: mode %v", m.mode)
	}
	waits, runs := 0, 0
	for _, it := range m.slashEntries() {
		if strings.HasPrefix(it.desc, "after the run · ") {
			waits++
		} else {
			runs++
		}
	}
	if waits == 0 || runs == 0 {
		t.Fatalf("palette must mark waiting commands while busy: %d wait, %d run", waits, runs)
	}
	m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m.input.SetValue("/menu")
	m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if m.mode != modeMenu {
		t.Fatalf("/menu did not open while busy: mode %v", m.mode)
	}
}

func TestQuitWhileBusyCancelsTheRun(t *testing.T) {
	m := busyModel(t)
	cancelled := false
	m.cancelFn = func() { cancelled = true }
	m.input.SetValue("/quit")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !cancelled {
		t.Fatal("/quit during a run must cancel it first")
	}
	if cmd == nil {
		t.Fatal("/quit must quit")
	}
}

func TestTransientNoticeShowsAboveTheInputThenExpires(t *testing.T) {
	m := newTestModel(t)
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	now := time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	m.Update(transientMsg("waiting for backend: no tokens for 45s"))
	v := m.View()
	if !strings.Contains(v, "waiting for backend") {
		t.Fatalf("notice not shown:\n%s", v)
	}
	if strings.Contains(m.rendered.String(), "waiting for backend") {
		t.Fatal("transient notices must not enter the transcript")
	}
	lines := strings.Split(v, "\n")
	row := -1
	for i, l := range lines {
		if strings.Contains(l, "waiting for backend") {
			row = i
		}
	}
	first := m.headerHeight() + m.vp.Height - 1 // last transcript row (0-based)
	if row != first {
		t.Fatalf("notice on row %d, want the last transcript row %d", row, first)
	}
	now = now.Add(21 * time.Second)
	m.Update(toastTickMsg(now))
	if strings.Contains(m.View(), "waiting for backend") {
		t.Fatal("notice did not expire")
	}
	m.Update(transientMsg("context at 100 of 200 tokens"))
	now = now.Add(10 * time.Second)
	m.Update(transientMsg("compacted to 90 tokens"))
	now = now.Add(15 * time.Second)
	m.Update(toastTickMsg(now))
	if !strings.Contains(m.View(), "compacted to 90") {
		t.Fatal("a newer notice must restart the 20 s clock")
	}
}
