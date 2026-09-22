// Package procattr holds the one platform-specific thing every child process
// the harness starts has in common.
package procattr

import "os/exec"

// Hide keeps a child process from opening a console window of its own.
//
// It matters on Windows and only there. A hosted session runs in a detached
// process with no console (live.SpawnHost), and when a process that has no
// console starts a console program — powershell, git, go, python — Windows
// gives that program a brand-new, visible console window. Every tool call,
// every verification check and the git summary taken before each request
// flashed one open. With CREATE_NO_WINDOW the child still gets a console to
// write to, but it is never shown; the harness reads the child's output
// through pipes either way, so nothing is lost. Call it on every exec.Cmd
// before Start or Run; it preserves whatever SysProcAttr the command already
// carries. Elsewhere it does nothing.
func Hide(cmd *exec.Cmd) { hide(cmd) }
