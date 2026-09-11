//go:build !windows

package tools

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the child in its own process group so a timeout or
// Stop can take its grandchildren (dev servers, `sleep &`) with it.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup kills the child's whole group; falls back to the child.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err == nil {
		return nil
	}
	return cmd.Process.Kill()
}
