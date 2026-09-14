package tui

import (
	"strings"
	"testing"
)

// Themes with a canonical background emit OSC 11 (and OSC 10 for light
// themes); the built-in dark/light/mono emit the reset sequences.
func TestTerminalColorSequences(t *testing.T) {
	seq := terminalColorSeq("dracula")
	if !strings.Contains(seq, "\x1b]11;#282a36\x07") {
		t.Fatalf("dracula background missing: %q", seq)
	}
	seq = terminalColorSeq("solarized-light")
	if !strings.Contains(seq, "\x1b]11;#fdf6e3\x07") || !strings.Contains(seq, "\x1b]10;#657b83\x07") {
		t.Fatalf("solarized-light must set background and foreground: %q", seq)
	}
	for _, n := range []string{"dark", "light", "mono", "no-such"} {
		if seq := terminalColorSeq(n); seq != terminalColorReset() {
			t.Fatalf("%s should reset the terminal colours, got %q", n, seq)
		}
	}
	if r := terminalColorReset(); !strings.Contains(r, "\x1b]111\x07") || !strings.Contains(r, "\x1b]110\x07") {
		t.Fatalf("reset must contain OSC 111 and 110: %q", r)
	}
}

// Applying a theme through the TUI sends the sequence via the swappable
// writer; the config switch disables it.
func TestApplyThemeSendsTerminalColors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	m := newTestModel(t)
	var sent []string
	m.termWrite = func(s string) { sent = append(sent, s) }
	m.applyTheme("nord")
	if len(sent) != 1 || !strings.Contains(sent[0], "#2e3440") {
		t.Fatalf("sent = %q", sent)
	}
	m.cfg.ThemeTerminalColors = false
	m.applyTheme("dracula")
	if len(sent) != 1 {
		t.Fatalf("sequence sent although disabled: %q", sent)
	}
}
