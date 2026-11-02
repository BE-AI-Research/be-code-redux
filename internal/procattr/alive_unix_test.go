//go:build !windows

package procattr

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestAliveOnUnix(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Fatal("this process is alive")
	}
	if Alive(0) || Alive(-1) {
		t.Fatal("pid 0 and negative pids are never alive")
	}
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if !Alive(cmd.Process.Pid) {
		t.Fatal("a running child is alive")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	deadline := time.Now().Add(2 * time.Second)
	for Alive(cmd.Process.Pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if Alive(cmd.Process.Pid) {
		t.Fatal("a reaped child is not alive")
	}
}
