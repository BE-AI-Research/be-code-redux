//go:build windows

package live

import (
	"errors"
	"os"
)

// Terminate stops the process. Windows has no SIGTERM, so this is the
// abrupt kill; it is the last resort behind a polite quit frame (see
// `be-code sessions kill`).
func Terminate(pid int) error {
	if pid <= 0 {
		return errors.New("live: no pid")
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	defer p.Release()
	return p.Kill()
}

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
