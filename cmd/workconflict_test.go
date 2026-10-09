package cmd

import (
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
)

// -y means "run the commands I chose", not "discard the other session's
// work". With nobody to ask, workledger.Resolve writes after keeping the old
// version — it never needs an answer, so there is nothing for -y to give.
func TestHeadlessNeverApprovesWorkConflict(t *testing.T) {
	cfg := config.Default()
	cfg.AutoApproveShell = true
	cfg.AutoApproveBrowser = true

	var ok bool
	out := captureStderr(t, func() {
		ok = headlessApprover(cfg)("work_conflict", "doc.md changed since this session read it")
	})
	if ok {
		t.Fatal("-y approved work_conflict")
	}
	if !strings.Contains(out, "refused work_conflict") {
		t.Errorf("work_conflict was refused by the wrong branch:\n%s", out)
	}
}

func TestHeadlessNonInteractiveWorkConflictDoesNotPromiseYes(t *testing.T) {
	cfg := config.Default()
	saved := stdinIsTTY
	stdinIsTTY = func() bool { return false }
	defer func() { stdinIsTTY = saved }()

	out := captureStderr(t, func() {
		headlessApprover(cfg)("work_conflict", "doc.md changed since this session read it")
	})
	if strings.Contains(out, "use -y") {
		t.Errorf("work_conflict always asks a person, so the hint must not promise -y:\n%s", out)
	}
}
