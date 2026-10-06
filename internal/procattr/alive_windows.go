//go:build windows

package procattr

import "syscall"

// processQueryLimitedInformation is PROCESS_QUERY_LIMITED_INFORMATION, which
// Windows grants across integrity levels for the same user — unlike the
// PROCESS_QUERY_INFORMATION os.FindProcess asks for, which a non-elevated
// process is refused on an elevated one. Not exported by syscall.
const processQueryLimitedInformation = 0x1000

// Alive reports whether pid is a running process. Used for every "is this
// session host / editor still there" check on Windows: a wrong "no" there
// deletes a live session's record and starts a second host on a copy.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return aliveAfterOpenError(err)
	}
	defer syscall.CloseHandle(h)
	var code uint32
	if err := syscall.GetExitCodeProcess(h, &code); err != nil {
		return true // opened but unreadable: it exists; never call a live host dead
	}
	return aliveFromExitCode(code)
}
