package tui

import (
	"os"
	"strings"
)

// inputHistory gives the TUI up/down input recall, persisted to the same
// history file readline uses in plain mode, so both UIs share one history.
type inputHistory struct {
	path  string
	lines []string
	pos   int // navigation cursor; len(lines) == "live" (not navigating)
	dirty bool
}

const historyLimit = 2000

func loadInputHistory(path string) *inputHistory {
	h := &inputHistory{path: path}
	if path != "" {
		if data, err := os.ReadFile(path); err == nil {
			for _, l := range strings.Split(string(data), "\n") {
				if strings.TrimSpace(l) != "" {
					h.lines = append(h.lines, l)
				}
			}
		}
	}
	h.pos = len(h.lines)
	return h
}

func (h *inputHistory) add(line string) {
	if line == "" || (len(h.lines) > 0 && h.lines[len(h.lines)-1] == line) {
		h.pos = len(h.lines)
		return
	}
	h.lines = append(h.lines, line)
	if len(h.lines) > historyLimit {
		h.lines = h.lines[len(h.lines)-historyLimit:]
	}
	h.pos = len(h.lines)
	h.dirty = true
}

func (h *inputHistory) prev() (string, bool) {
	if h.pos == 0 || len(h.lines) == 0 {
		return "", false
	}
	h.pos--
	return h.lines[h.pos], true
}

func (h *inputHistory) next() (string, bool) {
	if h.pos >= len(h.lines) {
		return "", false
	}
	h.pos++
	if h.pos == len(h.lines) {
		return "", true // back to live (empty) input
	}
	return h.lines[h.pos], true
}

func (h *inputHistory) save() {
	if !h.dirty || h.path == "" {
		return
	}
	_ = os.WriteFile(h.path, []byte(strings.Join(h.lines, "\n")+"\n"), 0o600)
}
