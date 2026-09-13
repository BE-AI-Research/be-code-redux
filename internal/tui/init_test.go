package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestInitHintShownOnceForAProjectWithoutNotes(t *testing.T) {
	m := newTestModel(t)
	os.WriteFile(filepath.Join(m.ag.Tools.Root, "go.mod"), []byte("module x\n"), 0o644)
	m.Init()
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	if strings.Count(m.transcript.String(), "no BECODE.md; /init maps this project") != 1 {
		t.Fatalf("hint:\n%s", m.transcript.String())
	}
	m.Update(tea.WindowSizeMsg{Width: 90, Height: 30})
	if strings.Count(m.transcript.String(), "no BECODE.md") != 1 {
		t.Fatal("hint repeated")
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
