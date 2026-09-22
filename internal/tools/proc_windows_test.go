//go:build windows

package tools

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// alive reports whether a process with this pid exists.
func alive(pid int) bool {
	out, _ := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/NH").Output()
	return strings.Contains(string(out), strconv.Itoa(pid))
}

// A command that times out takes what it started down with it. Written on
// Linux and never run there: it needs real Windows processes.
func TestATimedOutCommandTakesItsChildrenWithIt(t *testing.T) {
	cmd := exec.Command("powershell", "-NoProfile", "-Command", "ping -n 120 127.0.0.1 | Out-Null")
	setProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()

	// Find the ping that powershell started.
	var child int
	deadline := time.Now().Add(15 * time.Second)
	for child == 0 && time.Now().Before(deadline) {
		q := "(Get-CimInstance Win32_Process -Filter 'ParentProcessId=" + strconv.Itoa(cmd.Process.Pid) + "' | Select-Object -First 1).ProcessId"
		out, _ := exec.CommandContext(context.Background(), "powershell", "-NoProfile", "-Command", q).Output()
		child, _ = strconv.Atoi(strings.TrimSpace(string(out)))
		if child == 0 {
			time.Sleep(300 * time.Millisecond)
		}
	}
	if child == 0 {
		t.Skip("could not find the grandchild to watch; nothing to assert")
	}

	if err := killProcessGroup(cmd); err != nil {
		t.Fatalf("killProcessGroup: %v", err)
	}
	cmd.Wait()
	for i := 0; i < 20 && alive(child); i++ {
		time.Sleep(250 * time.Millisecond)
	}
	if alive(child) {
		exec.Command("taskkill", "/F", "/PID", strconv.Itoa(child)).Run()
		t.Fatalf("the grandchild (pid %d) outlived the kill", child)
	}
}
