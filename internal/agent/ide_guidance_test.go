package agent

import (
	"strings"
	"testing"
)

func TestEditorLabelNamesTheEditorNotItsIdentifier(t *testing.T) {
	for id, want := range map[string]string{
		"": "VS Code", "ide": "VS Code", "vscode": "VS Code",
		"visualstudio": "Visual Studio", "zed": "zed",
	} {
		if got := EditorLabel(id); got != want {
			t.Errorf("EditorLabel(%q) = %q, want %q", id, got, want)
		}
	}
}

// Visual Studio refuses debug_start's program form, and its tool description
// says so; guidance that still recommended it would cost a failed call.
func TestVisualStudioGuidanceDoesNotRecommendTheProgramForm(t *testing.T) {
	vs := IDEGuidanceFor("visualstudio")
	if strings.Contains(vs, "program") {
		t.Fatalf("Visual Studio guidance still mentions the program form:\n%s", vs)
	}
	if !strings.Contains(vs, "ide_debug_configs") {
		t.Fatalf("Visual Studio guidance should point at ide_debug_configs:\n%s", vs)
	}
	if IDEGuidanceFor("vscode") != IDEGuidance || IDEGuidanceFor("") != IDEGuidance {
		t.Fatal("any other editor keeps the VS Code guidance unchanged")
	}
}
