package ui

import (
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
)

// A setting saved from inside a session never carries the session's own
// approvals: the "a" answer sets AutoApproveShell on the session's Config
// under a key the file has, and /update check used to write it to disk.
func TestUpdateCheckCommandLeavesSessionApprovalsOut(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	cfg := config.Default()
	cfg.AutoApproveShell = true
	cfg.ApproveFileWrites = false
	if msg := UpdateCheckCommand(cfg, "off"); msg == "" {
		t.Fatal("no message")
	}
	saved, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.UpdateCheck == nil || *saved.UpdateCheck {
		t.Fatal("update_check off was not saved")
	}
	if saved.AutoApproveShell || !saved.ApproveFileWrites {
		t.Fatal("session-only approvals were written to config.json")
	}
	if cfg.UpdateCheck == nil || *cfg.UpdateCheck {
		t.Fatal("the session's own copy was not changed")
	}
}
