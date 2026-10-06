//go:build windows

package ide

import "github.com/brown-enterprises/be-code/internal/procattr"

// processAlive asks procattr.Alive: an editor of another integrity level is
// still running, and its lock must not be pruned as dead.
func processAlive(pid int) bool { return procattr.Alive(pid) }
