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
	fwd, act = c.Feed([]byte("t"), now)
	if string(fwd) != "\x1dt" || act != ActionNone {
		t.Fatalf("Ctrl+] t is no longer a chord; both bytes forward: %q %v", fwd, act)
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

// TestChordForwardsBytesAroundAnUnknownChordKey covers a chord byte that
// shares its Read with the keystrokes on both sides of it (or a paste): since
// Ctrl+] followed by an unrecognised key (now including 't') forwards both
// bytes rather than consuming them, the rest of the buffer must still come
// back for forwarding.
//
// This replaces the old TestChordTakeoverKeepsProcessingTheRest, which
// exercised ActionTakeover: Task 3 removes that action entirely (Ctrl+] t no
// longer forwards to the host's take-input-back logic - see client.go), so
// there is nothing left for that test to assert beyond ordinary forwarding,
// already covered by TestChordDetachAndTakeover's "unknown chord key" case
// and this one's multi-byte read.
func TestChordForwardsBytesAroundAnUnknownChordKey(t *testing.T) {
	now := time.Now()
	var c Chord
	fwd, act := c.Feed([]byte{0x1d, 't', 'h', 'i'}, now)
	if act != ActionNone || string(fwd) != "\x1dthi" {
		t.Fatalf("forward = %q, act = %v; want \"\\x1dthi\" and ActionNone", fwd, act)
	}
	// A detach later in the same buffer still ends the scan and wins: this
	// client is leaving, so there is nowhere to deliver the rest.
	fwd, act = c.Feed([]byte("x\x1dty\x1ddz"), now)
	if act != ActionDetach {
		t.Fatalf("act = %v, want ActionDetach", act)
	}
	if string(fwd) != "x\x1dty" {
		t.Fatalf("forward = %q, want \"x\\x1dty\" (bytes after the detach are dropped)", fwd)
	}
	// The chord state is not left pending afterward.
	fwd, act = c.Feed([]byte("z"), now)
	if string(fwd) != "z" || act != ActionNone {
		t.Fatalf("after processing: %q %v", fwd, act)
	}
}
