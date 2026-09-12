//go:build !windows

package live

import (
	"os"
	"os/exec"
	"syscall"
)

// SpawnHost starts exe with arg plus extraArgs as a detached process: its own
// session (Setsid), no stdin, and stdout/stderr appended to logPath. The
// child outlives this process, which is the point — the launcher attaches to
// it over the socket and may come and go, or be a terminal that gets closed.
func SpawnHost(exe, arg, logPath string, env []string, extraArgs ...string) (int, error) {
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	cmd := exec.Command(exe, append([]string{arg}, extraArgs...)...)
	cmd.Env = env
	cmd.Stdin = nil
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	go cmd.Wait() // reap if we are still around when it exits
	return pid, nil
}
