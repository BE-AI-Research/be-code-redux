//go:build windows

package procattr

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestHideSetsNoWindowAndKeepsExistingFlags(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "echo hi")
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
	Hide(cmd)
	if !cmd.SysProcAttr.HideWindow {
		t.Fatal("HideWindow not set")
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Fatal("CREATE_NO_WINDOW not set")
	}
	if cmd.SysProcAttr.CreationFlags&syscall.CREATE_NEW_PROCESS_GROUP == 0 {
		t.Fatal("an existing creation flag was lost")
	}
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		t.Fatalf("a hidden child must still run and be read: %q %v", out, err)
	}
}
