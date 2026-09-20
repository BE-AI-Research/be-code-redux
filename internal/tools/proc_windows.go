//go:build windows

package tools

import (
	"os/exec"

	"github.com/brown-enterprises/be-code/internal/procattr"
)

// setProcessGroup has no process group to set on Windows; what every child of
// the shell and process tools needs there is not to open a console window of
// its own (see procattr.Hide).
func setProcessGroup(cmd *exec.Cmd) { procattr.Hide(cmd) }

func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}
