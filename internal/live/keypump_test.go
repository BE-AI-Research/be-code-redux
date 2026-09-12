package live

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestKeyPumpTagsClientsAndParsesSequences(t *testing.T) {
	got := make(chan tea.Msg, 16)
	kp := NewKeyPump("xterm-256color", func(m tea.Msg) { got <- m })
	defer kp.Close()
	kp.Feed(1, []byte("a"))
	kp.Feed(2, []byte("\x1b[A"))       // up arrow
	kp.Feed(1, []byte("\x1b[<0;4;5M")) // SGR left press at col 4,row 5
	seen := map[int][]tea.Msg{}
	for i := 0; i < 3; i++ {
		select {
		case m := <-got:
			switch v := m.(type) {
			case ClientKeyMsg:
				seen[v.Client] = append(seen[v.Client], v.Key)
			case ClientMouseMsg:
				seen[v.Client] = append(seen[v.Client], v.Mouse)
			default:
				t.Fatalf("untagged message %T", m)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("pump delivered fewer than 3 messages")
		}
	}
	if len(seen[1]) != 2 || len(seen[2]) != 1 {
		t.Fatalf("tagging: %+v", seen)
	}
	if k := seen[1][0].(tea.KeyMsg); k.Type != tea.KeyRunes || string(k.Runes) != "a" {
		t.Fatalf("client 1 key: %+v", k)
	}
	if k := seen[2][0].(tea.KeyMsg); k.Type != tea.KeyUp {
		t.Fatalf("client 2 key: %+v", k)
	}
	if mm := seen[1][1].(tea.MouseMsg); mm.X != 3 || mm.Y != 4 || mm.Action != tea.MouseActionPress {
		t.Fatalf("client 1 mouse: %+v", mm)
	}
	kp.Drop(2)
	kp.Feed(2, []byte("z")) // dropped client: silently ignored
	select {
	case m := <-got:
		t.Fatalf("message after Drop: %+v", m)
	case <-time.After(200 * time.Millisecond):
	}
}

func TestSplitPendingEscape(t *testing.T) {
	cases := []struct {
		name             string
		in               string
		forward, pending string
	}{
		{"no escape at all", "ab", "ab", ""},
		{"bare trailing ESC", "a\x1b", "a", "\x1b"},
		{"complete CSI arrow", "\x1b[A", "\x1b[A", ""},
		{"CSI cut mid-params", "\x1b[<0;4;", "", "\x1b[<0;4;"},
		{"CSI with data before it", "hi\x1b[<0;4;", "hi", "\x1b[<0;4;"},
		{"SS3 introducer only", "\x1bO", "", "\x1bO"},
		{"complete SS3", "\x1bOP", "\x1bOP", ""},
		{"ESC plus plain rune (alt+x)", "\x1bx", "\x1bx", ""},
		{"OSC cut before terminator", "\x1b]52;c;", "", "\x1b]52;c;"},
		{"OSC cut mid payload (contains a letter)", "\x1b]52;c;Zm9v", "", "\x1b]52;c;Zm9v"},
		{"OSC complete with BEL", "\x1b]52;c;Zm9v\x07", "\x1b]52;c;Zm9v\x07", ""},
		{"DCS cut before terminator", "\x1bPq", "", "\x1bPq"},
		{"DCS complete with ST", "\x1bPq\x1b\\", "\x1bPq\x1b\\", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			forward, pending := splitPendingEscape([]byte(c.in))
			if string(forward) != c.forward || string(pending) != c.pending {
				t.Fatalf("splitPendingEscape(%q) = (%q, %q), want (%q, %q)", c.in, forward, pending, c.forward, c.pending)
			}
		})
	}
}

func TestKeyPumpBuffersSplitEscapeSequence(t *testing.T) {
	got := make(chan tea.Msg, 4)
	kp := NewKeyPump("xterm-256color", func(m tea.Msg) { got <- m })
	defer kp.Close()

	kp.Feed(1, []byte("\x1b")) // just the ESC byte, could be the start of "up arrow"
	kp.Feed(1, []byte("[A"))   // rest arrives a moment later

	select {
	case m := <-got:
		ck, ok := m.(ClientKeyMsg)
		if !ok || ck.Client != 1 || ck.Key.Type != tea.KeyUp {
			t.Fatalf("want a single KeyUp, got %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("split escape sequence never arrived")
	}
	select {
	case m := <-got:
		t.Fatalf("split escape sequence misreported as two events, extra: %+v", m)
	case <-time.After(150 * time.Millisecond):
	}
}

func TestKeyPumpFlushesLoneEscapeAfterHoldback(t *testing.T) {
	got := make(chan tea.Msg, 4)
	kp := NewKeyPump("xterm-256color", func(m tea.Msg) { got <- m })
	defer kp.Close()

	kp.Feed(1, []byte("\x1b")) // a genuine, unaccompanied Esc keypress

	select {
	case m := <-got:
		ck, ok := m.(ClientKeyMsg)
		if !ok || ck.Key.Type != tea.KeyEsc {
			t.Fatalf("want KeyEsc, got %+v", m)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("lone Esc was never flushed after the holdback timer")
	}
}

func TestKeyPumpBuffersSplitMouseSequence(t *testing.T) {
	got := make(chan tea.Msg, 4)
	kp := NewKeyPump("xterm-256color", func(m tea.Msg) { got <- m })
	defer kp.Close()

	kp.Feed(1, []byte("\x1b[<0;4;")) // SGR mouse sequence, cut mid-params
	kp.Feed(1, []byte("5M"))         // button code + final byte arrive later

	select {
	case m := <-got:
		cm, ok := m.(ClientMouseMsg)
		if !ok || cm.Mouse.Action != tea.MouseActionPress || cm.Mouse.X != 3 || cm.Mouse.Y != 4 {
			t.Fatalf("want a single mouse press at (3,4), got %+v", m)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("split mouse sequence never arrived")
	}
}

func TestKeyPumpBuffersSplitOSCSequence(t *testing.T) {
	got := make(chan tea.Msg, 4)
	kp := NewKeyPump("xterm-256color", func(m tea.Msg) { got <- m })
	defer kp.Close()

	kp.Feed(1, []byte("\x1b]52;c;")) // OSC 52 clipboard reply, cut before the payload/terminator
	kp.Feed(1, []byte("Zm9v\x07"))   // base64 payload ("foo") + BEL terminator arrive later

	// A complete OSC 52 sequence parses as a clipboard event, which
	// ConvertEvent has no mapping for, so the only correct outcome is no
	// message at all - not a misparsed KeyEsc/rune fallout from splitting it
	// at the letter 'Z' or 'm' the way reusing the CSI final-byte range did.
	select {
	case m := <-got:
		t.Fatalf("split OSC 52 sequence produced an event, want none: %+v", m)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestKeyPumpEscapeRaceStress forces the timer-vs-Feed ordering race: with
// escapeHoldback shrunk to effectively zero, a lone ESC's holdback timer is
// racing the very next Feed call (carrying the rest of the sequence) on
// every iteration. Two outcomes are legitimate:
//
//   - one KeyUp: the rest of the sequence arrived (or bufferLoop's
//     feed-priority recheck caught it) before the timer's flush committed.
//   - KeyEsc followed by runes '[' and 'A': the holdback had already,
//     genuinely expired (nothing readable on cs.feed yet at that instant)
//     when bufferLoop serviced the timer, so flushing the lone Esc on its
//     own is the correct, specified behaviour for an at-deadline race, not
//     a bug - with a 1ns holdback this is expected to happen occasionally.
//
// Anything else (wrong message count, wrong key types) is a real bug: it
// would mean the sequence was split into more than these two shapes, or
// misparsed. The at-deadline outcome must also stay rare: round 1's fix
// (a mutex + generation counter racing flushPending against Feed) measured
// at roughly 1 in 1000-3000 iterations under this same stress; the current
// design (a single per-client goroutine owning all pending/timer state,
// with an explicit non-blocking priority check of new bytes before
// committing to a timer-driven flush) measured at roughly 1 in 25000. 1000
// iterations with a 1% ceiling comfortably distinguishes "the current
// design" from "a regression back to something like round 1's rate" while
// never itself flaking: at a true rate of 1-in-25000, seeing more than 10
// occurrences (1% of 1000) in one run essentially never happens by chance,
// but seeing zero is the common case.
func TestKeyPumpEscapeRaceStress(t *testing.T) {
	orig := escapeHoldback
	escapeHoldback = time.Nanosecond
	defer func() { escapeHoldback = orig }()

	got := make(chan tea.Msg, 8)
	kp := NewKeyPump("xterm-256color", func(m tea.Msg) { got <- m })
	defer kp.Close()

	const iterations = 1000
	const maxFlushRate = 0.01 // the legitimate at-deadline flush must stay rare

	atDeadlineFlushes := 0
	for i := 0; i < iterations; i++ {
		client := i + 1 // client ids are never reused, so a fresh one each time
		kp.Feed(client, []byte("\x1b"))
		kp.Feed(client, []byte("[A"))

		var msgs []tea.Msg
	collect:
		for {
			select {
			case m := <-got:
				msgs = append(msgs, m)
			case <-time.After(8 * time.Millisecond):
				break collect
			}
		}

		switch len(msgs) {
		case 1:
			ck, ok := msgs[0].(ClientKeyMsg)
			if !ok || ck.Key.Type != tea.KeyUp {
				t.Fatalf("iteration %d (client %d): single message but not KeyUp: %+v", i, client, msgs[0])
			}
		case 3:
			esc, ok0 := msgs[0].(ClientKeyMsg)
			bracket, ok1 := msgs[1].(ClientKeyMsg)
			a, ok2 := msgs[2].(ClientKeyMsg)
			if !ok0 || !ok1 || !ok2 ||
				esc.Key.Type != tea.KeyEsc ||
				bracket.Key.Type != tea.KeyRunes || string(bracket.Key.Runes) != "[" ||
				a.Key.Type != tea.KeyRunes || string(a.Key.Runes) != "A" {
				t.Fatalf("iteration %d (client %d): 3 messages but not the at-deadline shape (Esc, '[', 'A'): %+v", i, client, msgs)
			}
			atDeadlineFlushes++
		default:
			t.Fatalf("iteration %d (client %d): want 1 or 3 messages, got %d: %+v", i, client, len(msgs), msgs)
		}
	}

	if rate := float64(atDeadlineFlushes) / float64(iterations); rate > maxFlushRate {
		t.Fatalf("at-deadline flush happened in %d/%d iterations (%.2f%%), want <= %.0f%%",
			atDeadlineFlushes, iterations, rate*100, maxFlushRate*100)
	}
}

// TestKeyPumpMarkDroppedOnReadError exercises the parser goroutine's own
// cleanup path (markDropped), independent of an explicit Drop/Close: the
// client's underlying pipe is closed out from under it (standing in for
// some genuine reader failure), which makes rd.ReadEvents error out and
// return from run() on its own. That must remove the stale client entry and
// mark the client permanently dropped, not just leave a dead pipe that
// silently swallows every later Feed.
func TestKeyPumpMarkDroppedOnReadError(t *testing.T) {
	got := make(chan tea.Msg, 4)
	kp := NewKeyPump("xterm-256color", func(m tea.Msg) { got <- m })
	defer kp.Close()

	kp.Feed(5, []byte("a")) // creates the client's pipe + parser goroutine
	select {
	case <-got: // drain the 'a' key: confirms the parser goroutine is up and reading
	case <-time.After(2 * time.Second):
		t.Fatal("initial Feed never produced a message")
	}

	kp.mu.Lock()
	cs := kp.clients[5]
	kp.mu.Unlock()
	if cs == nil {
		t.Fatal("client state missing after Feed")
	}
	cs.w.Close() // simulate the reader side failing on its own, not via Drop/Close

	deadline := time.Now().Add(2 * time.Second)
	for {
		kp.mu.Lock()
		_, present := kp.clients[5]
		kp.mu.Unlock()
		if !present {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run() never cleaned up its client entry after a read error")
		}
		time.Sleep(time.Millisecond)
	}

	kp.Feed(5, []byte("z")) // must now be a permanent no-op
	select {
	case m := <-got:
		t.Fatalf("message after a read-error cleanup: %+v", m)
	case <-time.After(200 * time.Millisecond):
	}

	kp.mu.Lock()
	n := len(kp.clients)
	dropped := kp.dropped[5]
	kp.mu.Unlock()
	if n != 0 || !dropped {
		t.Fatalf("client entry not cleaned up: clients=%d dropped=%v", n, dropped)
	}
}

func TestKeyPumpCloseRejectsLaterFeed(t *testing.T) {
	got := make(chan tea.Msg, 4)
	kp := NewKeyPump("xterm-256color", func(m tea.Msg) { got <- m })
	kp.Close()

	kp.Feed(9, []byte("a")) // no client was ever fed before Close

	kp.mu.Lock()
	n := len(kp.clients)
	kp.mu.Unlock()
	if n != 0 {
		t.Fatalf("Feed after Close spawned client state: %d entries", n)
	}
	select {
	case m := <-got:
		t.Fatalf("message after Close: %+v", m)
	case <-time.After(200 * time.Millisecond):
	}
}
