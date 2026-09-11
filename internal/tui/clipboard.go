package tui

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

// Clipboard access from a TUI has no single reliable path, so copy uses
// two at once: an OSC 52 escape (honoured by VS Code's terminal, most
// modern terminals and over SSH) plus the first system tool found. Reads
// use the system tools only, since terminals rarely permit OSC 52 reads.

// osc52 encodes text as an OSC 52 "set clipboard" sequence.
func osc52(s string) string {
	return "\x1b]52;c;" + base64.StdEncoding.EncodeToString([]byte(s)) + "\a"
}

var copyTools = [][]string{
	{"wl-copy"},
	{"xclip", "-selection", "clipboard"},
	{"xsel", "--clipboard", "--input"},
	{"pbcopy"},
	{"clip"},
}

var pasteTools = [][]string{
	{"wl-paste", "-n"},
	{"xclip", "-selection", "clipboard", "-o"},
	{"xsel", "--clipboard", "--output"},
	{"pbpaste"},
	{"powershell", "-NoProfile", "-Command", "Get-Clipboard"},
}

// writeClipboard sends text to the terminal clipboard via OSC 52 and to
// the system clipboard via the first available tool.
func writeClipboard(s string) error {
	// OSC sequences do not move the cursor, so writing between renderer
	// frames is harmless.
	_, _ = os.Stdout.WriteString(osc52(s))
	for _, t := range copyTools {
		if _, err := exec.LookPath(t[0]); err != nil {
			continue
		}
		cmd := exec.Command(t[0], t[1:]...)
		cmd.Stdin = strings.NewReader(s)
		if err := cmd.Run(); err == nil {
			return nil
		}
	}
	return nil // OSC 52 was sent; the terminal decides
}

// readClipboard returns the system clipboard text via the first tool that
// works.
func readClipboard() (string, error) {
	for _, t := range pasteTools {
		if runtime.GOOS != "windows" && t[0] == "powershell" {
			continue
		}
		if _, err := exec.LookPath(t[0]); err != nil {
			continue
		}
		var out bytes.Buffer
		cmd := exec.Command(t[0], t[1:]...)
		cmd.Stdout = &out
		if err := cmd.Run(); err == nil {
			return strings.TrimRight(out.String(), "\r\n"), nil
		}
	}
	return "", errors.New("no clipboard tool found (install wl-clipboard, xclip or xsel); paste with your terminal instead")
}
