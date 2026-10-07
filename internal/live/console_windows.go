package live

import "golang.org/x/sys/windows"

// winConsole binds consoleAPI to the real Windows console. Everything
// decidable lives in console.go, which is why this is the whole of it.
type winConsole struct{}

func (winConsole) Mode(fd uintptr) (uint32, error) {
	var mode uint32
	if err := windows.GetConsoleMode(windows.Handle(fd), &mode); err != nil {
		return 0, err
	}
	return mode, nil
}

func (winConsole) SetMode(fd uintptr, mode uint32) error {
	return windows.SetConsoleMode(windows.Handle(fd), mode)
}

func (winConsole) OutputCP() uint32 {
	cp, err := windows.GetConsoleOutputCP()
	if err != nil {
		return 0
	}
	return cp
}

func (winConsole) SetOutputCP(cp uint32) error { return windows.SetConsoleOutputCP(cp) }

// prepareTerminal is called with the client's output handle when it takes the
// terminal for an attach; the restore runs when the attach ends.
func prepareTerminal(out uintptr) (restore func(), utf8 bool) {
	return prepareConsole(winConsole{}, out)
}

