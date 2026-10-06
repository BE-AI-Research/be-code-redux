//go:build windows

package procattr

import (
	"os"
	"os/exec"
	"testing"
)

func TestAliveOnWindows(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Fatal("this process is alive")
	}
	cmd := exec.Command("cmd", "/c", "exit", "0")
	Hide(cmd)
	if err := cmd.Run(); err != nil {
		t.Skip("cannot run cmd.exe:", err)
	}
	if Alive(cmd.Process.Pid) {
		t.Fatal("an exited child is not alive")
	}
	if Alive(0) || Alive(-1) {
		t.Fatal("no pid is alive")
	}
}
