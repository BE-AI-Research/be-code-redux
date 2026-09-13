package cmd

import "testing"

func TestInitCommandIsRegistered(t *testing.T) {
	for _, c := range rootCmd.Commands() {
		if c.Name() == "init" {
			return
		}
	}
	t.Fatal("init command not registered")
}

func TestInitApproveHonoursAskYesNo(t *testing.T) {
	oldAsk, oldTTY := askYesNo, stdinIsTTY
	defer func() { askYesNo, stdinIsTTY = oldAsk, oldTTY }()
	stdinIsTTY = func() bool { return true } // interactive: initApprove must ask
	asked := ""
	askYesNo = func(prompt string) bool { asked = prompt; return false }
	flagYes = false
	if initApprove("preview") {
		t.Fatal("a 'no' must reject the write")
	}
	if asked != "write BECODE.md? [y/N] " {
		t.Fatalf("prompt %q", asked)
	}
	flagYes = true
	if !initApprove("preview") {
		t.Fatal("-y must approve without asking")
	}
	flagYes = false
}

func TestInitApproveDeniesOnNonInteractiveStdinWithoutY(t *testing.T) {
	oldAsk, oldTTY := askYesNo, stdinIsTTY
	defer func() { askYesNo, stdinIsTTY = oldAsk, oldTTY }()
	asked := false
	askYesNo = func(string) bool { asked = true; return true }
	stdinIsTTY = func() bool { return false }
	flagYes = false
	if initApprove("preview") {
		t.Fatal("non-interactive stdin without -y must deny")
	}
	if asked {
		t.Fatal("must not read stdin when denying non-interactively")
	}
}
