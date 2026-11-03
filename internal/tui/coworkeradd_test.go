package tui

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

func stubCoworkerModels(t *testing.T) {
	t.Helper()
	old := coworkerModels
	coworkerModels = func(v *View, name string) ([]provider.ModelDetail, error) {
		if name != "far" {
			return nil, errors.New("listing refused")
		}
		return []provider.ModelDetail{{ID: "claude-x"}, {ID: "m2"}}, nil
	}
	t.Cleanup(func() { coworkerModels = old })
}

func TestCoworkerAddFlowFromTheMenuRow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubCoworkerModels(t)
	v := newTestModel(t)
	v.cfg.Providers["far"] = config.ProviderConfig{Type: "openai", BaseURL: "https://api.example.com/v1"}
	if err := v.cfg.Save(); err != nil {
		t.Fatal(err)
	}
	typeLine(v, "/coworkers add")
	if v.coworkerAdd == nil || v.mode != modePicker {
		t.Fatalf("flow not open: %+v mode %v", v.coworkerAdd, v.mode)
	}
	if got := strings.Join(pickerIDs(v), ","); !strings.Contains(got, "far") || !strings.Contains(got, "ollama") {
		t.Fatalf("providers %v", got)
	}
	pickRowCW(t, v, "far")
	runCmd(t, v, v.lastCmd) // the model picker's load
	if got := pickerIDs(v); strings.Join(got, ",") != "claude-x,m2" {
		t.Fatalf("models %v", got)
	}
	pickRowCW(t, v, "claude-x")
	if v.coworkerAdd.step != cwStepName {
		t.Fatalf("step %v", v.coworkerAdd.step)
	}
	flush(v)
	if !strings.Contains(v.transcriptText(), "name for this co-worker (Enter keeps claude-x):") {
		t.Fatalf("%s", v.transcriptText())
	}
	typeLine(v, "")    // empty line: nothing happens (textarea empty-check)
	typeLine(v, "big") // the name
	if v.coworkerAdd == nil || v.coworkerAdd.step != cwStepSubAgent || v.mode != modePicker {
		t.Fatalf("after name: %+v", v.coworkerAdd)
	}
	pickRowCW(t, v, "no")
	if v.coworkerAdd != nil {
		t.Fatal("flow still open")
	}
	flush(v)
	if !strings.Contains(v.transcriptText(), "co-worker big added (far/claude-x)") {
		t.Fatalf("%s", v.transcriptText())
	}
	cws := v.ag.Coworkers()
	if len(cws) != 1 || cws[0].Name != "big" || !cws[0].Online || cws[0].SubAgent {
		t.Fatalf("%+v", cws)
	}
	if _, ok := v.ag.Tools.Tool("consult"); !ok {
		t.Fatal("consult not registered")
	}
	b, _ := os.ReadFile(filepath.Join(home, ".be-code", "config.json"))
	if !strings.Contains(string(b), `"big"`) {
		t.Fatal("config not saved")
	}
	if strings.Contains(strings.Join(v.histFile.lines, "\n"), "big") {
		t.Fatal("the flow's line reached the history")
	}
}

func TestCoworkerAddEscAndBusy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubCoworkerModels(t)
	v := newTestModel(t)
	v.cfg.Providers["far"] = config.ProviderConfig{Type: "openai", BaseURL: "https://api.example.com/v1"}
	v.mu.Lock()
	v.running = true
	v.mu.Unlock()
	typeLine(v, "/coworkers add")
	if v.coworkerAdd != nil {
		t.Fatal("opened while busy")
	}
	v.mu.Lock()
	v.running = false
	v.mu.Unlock()
	_, _ = v.startCoworkerAdd()
	v.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if v.coworkerAdd != nil || v.ag.HasCoworkers() {
		t.Fatal("esc did not cancel")
	}
	flush(v)
	if !strings.Contains(v.transcriptText(), "cancelled; nothing was added") {
		t.Fatalf("%s", v.transcriptText())
	}
}

// pickRowCW answers the open picker with the row id and runs the Cmd.
func pickRowCW(t *testing.T, v *View, id string) {
	t.Helper()
	if v.mode != modePicker || v.picker == nil {
		t.Fatalf("not in a picker (mode %v)", v.mode)
	}
	for _, it := range v.picker.items {
		if it.id == id {
			_, cmd := v.picker.onPick(v, it)
			if cmd != nil {
				runCmd(t, v, cmd)
			}
			return
		}
	}
	t.Fatalf("picker has no row %q", id)
}
