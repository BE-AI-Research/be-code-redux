package live

import "testing"

// fakeConsole stands in for the Windows console API so the decidable half of
// the setup is tested on every platform.
type fakeConsole struct {
	mode     uint32
	cp       uint32
	modeErr  error // Mode() fails: not a console (a redirected handle)
	setErr   error // SetMode() fails
	setCPErr error // SetOutputCP() fails
	modeSets []uint32
	cpSets   []uint32
}

func (f *fakeConsole) Mode(uintptr) (uint32, error) {
	if f.modeErr != nil {
		return 0, f.modeErr
	}
	return f.mode, nil
}

func (f *fakeConsole) SetMode(_ uintptr, m uint32) error {
	if f.setErr != nil {
		return f.setErr
	}
	f.mode = m
	f.modeSets = append(f.modeSets, m)
	return nil
}

func (f *fakeConsole) OutputCP() uint32 { return f.cp }

func (f *fakeConsole) SetOutputCP(cp uint32) error {
	if f.setCPErr != nil {
		return f.setCPErr
	}
	f.cp = cp
	f.cpSets = append(f.cpSets, cp)
	return nil
}

// A client owns its terminal for the attach: the served program renders into
// a socket, so Bubble Tea never configures this console (it has no tty) and
// MakeRaw only touches the input handle. Without this the console keeps its
// OEM code page, every non-ASCII byte of a frame is drawn as two or three
// cells, lines overflow the width the renderer padded them to, and the alt
// screen scrolls out of step with the repaint.
func TestPrepareConsoleEnablesVTAndUTF8(t *testing.T) {
	f := &fakeConsole{mode: 0x0001 | wrapAtEOL, cp: 437} // processed output, wrap on, OEM
	restore, utf8 := prepareConsole(f, 7)
	if f.mode&vtProcessing == 0 {
		t.Errorf("VT processing not enabled: mode = %#x", f.mode)
	}
	// The host writes the session's closing lines — the resume code above all
	// — as plain text through this same console, before the restore runs. With
	// the newline auto-return disabled a bare "\n" moves down without
	// returning to column 0, which staircases exactly that text, so the flag
	// stays off: clearing wrapAtEOL is what stops the cursor moving.
	if f.mode&noAutoReturn != 0 {
		t.Errorf("newline auto-return disabled; it staircases the host's own output: mode = %#x", f.mode)
	}
	// The frame's rows are padded to exactly the width, so the last cell of a
	// row is always written. With wrap-at-EOL on, writing it moves the cursor
	// to the next row — and on the bottom row that scrolls the alt screen, so
	// the frame lands one row out of step and what scrolled past the bottom is
	// never painted over again: the ghost rows.
	if f.mode&wrapAtEOL != 0 {
		t.Errorf("wrap at end of line still enabled: mode = %#x", f.mode)
	}
	if f.mode&0x0001 == 0 {
		t.Errorf("existing mode bits dropped: mode = %#x", f.mode)
	}
	if f.cp != cpUTF8 {
		t.Errorf("output code page = %d, want %d", f.cp, cpUTF8)
	}
	if !utf8 {
		t.Error("utf8 = false, but the console was just put into UTF-8")
	}
	restore()
	if f.mode != 0x0001|wrapAtEOL {
		t.Errorf("mode after restore = %#x, want %#x", f.mode, 0x0001|wrapAtEOL)
	}
	if f.cp != 437 {
		t.Errorf("code page after restore = %d, want 437", f.cp)
	}
}

// A console whose wrap is already off and VT already on needs no mode write at
// all, and restore must not put a mode back that was never changed.
func TestPrepareConsoleLeavesReadyConsoleAlone(t *testing.T) {
	f := &fakeConsole{mode: vtProcessing, cp: cpUTF8}
	restore, _ := prepareConsole(f, 7)
	if len(f.modeSets) != 0 {
		t.Errorf("mode rewritten needlessly: %v", f.modeSets)
	}
	restore()
	if f.mode != vtProcessing {
		t.Errorf("mode after restore = %#x, want it untouched", f.mode)
	}
}

// A console already in UTF-8 is left alone, and restore must not set a code
// page back that was never changed.
func TestPrepareConsoleLeavesUTF8ConsoleAlone(t *testing.T) {
	f := &fakeConsole{mode: vtProcessing, cp: cpUTF8}
	restore, utf8 := prepareConsole(f, 7)
	if !utf8 {
		t.Error("utf8 = false for a console already in UTF-8")
	}
	if len(f.cpSets) != 0 {
		t.Errorf("code page rewritten needlessly: %v", f.cpSets)
	}
	restore()
	if f.cp != cpUTF8 {
		t.Errorf("code page after restore = %d, want %d", f.cp, cpUTF8)
	}
}

// A redirected handle is not a console: nothing is set, nothing is restored,
// and the caller is told this terminal is not known to render UTF-8.
func TestPrepareConsoleIgnoresNonConsole(t *testing.T) {
	f := &fakeConsole{modeErr: errNotConsole, cp: 437}
	restore, utf8 := prepareConsole(f, 7)
	if len(f.modeSets) != 0 {
		t.Errorf("mode set on a non-console: %v", f.modeSets)
	}
	if utf8 {
		t.Error("utf8 = true for a handle that is not a console")
	}
	restore() // must not panic, must change nothing
	if f.cp != 437 {
		t.Errorf("code page = %d, want 437 untouched", f.cp)
	}
}


// A console whose mode cannot be changed must still get UTF-8 output, and
// restore must put back only the code page.
func TestPrepareConsoleSetsCodePageWhenModeRefused(t *testing.T) {
	f := &fakeConsole{mode: 0x0001, cp: 437, setErr: errNotConsole}
	restore, utf8 := prepareConsole(f, 7)
	if f.cp != cpUTF8 {
		t.Errorf("output code page = %d, want %d", f.cp, cpUTF8)
	}
	if !utf8 {
		t.Error("utf8 = false after the code page was set")
	}
	restore()
	if f.cp != 437 {
		t.Errorf("code page after restore = %d, want 437", f.cp)
	}
	if f.mode != 0x0001 {
		t.Errorf("mode = %#x, want 0x1 untouched", f.mode)
	}
}
