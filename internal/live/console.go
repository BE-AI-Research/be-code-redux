package live

import (
	"errors"
	"os"
	"strings"
)

// Windows console output flags and the UTF-8 code page, named here so the
// decidable half of the setup — prepareConsole — compiles and is tested on
// every platform. The bindings live in console_windows.go.
//
// Why a client has to do this at all: in a served session the Bubble Tea
// program runs in the detached host with no tty of its own (WithInput(nil),
// WithOutput(socket)), so the Windows console setup Bubble Tea does when it
// owns a terminal never runs, and x/term's MakeRaw only touches the *input*
// handle. Nothing else configures the client's console output. Left alone it
// keeps its OEM code page, so every non-ASCII byte of a frame is drawn as two
// or three cells; lines then overflow the width the renderer padded them to,
// the console wraps them, the alt screen scrolls, and each following
// cursor-home repaint lands out of step — text duplicating and degrading while
// the frame's shape survives.
const (
	vtProcessing = 0x0004 // ENABLE_VIRTUAL_TERMINAL_PROCESSING
	noAutoReturn = 0x0008 // DISABLE_NEWLINE_AUTO_RETURN: no wrap at the last cell
	cpUTF8       = 65001
)

// errNotConsole is what a console API reports for a handle that is not one
// (a redirected stdout, a pipe in a test).
var errNotConsole = errors.New("not a console")

// consoleAPI is the slice of the Windows console API a client needs to own
// its terminal's output for the length of an attach.
type consoleAPI interface {
	Mode(fd uintptr) (uint32, error)
	SetMode(fd uintptr, mode uint32) error
	OutputCP() uint32
	SetOutputCP(cp uint32) error
}

// prepareConsole takes ownership of one console's output for an attach: VT
// processing on (so the host's escape sequences mean anything), the last-cell
// auto-wrap off (a full-width line must not spill into a second row), and the
// output code page in UTF-8. It reports whether the console now renders UTF-8,
// and returns a restore that puts back exactly what was changed, innermost
// last — nothing at all for a handle that is not a console.
func prepareConsole(api consoleAPI, out uintptr) (restore func(), utf8 bool) {
	var undo []func()
	restore = func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}
	mode, err := api.Mode(out)
	if err != nil {
		// Not a console: there is nothing to own here, and no claim to make
		// about what it can draw.
		return restore, false
	}
	if want := mode | vtProcessing | noAutoReturn; want != mode {
		if api.SetMode(out, want) == nil {
			undo = append(undo, func() { api.SetMode(out, mode) })
		}
	}
	// A console already in UTF-8 is left alone: restoring a code page nobody
	// changed would be a change of its own.
	if cp := api.OutputCP(); cp == cpUTF8 {
		return restore, true
	} else if cp != 0 && api.SetOutputCP(cpUTF8) == nil {
		undo = append(undo, func() { api.SetOutputCP(cp) })
		return restore, true
	}
	return restore, false
}


// localeUTF8 is a POSIX terminal's own claim about its character set. Windows
// has no such variables, which is why it gets asked about its console instead.
func localeUTF8() bool {
	return strings.Contains(strings.ToLower(os.Getenv("LANG")+os.Getenv("LC_ALL")+os.Getenv("LC_CTYPE")), "utf")
}
