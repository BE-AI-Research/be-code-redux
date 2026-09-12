//go:build windows

package live

import "os"

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil || p == nil {
		return false
	}
	// FindProcess opens a process handle on Windows; release it so a
	// discovery sweep does not leak one handle per lock file examined.
	defer p.Release()
	return true
}
