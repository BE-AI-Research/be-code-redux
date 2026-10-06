//go:build windows

package live

import (
	"errors"
	"os"

	"github.com/brown-enterprises/be-code/internal/procattr"
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

// processAlive asks procattr.Alive, which tells a running process from an
// exited one and counts a process of another integrity level (an elevated SSH
// login's host seen from a non-elevated terminal) as alive rather than
// deleting its record.
func processAlive(pid int) bool { return procattr.Alive(pid) }
