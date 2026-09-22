//go:build !windows

package inbox

import (
	"os"
	"syscall"
)

// withLock runs fn holding an exclusive flock on path+".lock": two session
// hosts binding IDs at once must not overwrite each other's write.
func withLock(path string, fn func() error) error {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}
