package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/setup"
)

// stubOnline makes the network seams deterministic: validation accepts
// "good", sign-in returns "sk-or-test".
func stubOnline(t *testing.T) {
	t.Helper()
	ov, os_, ob := onlineValidate, onlineSignIn, onlineOpenBrowser
	onlineValidate = func(ctx context.Context, ch setup.Choice, key string) ([]provider.ModelInfo, error) {
		if strings.TrimSpace(key) != "good" && key != "sk-or-test" {
			return nil, setup.ErrKeyRejected
		}
		return []provider.ModelInfo{{ID: "m1"}, {ID: "m2"}}, nil
	}
	onlineSignIn = func(ctx context.Context, o signinOptions) (string, error) { return "sk-or-test", nil }
	onlineOpenBrowser = func(string) error { t.Fatal("the browser must never be launched in a test"); return nil }
	t.Cleanup(func() { onlineValidate, onlineSignIn, onlineOpenBrowser = ov, os_, ob })
}

// pickRow answers the open picker with the row id. The Cmd the pick
// returns is run only while the flow is still open: the last pick ends in
// setProvider, whose ping reaches the network.
func pickRow(t *testing.T, v *View, id string) {
	t.Helper()
	if v.mode != modePicker || v.picker == nil {
		t.Fatalf("not in a picker (mode %v)", v.mode)
	}
	for _, it := range v.picker.items {
		if it.id == id {
			_, cmd := v.picker.onPick(v, it)
			if cmd != nil && v.onlineAdd != nil {
				runCmd(t, v, cmd)
			}
			return
		}
	}
	t.Fatalf("picker has no row %q", id)
}

// runCmd executes a Cmd and feeds its message back through Update, once
// (each Cmd of a batch, once).
func runCmd(t *testing.T, v *View, cmd tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return
	}
	msg := cmd()
	if b, ok := msg.(tea.BatchMsg); ok {
		for _, c := range b {
			runCmd(t, v, c)
		}
		return
	}
	if msg != nil {
		v.Update(msg)
	}
}

func typeLine(v *View, s string) {
	v.input.SetValue(s)
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})
}

func TestOnlineAddPasteFlowWritesBothFilesAndSwitches(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubOnline(t)
	v := newTestModel(t)
	if err := v.cfg.Save(); err != nil {
		t.Fatal(err)
	}
	typeLine(v, "/online add")
	runCmd(t, v, v.lastCmd) // the preset picker's load Cmd
	pickRow(t, v, "groq")
	if v.onlineAdd == nil || v.onlineAdd.step != stepKey {
		t.Fatalf("after preset: %+v", v.onlineAdd)
	}
	// The masked field replaces the textarea.
	if !strings.Contains(v.inputView(), "key for groq") {
		t.Fatalf("input row is not the masked field: %q", v.inputView())
	}
	v.onlineAdd.secret.SetValue("good")
	if strings.Contains(v.inputView(), "good") {
		t.Fatal("the masked field shows the key")
	}
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	// Validation runs on a Cmd; drive it.
	runCmd(t, v, v.lastCmd)
	if v.onlineAdd.step != stepModel {
		t.Fatalf("after validate: step %v", v.onlineAdd.step)
	}
	pickRow(t, v, "m2")
	if v.onlineAdd.step != stepDefault {
		t.Fatalf("after model: step %v", v.onlineAdd.step)
	}
	pickRow(t, v, "no")
	if v.onlineAdd != nil {
		t.Fatal("flow still open")
	}
	flush(v) // the picks ran outside Update; render what they appended
	tr := v.transcriptText()
	if strings.Contains(tr, "good") {
		t.Fatal("the key reached the transcript")
	}
	for _, want := range []string{"key entered (hidden)", "groq added (model m2); switching to it"} {
		if !strings.Contains(tr, want) {
			t.Fatalf("transcript lacks %q:\n%s", want, tr)
		}
	}
	re, err := config.Load()
	if err != nil || re.Providers["groq"].DefaultModel != "m2" || re.DefaultProvider == "groq" {
		t.Fatalf("config: %+v %v", re.Providers["groq"], err)
	}
	if re.ProviderKey("groq") != "good" {
		t.Fatal("credential not stored")
	}
	if v.ag.CurrentProvider() != "groq" {
		t.Fatalf("session not switched: %s", v.ag.CurrentProvider())
	}
	// The history store is written to disk at exit; what it holds in
	// memory is what would be written.
	hist := strings.Join(v.histFile.lines, "\n")
	if strings.Contains(hist, "good") {
		t.Fatal("the key reached the input history")
	}
	if !strings.Contains(hist, "/online add") {
		t.Fatalf("the input history holds nothing (%q), so the check above proves nothing", hist)
	}
}

func TestOnlineAddRejectedKeyReturnsToKeyStep(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubOnline(t)
	v := newTestModel(t)
	_, _ = v.startOnlineAdd("groq")
	v.onlineAdd.secret.SetValue("bad")
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	runCmd(t, v, v.lastCmd)
	if v.onlineAdd == nil || v.onlineAdd.step != stepKey {
		t.Fatalf("want the key step again, got %+v", v.onlineAdd)
	}
	if !strings.Contains(v.transcriptText(), "key rejected by groq") {
		t.Fatalf("no refusal line: %s", v.transcriptText())
	}
	if strings.Contains(v.transcriptText(), "bad") {
		t.Fatal("the rejected key reached the transcript")
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".be-code", "credentials.json")); !os.IsNotExist(err) {
		t.Fatal("a rejected key was stored")
	}
}

func TestOnlineAddEscLeavesNoFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubOnline(t)
	v := newTestModel(t)
	_, _ = v.startOnlineAdd("groq")
	v.onlineAdd.secret.SetValue("good")
	v.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if v.onlineAdd != nil || v.mode != modeInput {
		t.Fatalf("flow not cancelled: %+v mode %v", v.onlineAdd, v.mode)
	}
	if _, err := os.Stat(filepath.Join(home, ".be-code", "credentials.json")); !os.IsNotExist(err) {
		t.Fatal("esc left a credential file")
	}
	if _, err := os.Stat(filepath.Join(home, ".be-code", "config.json")); !os.IsNotExist(err) {
		t.Fatal("esc left a config file")
	}
	if !strings.Contains(v.transcriptText(), "cancelled; nothing was stored") {
		t.Fatalf("no cancel line: %s", v.transcriptText())
	}
}

func TestOnlineAddEscOnAPickerCancelsTheFlow(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubOnline(t)
	v := newTestModel(t)
	_, cmd := v.startOnlineAdd("")
	runCmd(t, v, cmd)
	v.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if v.onlineAdd != nil || v.Session.onlineAdding || v.mode != modeInput {
		t.Fatalf("picker esc did not cancel: %+v mode %v", v.onlineAdd, v.mode)
	}
	if !strings.Contains(v.transcriptText(), "cancelled; nothing was stored") {
		t.Fatalf("no cancel line: %s", v.transcriptText())
	}
}

func TestOnlineAddRefusedWhileBusyAndWhenAnotherTerminalIsAdding(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubOnline(t)
	_, a, b := twoViews(t)
	a.mu.Lock()
	a.running = true
	a.mu.Unlock()
	typeLine(a, "/online add groq")
	if a.onlineAdd != nil || !strings.Contains(a.transcriptText(), "commands wait until the agent is done") {
		t.Fatalf("busy: %+v %s", a.onlineAdd, a.transcriptText())
	}
	a.mu.Lock()
	a.running = false
	a.mu.Unlock()
	_, _ = a.startOnlineAdd("groq")
	_, _ = b.startOnlineAdd("groq")
	flush(a, b)
	if b.onlineAdd != nil || !strings.Contains(b.transcriptText(), "another terminal is adding a provider") {
		t.Fatalf("second terminal: %+v %s", b.onlineAdd, b.transcriptText())
	}
}

func TestOnlineAddBrowserPathUsesSignIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubOnline(t)
	v := newTestModel(t)
	_, _ = v.startOnlineAdd("openrouter")
	if v.onlineAdd.step != stepMethod {
		t.Fatalf("openrouter must offer the method picker, got %v", v.onlineAdd.step)
	}
	pickRow(t, v, "browser") // runs the sign-in Cmd, which chains into validation
	runCmd(t, v, v.lastCmd)  // the validation Cmd
	runCmd(t, v, v.lastCmd)  // the model picker's load Cmd
	if v.onlineAdd == nil || v.onlineAdd.step != stepModel || v.onlineAdd.source != "oauth" {
		t.Fatalf("after sign-in: %+v", v.onlineAdd)
	}
}

func TestOnlineAddCustomEndpointAsksNameAndURL(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubOnline(t)
	v := newTestModel(t)
	_, cmd := v.startOnlineAdd("")
	runCmd(t, v, cmd)
	pickRow(t, v, "custom")
	flush(v)
	if v.onlineAdd.step != stepName {
		t.Fatalf("step %v", v.onlineAdd.step)
	}
	typeLine(v, "groq") // a preset's name is refused; stays on the name step
	if v.onlineAdd.step != stepName || !strings.Contains(v.transcriptText(), "is a preset") {
		t.Fatalf("preset name accepted: %+v", v.onlineAdd)
	}
	typeLine(v, "mine")
	if v.onlineAdd.step != stepURL {
		t.Fatalf("step %v", v.onlineAdd.step)
	}
	typeLine(v, "https://api.together.xyz/v1")
	if v.onlineAdd.step != stepKey || v.onlineAdd.choice.Name != "mine" {
		t.Fatalf("%+v", v.onlineAdd)
	}
	if hist := strings.Join(v.histFile.lines, "\n"); strings.Contains(hist, "together") || strings.Contains(hist, "mine") {
		t.Fatalf("a line typed into the flow reached the input history: %q", hist)
	}
}

func TestOnlineAddKeyStepKeepsTheLineWhenARunStarts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubOnline(t)
	v := newTestModel(t)
	_, _ = v.startOnlineAdd("groq")
	v.mode = modeBusy // another terminal started a run
	v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("good")})
	if v.input.Value() != "" || v.onlineAdd.secret.Value() != "good" {
		t.Fatalf("keys went to the textarea: input %q", v.input.Value())
	}
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if v.ag.Pending() != 0 {
		t.Fatal("the key was queued to the agent")
	}
	if v.onlineAdd == nil || v.onlineAdd.step != stepWait {
		t.Fatalf("enter did not reach the flow: %+v", v.onlineAdd)
	}
}

func TestOnlineAddStatusShowsOnThisTerminalsBottomLine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubOnline(t)
	const waiting, checking = "waiting for the browser; or paste the redirect URL", "checking groq…"

	v := newTestModel(t)
	_, _ = v.startOnlineAdd("openrouter")
	it := pickItem{id: "browser"}
	_, signIn := v.picker.onPick(v, it) // the sign-in Cmd, not yet run
	if !strings.Contains(v.bottomLine(), waiting) {
		t.Fatalf("bottom line lacks the browser hint: %q", v.bottomLine())
	}
	v.cancelOnlineAdd()
	_ = signIn
	if b := v.bottomLine(); strings.Contains(b, waiting) {
		t.Fatalf("status outlived the flow: %q", b)
	}

	w := newTestModel(t)
	_, _ = w.startOnlineAdd("groq")
	w.onlineAdd.secret.SetValue("good")
	w.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !strings.Contains(w.bottomLine(), checking) {
		t.Fatalf("bottom line lacks the check: %q", w.bottomLine())
	}
	if w.statusNote == checking {
		t.Fatal("the flow wrote the shared run status")
	}
	runCmd(t, w, w.lastCmd) // validation -> model picker
	pickRow(t, w, "m1")
	pickRow(t, w, "no") // ends the flow (its ping Cmd is not run)
	if b := w.bottomLine(); strings.Contains(b, checking) || strings.Contains(b, waiting) {
		t.Fatalf("status outlived the flow: %q", b)
	}
}

// I3 (final review): the context menu's Paste at the key step lands in the
// masked field, never the hidden textarea; and the textarea is empty on
// entering the key step and after the flow ends.
func TestOnlineAddContextMenuPasteGoesToTheMaskedField(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubOnline(t)
	v := newTestModel(t)
	v.input.SetValue("a draft typed before")
	_, _ = v.startOnlineAdd("groq")
	if v.onlineAdd == nil || v.onlineAdd.step != stepKey {
		t.Fatalf("not at the key step: %+v", v.onlineAdd)
	}
	if v.input.Value() != "" {
		t.Fatal("the textarea kept text under the masked field")
	}
	v.clipboardRead = func() (string, error) { return "sk-pasted-key", nil }
	_, _ = v.openContextMenu()
	if v.picker == nil {
		t.Fatal("no context menu")
	}
	for _, it := range v.picker.items {
		if it.id == "paste" {
			v.picker.onPick(v, it)
		}
	}
	if v.input.Value() != "" {
		t.Fatal("the pasted key landed in the hidden textarea")
	}
	if v.onlineAdd == nil || v.onlineAdd.secret.Value() != "sk-pasted-key" {
		t.Fatal("the pasted key is not in the masked field")
	}
	if strings.Contains(v.inputView(), "sk-pasted-key") {
		t.Fatal("the masked field shows the key")
	}
	v.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if v.onlineAdd != nil || v.input.Value() != "" {
		t.Fatal("after Esc the textarea is not empty")
	}
}
