//go:build !windows

package live

// A POSIX terminal needs no mode or code-page setup to render a frame: the
// escape sequences work as written and the character set comes from the
// locale. Only Windows has a console to take ownership of (console_windows.go).

func prepareTerminal(uintptr) (restore func(), utf8 bool) {
	return func() {}, localeUTF8()
}

