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

// Kill stops the process outright. On Windows Terminate is already the
// abrupt kill, so this is the same call; it exists so callers can escalate
// without build tags.
func Kill(pid int) error { return Terminate(pid) }

// processAlive is optimistic on Windows: OpenProcess succeeds for a pid that
// has exited but whose handle is still around, so a caller escalating on
// "still alive" may escalate once unnecessarily rather than miss a live host.
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
