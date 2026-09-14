package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/agent"
)

func TestInitHintShownOnceForAProjectWithoutNotes(t *testing.T) {
	// The workspace has to look like a project before the first
	// WindowSizeMsg, which is where the hint is decided (and latched).
	m := newTestModel(t, func(ag *agent.Agent) {
		os.WriteFile(filepath.Join(ag.Tools.Root, "go.mod"), []byte("module x\n"), 0o644)
	})
	if strings.Count(m.rendered.String(), "no BECODE.md; /init maps this project") != 1 {
		t.Fatalf("hint:\n%s", m.rendered.String())
	}
	m.Update(tea.WindowSizeMsg{Width: 90, Height: 30})
	if strings.Count(m.rendered.String(), "no BECODE.md") != 1 {
		t.Fatal("hint repeated")
	}
}

// TestInitHintCheckIsLatched: a WindowSizeMsg arrives on every resize, so
// the *check* (four os.Stat calls in the workspace) must happen once, not
// only the hint it may print.
func TestInitHintCheckIsLatched(t *testing.T) {
	m := newTestModel(t) // empty dir: verify.Detect says "none", no hint
	if !m.initChecked {
		t.Fatal("the first size message must latch the check")
	}
	os.WriteFile(filepath.Join(m.ag.Tools.Root, "go.mod"), []byte("module x\n"), 0o644)
	m.Update(tea.WindowSizeMsg{Width: 90, Height: 30})
	if strings.Contains(m.rendered.String(), "no BECODE.md") {
		t.Fatal("the workspace was re-checked on a resize")
	}
}

func TestInitCommandRunsTheFlowNotATurn(t *testing.T) {
	tempHome(t)
	m := newTestModel(t)
	var ran string // any model turn this command starts, which must be none
	m.startTurnHook = func(text string) { ran = text }
	m.input.SetValue("/init")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("/init must return the running session's wheel tick")
	}
	// The flow runs on its own goroutine and always ends by putting its
	// outcome on the transcript — with the null provider, a failure. Waiting
	// for that is what makes the "no turn" assertion below meaningful (and
	// race-free) rather than a snapshot taken before anything happened.
	waitFor(t, func() bool {
		flush(m)
		if m.mode == modeAsk { // the write approval, if it ever gets that far
			m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
		}
		tr := m.rendered.String()
		return strings.Contains(tr, "init failed") || strings.Contains(tr, "wrote ")
	})
	m.mu.Lock()
	defer m.mu.Unlock()
	if ran != "" {
		t.Fatalf("/init started a model turn with %q", ran)
	}
	if m.running {
		t.Fatal("the init flow did not return the session to idle")
	}
}
