package live

import "time"

// Action reports what a keystroke chord recognised in a stream fed to
// Chord.Feed.
type Action int

const (
	ActionNone Action = iota
	ActionDetach
	ActionTakeover
)

const chordKey = 0x1d // Ctrl+]
const chordWindow = time.Second

// Chord recognises the Ctrl+] prefix chords in a keystroke stream: Ctrl+]
// followed by 'd' or another Ctrl+] detaches, Ctrl+] followed by 't' asks
// to take over the input holder role, and Ctrl+] followed by anything else
// (or nothing within chordWindow) forwards the literal bytes.
type Chord struct {
	pending bool
	since   time.Time
}

// Feed splits keystrokes into forwardable bytes and actions. Ctrl+] (0x1d)
// starts a chord; the next key decides: 'd' or 0x1d → ActionDetach, 't' →
// ActionTakeover, anything else → both bytes are forwarded verbatim. The
// chord expires after 1s (the pending 0x1d is forwarded).
//
// A takeover does not end the scan: one Read routinely carries the chord and
// the keystrokes typed straight after it (and a paste carries everything at
// once), so the rest of the buffer keeps being processed and comes back in
// forward — the caller claims input first, then forwards those bytes (see
// Attach). A detach does end the scan: this client is leaving, so there is
// nowhere left to deliver anything that followed. A detach later in the same
// buffer therefore wins over an earlier takeover, which is the intent either
// way.
func (c *Chord) Feed(b []byte, now time.Time) (forward []byte, act Action) {
	var out []byte
	act = ActionNone
	for _, k := range b {
		if c.pending {
			c.pending = false
			if now.Sub(c.since) > chordWindow {
				out = append(out, chordKey)
			} else {
				switch k {
				case 'd', chordKey:
					return out, ActionDetach
				case 't':
					act = ActionTakeover
					continue
				default:
					out = append(out, chordKey)
				}
			}
		}
		if k == chordKey {
			c.pending, c.since = true, now
			continue
		}
		out = append(out, k)
	}
	return out, act
}
