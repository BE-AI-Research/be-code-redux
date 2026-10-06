package live

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// detachedProcess is DETACHED_PROCESS: the child gets no console of its own
// and does not inherit this process's, so closing the launcher's terminal
// cannot take the host down with it. Not exported by syscall, hence the
// literal.
const detachedProcess = 0x00000008

// createBreakawayFromJob is CREATE_BREAKAWAY_FROM_JOB: the child leaves the
// job the launcher runs in. Windows' SSH server puts each session in a job
// that kills every process in it when the connection closes, so without this
// a session hosted from an SSH login died with the login, and the next launch
// resumed a copy from disk instead of joining it.
const createBreakawayFromJob = 0x01000000

// SpawnHost starts exe with arg plus extraArgs as a detached process: its own
// process group, no console, no stdin, and stdout/stderr appended to logPath.
func SpawnHost(exe, arg, logPath string, env []string, extraArgs ...string) (int, error) {
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer logf.Close()
	var cmd *exec.Cmd
	broke, err := startDetached(func(breakaway bool) error {
		cmd = exec.Command(exe, append([]string{arg}, extraArgs...)...)
		cmd.Env = env
		cmd.Stdin = nil
		cmd.Stdout, cmd.Stderr = logf, logf
		flags := uint32(syscall.CREATE_NEW_PROCESS_GROUP | detachedProcess)
		if breakaway {
			flags |= createBreakawayFromJob
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags}
		return cmd.Start()
	})
	if err != nil {
		return 0, err
	}
	if !broke {
		// The launcher's job forbids breaking away (Access is denied): the
		// host runs, but ends when this login does.
		fmt.Fprintln(os.Stderr, "note: this login does not let BE-Code outlive it; the shared session ends when you disconnect")
	}
	pid := cmd.Process.Pid
	go cmd.Wait() // reap if we are still around when it exits
	return pid, nil
}
