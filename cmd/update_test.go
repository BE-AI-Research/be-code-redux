package cmd

import (
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
)

func TestHeadlessApproverRefusesUpdate(t *testing.T) {
	cfg := config.Default()
	cfg.AutoApproveBrowser, cfg.AutoApproveShell = true, true
	if headlessApprover(cfg)("update", "Update BE-Code 1.0.0 → 9.9.9?") {
		t.Fatal("-y answered update")
	}
	oldTTY := stdinIsTTY
	stdinIsTTY = func() bool { return false }
	t.Cleanup(func() { stdinIsTTY = oldTTY })
	out := captureStderr(t, func() {
		if headlessApprover(config.Default())("update", "x") {
			t.Error("non-interactive approved update")
		}
	})
	if !strings.Contains(out, "this action always asks a person") {
		t.Fatalf("message:\n%s", out)
	}
}
