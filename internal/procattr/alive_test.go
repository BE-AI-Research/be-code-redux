package procattr

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
)

// A process another integrity level owns (an elevated SSH login's session
// host, seen from a non-elevated Visual Studio terminal) answers "access
// denied": that is a running process, never a dead one. Counting it dead
// deleted a live session's record and started a second host on a copy.
func TestAccessDeniedMeansAlive(t *testing.T) {
	if !aliveAfterOpenError(syscall.Errno(errorAccessDenied)) {
		t.Fatal("access denied must count as alive")
	}
	if !aliveAfterOpenError(fmt.Errorf("OpenProcess: %w", syscall.Errno(errorAccessDenied))) {
		t.Fatal("a wrapped access denied must count as alive")
	}
	if aliveAfterOpenError(syscall.Errno(errorInvalidParameter)) {
		t.Fatal("no such process (invalid parameter) is dead")
	}
	if aliveAfterOpenError(errors.New("something else")) {
		t.Fatal("an unknown failure to open is not evidence of life")
	}
}

// A handle can outlive its process; only STILL_ACTIVE is running.
func TestExitCodeStillActive(t *testing.T) {
	if !aliveFromExitCode(stillActive) {
		t.Fatal("STILL_ACTIVE is running")
	}
	if aliveFromExitCode(0) || aliveFromExitCode(1) {
		t.Fatal("an exit code means the process has exited")
	}
}
