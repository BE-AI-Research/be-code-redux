//go:build !windows

package live

import "syscall"

func syscallUmask(m int) int { return syscall.Umask(m) }
