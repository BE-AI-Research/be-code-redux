package live

import (
	"path/filepath"
	"runtime"
	"strings"
)

// SameWorkspace reports whether two workspace paths name one folder, so a
// launch joins the live session already serving it. On Windows the same
// folder reaches BE-Code spelled differently from different shells —
// Visual Studio's terminal, an SSH session, a drive letter in either case,
// an 8.3 short name — and comparing the text started a second host on a
// copy of the session instead of joining the first.
func SameWorkspace(a, b string) bool { return sameWorkspace(a, b, runtime.GOOS == "windows") }

func sameWorkspace(a, b string, foldCase bool) bool {
	eq := func(x, y string) bool {
		x, y = filepath.Clean(x), filepath.Clean(y)
		if foldCase {
			return strings.EqualFold(x, y)
		}
		return x == y
	}
	if eq(a, b) {
		return true
	}
	// Only the file system can equate a link or a short name with the path
	// it stands for; a path that cannot be resolved compares as written.
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && eq(ra, rb)
}

// startDetached starts a host, first breaking away from the job the
// launcher runs in (an SSH session's, which kills everything it started
// when the connection closes) and, if the job forbids that, again without.
// broke says whether the host will outlive the launcher's session.
func startDetached(start func(breakaway bool) error) (broke bool, err error) {
	if err := start(true); err == nil {
		return true, nil
	}
	if err := start(false); err != nil {
		return false, err
	}
	return false, nil
}
