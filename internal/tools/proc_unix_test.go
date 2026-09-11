//go:build !windows

package tools

import (
	"context"
	"syscall"
	"testing"
	"time"
)

// A grandchild holding stdout must not keep RunShell alive past its
// timeout: the whole process group is killed.
func TestRunShellTimeoutKillsProcessGroup(t *testing.T) {
	start := time.Now()
	_, err := RunShell(context.Background(), t.TempDir(), "sleep 20 & sleep 20", 300*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("RunShell hung for %s after a 300ms timeout", el)
	}
}

// Stopping a background process must take its children with it.
func TestProcessStopKillsGroup(t *testing.T) {
	m := &ProcessManager{procs: map[int]*bgProc{}}
	id, err := m.Start(t.TempDir(), "sleep 30 & sleep 30")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := m.get(id)
	pid := p.cmd.Process.Pid
	time.Sleep(100 * time.Millisecond)
	if err := m.Stop(id); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	// With Setpgid the shell's pid is the group id; signal 0 to the group
	// reports whether any member is still alive.
	if err := syscall.Kill(-pid, 0); err == nil {
		t.Fatal("process group still alive after Stop")
	}
}
