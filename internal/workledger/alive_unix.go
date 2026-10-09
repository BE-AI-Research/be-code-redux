//go:build !windows

package workledger

import "syscall"

// processAlive reports whether pid is a running process. EPERM counts as
// alive: the process exists and belongs to another user, which is the same
// rule procattr.Alive applies to Windows' "access denied". internal/live and
// internal/ide each carry this pair for the same reason.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
