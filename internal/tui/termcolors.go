package tui

import "os"

// Terminal colours: a theme with a canonical background also recolours the
// terminal window itself through OSC 11 (background) and OSC 10
// (foreground), which most modern terminals honour and the rest ignore.
// The reset sequences (OSC 111/110) hand the terminal back its own colours
// on exit or when switching to a theme without a background of its own.

// terminalColorSeq builds the escape sequence for a theme; themes without
// a background yield the reset sequence.
func terminalColorSeq(name string) string {
	p, ok := themes[name]
	if !ok || p.BG == "" {
		return terminalColorReset()
	}
	seq := "\x1b]11;" + p.BG + "\x07"
	if p.FG != "" {
		seq += "\x1b]10;" + p.FG + "\x07"
	}
	return seq
}

func terminalColorReset() string { return "\x1b]111\x07\x1b]110\x07" }

// writeTerminal sends raw escape bytes to the terminal. OSC sequences do
// not move the cursor, so this is safe between renderer frames.
func writeTerminal(s string) { _, _ = os.Stdout.WriteString(s) }
