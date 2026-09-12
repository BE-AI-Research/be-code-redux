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
