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
	m := newTestModel(t)
	started := false
	m.startTurnHook = func(string) { started = true }
	m.inputFor(0).SetValue("/init")
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if started || cmd == nil {
		t.Fatal("/init must run the init flow as a command, not start a model turn")
	}
}
