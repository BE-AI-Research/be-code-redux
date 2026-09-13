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
	old := askYesNo
	defer func() { askYesNo = old }()
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
