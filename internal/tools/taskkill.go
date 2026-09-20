package tools

import "strconv"

// taskkillArgs is the taskkill invocation that ends a process and everything
// it started: /T walks the tree, /F forces (a console child has no window to
// receive a polite close), /PID names the root. Pure, and in an untagged file,
// so the one thing about the Windows teardown that can be checked anywhere is.
func taskkillArgs(pid int) []string {
	return []string{"/T", "/F", "/PID", strconv.Itoa(pid)}
}
