package setup

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/charmbracelet/x/term"
)

// The terminal seams ReadSecret uses, swapped by the tests.
var (
	secretGetState               = term.GetState
	secretRestore                = term.Restore
	secretReadPassword           = term.ReadPassword
	secretNotify                 = func(c chan<- os.Signal) { signal.Notify(c, os.Interrupt, syscall.SIGTERM) }
	secretStopNotify             = func(c chan<- os.Signal) { signal.Stop(c) }
	secretExit                   = func() { os.Exit(130) }
	secretOut          io.Writer = os.Stderr
)

// ReadSecret is term.ReadPassword that never leaves the terminal with echo
// off. ReadPassword turns echo off and keeps signals on, so Ctrl-C there
// would kill the process with the terminal still not echoing — the
// standalone `be-code online add` and the wizard run with no signal handler
// of their own. While the read is in progress an interrupt restores the
// saved state, says nothing was stored (nothing is written before the key
// is validated, spec §3) and exits 130, as the shell would have.
func ReadSecret(fd uintptr) ([]byte, error) {
	st, err := secretGetState(fd)
	if err != nil {
		return secretReadPassword(fd)
	}
	sig := make(chan os.Signal, 1)
	secretNotify(sig)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		select {
		case <-sig:
			_ = secretRestore(fd, st)
			fmt.Fprintln(secretOut, "\ncancelled; nothing was stored")
			secretExit()
		case <-done:
		}
	}()
	defer func() {
		secretStopNotify(sig)
		close(done)
		<-finished
	}()
	return secretReadPassword(fd)
}
