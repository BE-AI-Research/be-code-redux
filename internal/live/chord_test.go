package live

import (
	"testing"
	"time"
)

func TestChordDetachAndTakeover(t *testing.T) {
	now := time.Now()
	var c Chord
	fwd, act := c.Feed([]byte("ab"), now)
	if string(fwd) != "ab" || act != ActionNone {
		t.Fatalf("plain: %q %v", fwd, act)
	}
	fwd, act = c.Feed([]byte{0x1d}, now)
	if len(fwd) != 0 || act != ActionNone {
		t.Fatalf("pending chord leaked: %q %v", fwd, act)
	}
	if _, act = c.Feed([]byte("d"), now); act != ActionDetach {
		t.Fatalf("Ctrl+] d: %v", act)
	}
	c.Feed([]byte{0x1d}, now)
	if _, act = c.Feed([]byte{0x1d}, now); act != ActionDetach {
		t.Fatalf("Ctrl+] Ctrl+]: %v", act)
	}
	c.Feed([]byte{0x1d}, now)
	if _, act = c.Feed([]byte("t"), now); act != ActionTakeover {
		t.Fatalf("Ctrl+] t: %v", act)
	}
	c.Feed([]byte{0x1d}, now)
	fwd, act = c.Feed([]byte("x"), now)
	if string(fwd) != "\x1dx" || act != ActionNone {
		t.Fatalf("unknown chord key must forward both: %q %v", fwd, act)
	}
	c.Feed([]byte{0x1d}, now)
	fwd, _ = c.Feed([]byte("y"), now.Add(2*time.Second))
	if string(fwd) != "\x1dy" {
		t.Fatalf("expired chord must forward: %q", fwd)
	}
}

// TestChordTakeoverKeepsProcessingTheRest covers a takeover chord that shares
// its Read with the keystrokes typed straight after it (or with a paste): the
// point of taking input back is to type, so those bytes must be returned for
// forwarding rather than dropped with the chord.
func TestChordTakeoverKeepsProcessingTheRest(t *testing.T) {
	now := time.Now()
	var c Chord
	fwd, act := c.Feed([]byte{0x1d, 't', 'h', 'i'}, now)
	if act != ActionTakeover {
		t.Fatalf("act = %v, want ActionTakeover", act)
	}
	if string(fwd) != "hi" {
		t.Fatalf("forward = %q, want \"hi\"", fwd)
	}
	// Bytes on both sides of the chord survive.
	fwd, act = c.Feed([]byte("ab\x1dtcd"), now)
	if act != ActionTakeover || string(fwd) != "abcd" {
		t.Fatalf("forward = %q, act = %v; want \"abcd\" and ActionTakeover", fwd, act)
	}
	// A detach later in the same buffer still ends the scan and wins: this
	// client is leaving, so there is nowhere to deliver the rest.
	fwd, act = c.Feed([]byte("x\x1dty\x1ddz"), now)
	if act != ActionDetach {
		t.Fatalf("act = %v, want ActionDetach", act)
	}
	if string(fwd) != "xy" {
		t.Fatalf("forward = %q, want \"xy\" (bytes after the detach are dropped)", fwd)
	}
	// The chord state is not left pending after a takeover.
	fwd, act = c.Feed([]byte("z"), now)
	if string(fwd) != "z" || act != ActionNone {
		t.Fatalf("after a takeover: %q %v", fwd, act)
	}
}
