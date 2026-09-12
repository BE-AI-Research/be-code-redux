//go:build !windows

package live

import "syscall"

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil || syscall.Kill(pid, 0) == syscall.EPERM
}
