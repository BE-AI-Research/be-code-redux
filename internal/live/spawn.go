package live

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"
)

// WaitForSocket polls for the host's socket file, which the host creates in
// Listen. Returns an error when it has not appeared within timeout — the
// launcher then has a spawned process that never came up, and says so
// instead of dialing a socket that will never exist.
func WaitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("live: host did not start (no socket at %s within %s)", path, timeout)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// NewToken returns a fresh 192-bit hex token. Clients must present it in
// their hello frame, so it is the only thing standing between a live session
// and any other local process that can reach the socket.
func NewToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read never returns an error on the platforms we
		// build for; a token we cannot trust must not be used as one.
		panic("live: cannot read random bytes: " + err.Error())
	}
	return hex.EncodeToString(b)
}
