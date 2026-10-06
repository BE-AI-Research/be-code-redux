package procattr

import (
	"errors"
	"syscall"
)

// Windows error numbers and the exit code a running process reports. They are
// plain numbers here so the decision below is tested on every platform.
const (
	errorAccessDenied     = 5   // ERROR_ACCESS_DENIED
	errorInvalidParameter = 87  // ERROR_INVALID_PARAMETER: no such process
	stillActive           = 259 // STILL_ACTIVE
)

// aliveAfterOpenError decides from a failure to open a process whether it is
// running. "Access denied" means it exists but belongs to another integrity
// level (an elevated SSH login's process seen from a non-elevated terminal),
// so it is alive — as EPERM is on Unix. Anything else is not evidence of life.
func aliveAfterOpenError(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == errorAccessDenied
}

// aliveFromExitCode reports whether an opened process is still running: a
// handle can outlive its process, which then reports its real exit code.
func aliveFromExitCode(code uint32) bool { return code == stillActive }
