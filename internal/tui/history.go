package tui

import (
	"os"
	"strings"
)

// inputHistory gives the TUI up/down input recall, persisted to the same
// history file readline uses in plain mode, so both UIs share one history.
//
// In a shared session the recalled *lines* are common to the whole session
// (everything anyone submitted, in one file), but the navigation cursor is
// per client: each terminal walks that shared store at its own pace, so one
// person pressing Up never moves another's place in the list. Cursors are
// keyed by client id like Model.inputs, and an absent key means "live"
// (not navigating) — the resting state of a terminal that has never
// pressed Up.
type inputHistory struct {
	path    string
	lines   []string
	cursors map[int]int
	dirty   bool
}

// cursor is client's position in lines, clamped into range (the store is
// trimmed at historyLimit, which can leave an old cursor out of bounds).
func (h *inputHistory) cursor(client int) int {
	pos, ok := h.cursors[client]
	if !ok || pos > len(h.lines) {
		return len(h.lines)
	}
	if pos < 0 {
		return 0
	}
	return pos
}

func (h *inputHistory) setCursor(client, pos int) {
	if h.cursors == nil {
		h.cursors = map[int]int{}
	}
	h.cursors[client] = pos
}

// live puts client back at the end of the list, where Up starts from the
// newest line in the store — including lines another terminal has added
// since. That is why the resting state is an absent key and not a frozen
// index: a terminal at rest follows the shared store.
func (h *inputHistory) live(client int) { delete(h.cursors, client) }

// drop forgets a departed terminal's cursor.
func (h *inputHistory) drop(client int) { delete(h.cursors, client) }

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
	return h
}

// add records a submitted line in the shared store and returns the client
// that submitted it to the live end of the list. Everyone else keeps their
// place: appending does not shift the indices they are pointing at.
func (h *inputHistory) add(line string, client int) {
	if line != "" && !(len(h.lines) > 0 && h.lines[len(h.lines)-1] == line) {
		h.lines = append(h.lines, line)
		if len(h.lines) > historyLimit {
			h.lines = h.lines[len(h.lines)-historyLimit:]
		}
		h.dirty = true
	}
	h.live(client)
}

func (h *inputHistory) prev(client int) (string, bool) {
	pos := h.cursor(client)
	if pos == 0 || len(h.lines) == 0 {
		return "", false
	}
	pos--
	h.setCursor(client, pos)
	return h.lines[pos], true
}

func (h *inputHistory) next(client int) (string, bool) {
	pos := h.cursor(client)
	if pos >= len(h.lines) {
		return "", false
	}
	pos++
	if pos == len(h.lines) {
		h.live(client)
		return "", true // back to live (empty) input
	}
	h.setCursor(client, pos)
	return h.lines[pos], true
}

func (h *inputHistory) save() {
	if !h.dirty || h.path == "" {
		return
	}
	_ = os.WriteFile(h.path, []byte(strings.Join(h.lines, "\n")+"\n"), 0o600)
}
