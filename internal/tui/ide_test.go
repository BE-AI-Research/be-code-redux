package tui

import (
	"strings"
	"testing"
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
