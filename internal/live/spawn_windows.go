package live

import (
	"os"
	"os/exec"
	"syscall"
)

// detachedProcess is DETACHED_PROCESS: the child gets no console of its own
// and does not inherit this process's, so closing the launcher's terminal
// cannot take the host down with it. Not exported by syscall, hence the
// literal.
const detachedProcess = 0x00000008

// SpawnHost starts exe with arg plus extraArgs as a detached process: its own
// process group, no console, no stdin, and stdout/stderr appended to logPath.
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
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	go cmd.Wait() // reap if we are still around when it exits
	return pid, nil
}
