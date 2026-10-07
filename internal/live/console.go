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
// handle. Nothing else configures the client's console output.
//
// wrapAtEOL is the one that produced the ghost rows. Every row of a frame is
// padded to exactly the width (padToWidth, and lipgloss pads its own), so the
// last cell of a row is always written — and Bubble Tea appends no
// EraseLineRight to a line that already fills the width, since it expects the
// write itself to cover the old cells. With wrap-at-EOL on, writing that last
// cell moves the cursor to the next row; on the bottom row that scrolls the
// alt screen, so the next cursor-home repaint paints one row out of step and
// whatever scrolled past the bottom is never painted over again. A frame is
// height-1 rows, so the bottom row of the terminal is not painted even in the
// steady state, and EraseScreenBelow only fires when a frame gets shorter —
// nothing ever cleans those rows up. Cleared, a full-width row leaves the
// cursor where it was and nothing scrolls.
const (
	wrapAtEOL    = 0x0002 // ENABLE_WRAP_AT_EOL_OUTPUT — cleared, see below
	vtProcessing = 0x0004 // ENABLE_VIRTUAL_TERMINAL_PROCESSING
	// noAutoReturn (DISABLE_NEWLINE_AUTO_RETURN) is deliberately *not* set.
	// It is part of the usual "enable VT output" recipe, but it makes a bare
	// "\n" move down without returning to column 0 — and the host writes the
	// session's closing lines, the resume code among them, as plain text
	// through this console before the restore runs. Clearing wrapAtEOL is
	// what stops the cursor moving off a full-width row; this would only
	// staircase that text.
	noAutoReturn = 0x0008
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
	if want := mode&^wrapAtEOL | vtProcessing; want != mode {
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
