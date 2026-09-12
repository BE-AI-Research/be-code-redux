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
