//go:build windows

package tools

import (
	"context"
	"os/exec"
	"time"

	"github.com/brown-enterprises/be-code/internal/procattr"
)

// setProcessGroup has no process group to set on Windows; what every child of
// the shell and process tools needs there is not to open a console window of
// its own (see procattr.Hide).
func setProcessGroup(cmd *exec.Cmd) { procattr.Hide(cmd) }

// taskkillTimeout bounds the teardown itself: a kill that hangs must not turn
// a timed-out command into a hung tool call.
const taskkillTimeout = 5 * time.Second

// killProcessGroup ends the child and everything it started. Windows has no
// process group to signal, and Process.Kill ends the direct child only — so a
// timed-out `powershell` used to leave the python, test runner or dev server
// it had started running, holding files and ports, with nothing left that
// knew about it. taskkill /T walks the tree. If it cannot be run, or fails,
// the direct child is still killed, which is what happened before.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), taskkillTimeout)
	defer cancel()
	k := exec.CommandContext(ctx, "taskkill", taskkillArgs(cmd.Process.Pid)...)
	procattr.Hide(k)
	if err := k.Run(); err == nil {
		return nil
	}
	return cmd.Process.Kill()
}
