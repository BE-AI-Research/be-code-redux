package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/agent"
)

// The bottom line marks an attached editor with "⌘ ide", and a statusMsg
// updates the busy-state note (used to show "reviewing change in VS
// Code…" without writing a transcript line).
func TestIDEMarkerAndStatusMsg(t *testing.T) {
	m := newTestModel(t)
	m.ag.IDEName = "vscode"
	if !strings.Contains(m.View(), "⌘ ide") {
		t.Fatal("ide marker missing from bottom line")
	}
	m.mode = modeBusy
	m.running = true
	m.Update(statusMsg("reviewing change in VS Code…"))
	if !strings.Contains(m.View(), "reviewing change in VS Code") {
		t.Fatal("status not shown")
	}
}

// The "VS Code connected: N tools" line belongs in the transcript: stderr
// written before the alt screen opens is wiped by it, so the TUI reports
// the connection itself, dimmed, once it has the screen.
func TestIDEConnectedLineInTranscript(t *testing.T) {
	m := newTestModel(t, func(ag *agent.Agent) { ag.IDEName = "vscode"; ag.IDETools = 16 })
	if !strings.Contains(m.rendered.String(), "VS Code connected: 16 tools") {
		t.Fatalf("connection line missing from transcript:\n%s", m.rendered.String())
	}
	before := strings.Count(m.rendered.String(), "VS Code connected")
	m.Update(tea.WindowSizeMsg{Width: 100, Height: 40})
	if got := strings.Count(m.rendered.String(), "VS Code connected"); got != before {
		t.Fatalf("line repeated on a later resize: %d occurrences", got)
	}
}

// With no editor attached nothing is announced.
func TestNoIDENoConnectedLine(t *testing.T) {
	m := newTestModel(t)
	if strings.Contains(m.rendered.String(), "VS Code connected") {
		t.Fatal("connection line shown without an editor")
	}
}

// "a" on a file-write approval means "stop asking": it must silence the
// editor diff review too, not just the terminal prompt.
func TestApprovalAllDisablesEditorReviewToo(t *testing.T) {
	m := newTestModel(t)
	m.ag.Tools.ApproveWrites = true
	m.cfg.ApproveFileWrites = true
	resp := make(chan bool, 1)
	m.Update(approvalMsg{action: "file_write", detail: "a.go", resp: resp})
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}})
	if !<-resp {
		t.Fatal("approval not granted")
	}
	if m.cfg.ApproveFileWrites {
		t.Fatal("terminal file-write prompts still enabled")
	}
	if m.ag.Tools.ApproveWrites {
		t.Fatal("editor diff review still enabled after accept-all")
	}
}

// The per-turn editor context note is ambient, not a warning: it renders
// without the "note " label.
func TestEditorNoteRendersUnlabelled(t *testing.T) {
	m := newTestModel(t)
	m.Update(noticeMsg("[editor: a.go, cursor line 3]"))
	tr := m.rendered.String()
	if !strings.Contains(tr, "[editor: a.go, cursor line 3]") {
		t.Fatalf("note missing:\n%s", tr)
	}
	if strings.Contains(tr, "note [editor:") {
		t.Fatalf("editor note carries the warning label:\n%s", tr)
	}
	m.Update(noticeMsg("something else"))
	if !strings.Contains(m.rendered.String(), "note something else") {
		t.Fatal("ordinary notices lost their label")
	}
}
