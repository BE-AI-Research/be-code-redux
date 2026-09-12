//go:build !windows

package live

import "syscall"

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil || syscall.Kill(pid, 0) == syscall.EPERM
}

// Kill stops the process outright. It is the escalation behind Terminate for
// a host that has not gone after being asked twice (see `be-code sessions
// kill`): no defers run, so the handoff briefing is lost — which is still
// better than reporting a session killed while it holds the workspace.
func Kill(pid int) error {
	if pid <= 0 {
		return syscall.ESRCH
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}

// Terminate asks the process to stop. It is the last resort behind a polite
// quit frame (see `be-code sessions kill`), so it uses SIGTERM rather than
// SIGKILL: a host that is merely slow still gets to run its deferred saves.
func Terminate(pid int) error {
	if pid <= 0 {
		return syscall.ESRCH
	}
	return syscall.Kill(pid, syscall.SIGTERM)
}
