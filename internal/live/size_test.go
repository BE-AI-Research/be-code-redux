package live

import (
	"errors"
	"testing"
)

// On Windows a console's size can only be read from an *output* handle:
// GetConsoleScreenBufferInfo on stdin fails, so a client that asked stdin
// reported the 80x24 fallback for ever and the session drew itself in the
// top-left corner of whatever window it was really in. Unix answers on any of
// the terminal's descriptors, so asking stdout first costs nothing there.
func TestTerminalSizeAsksTheOutputHandleFirst(t *testing.T) {
	const stdin, stdout, stderr = 0, 1, 2
	windows := func(fd uintptr) (int, int, error) {
		if fd == stdin {
			return 0, 0, errors.New("The handle is invalid.")
		}
		return 171, 44, nil
	}
	if c, r := terminalSize(windows, stdout, stderr, stdin); c != 171 || r != 44 {
		t.Fatalf("got %dx%d, want the real 171x44", c, r)
	}
	// stdout redirected to a file: the next descriptor that is a terminal answers.
	redirected := func(fd uintptr) (int, int, error) {
		if fd == stderr {
			return 120, 30, nil
		}
		return 0, 0, errors.New("not a terminal")
	}
	if c, r := terminalSize(redirected, stdout, stderr, stdin); c != 120 || r != 30 {
		t.Fatalf("got %dx%d, want 120x30 from stderr", c, r)
	}
	// Nothing is a terminal, or one answers with zeroes: the fallback, never 0x0.
	none := func(uintptr) (int, int, error) { return 0, 0, nil }
	if c, r := terminalSize(none, stdout, stderr, stdin); c != 80 || r != 24 {
		t.Fatalf("got %dx%d, want the 80x24 fallback", c, r)
	}
}
