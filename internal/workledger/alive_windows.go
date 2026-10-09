//go:build windows

package workledger

import "github.com/brown-enterprises/be-code/internal/procattr"

// processAlive asks procattr.Alive, which uses
// PROCESS_QUERY_LIMITED_INFORMATION and counts "access denied" as alive —
// never os.FindProcess, which asks for a privilege a non-elevated terminal
// is refused for an elevated session's process. live.List once deleted a live
// record that way and the launch forked a copy onto a copy of the session.
func processAlive(pid int) bool { return procattr.Alive(pid) }
