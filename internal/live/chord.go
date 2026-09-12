package live

import "time"

// Action reports what a keystroke chord recognised in a stream fed to
// Chord.Feed.
type Action int

const (
	ActionNone Action = iota
	ActionDetach
)

const chordKey = 0x1d // Ctrl+]
const chordWindow = time.Second

// Chord recognises the Ctrl+] prefix chord in a keystroke stream: Ctrl+]
// followed by 'd' or another Ctrl+] detaches; Ctrl+] followed by anything
// else (or nothing within chordWindow) forwards the literal bytes.
type Chord struct {
	pending bool
	since   time.Time
}

// Feed splits keystrokes into forwardable bytes and actions. Ctrl+] (0x1d)
// starts a chord; the next key decides: 'd' or 0x1d → ActionDetach, anything
// else → both bytes are forwarded verbatim. The chord expires after 1s (the
// pending 0x1d is forwarded).
//
// A detach ends the scan: this client is leaving, so there is nowhere left
// to deliver anything that followed in the same buffer.
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
