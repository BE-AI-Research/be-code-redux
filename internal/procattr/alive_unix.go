//go:build !windows

package procattr

import "syscall"

// Alive reports whether pid is a running process. Used for every "is this
// session host / editor still there" check on Unix. EPERM counts as alive:
// the process exists and belongs to another user, the same rule Alive
// applies on Windows to "access denied". A wrong "no" there deletes a live
// session's record and starts a second host on a copy.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
