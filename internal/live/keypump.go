package live

import (
	"io"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/input"
)

// escapeHoldback is how long an incomplete escape sequence is held before
// being flushed as-is (so a genuine, unaccompanied Esc keypress still
// reaches the program). A var, not a const, so tests can shrink it to force
// the timer-vs-Feed race deterministically.
var escapeHoldback = 50 * time.Millisecond

// clientState is the per-client channel a Feed call hands raw bytes to, and
// the signal that tells its buffer goroutine (see bufferLoop) to stop.
// Nothing outside bufferLoop ever reads or writes pending/timer state: that
// state lives entirely on bufferLoop's stack, so there is exactly one
// goroutine that can ever decide "forward now" vs "hold and wait" for a
// given client, and no possibility of a timer callback racing a Feed call
// for the same decision.
type clientState struct {
	w    *io.PipeWriter
	feed chan []byte
	done chan struct{}
}

// KeyPump turns each client's raw terminal bytes into tagged Bubble Tea
// messages. One x/input reader per client keeps escape sequences from two
// terminals from interleaving. A client's bytes can arrive split across
// multiple Feed calls at an arbitrary boundary (e.g. one read syscall
// returning just an ESC byte); KeyPump holds back a trailing byte sequence
// that looks incomplete until more bytes arrive or escapeHoldback elapses.
type KeyPump struct {
	term string
	emit func(tea.Msg)

	mu      sync.Mutex
	clients map[int]*clientState
	dropped map[int]bool
	closed  bool
	wg      sync.WaitGroup
}

// NewKeyPump returns a pump that parses each client's bytes as termType and
// calls emit for every converted key or mouse message, tagged with the
// client's id.
func NewKeyPump(termType string, emit func(tea.Msg)) *KeyPump {
	return &KeyPump{term: termType, emit: emit, clients: map[int]*clientState{}, dropped: map[int]bool{}}
}

// Feed appends raw bytes read from one client's connection. It never blocks
// the caller for long: it only hands the bytes to that client's own buffer
// goroutine over a buffered channel. Feed on a dropped client, or after
// Close, is a no-op.
func (k *KeyPump) Feed(client int, b []byte) {
	k.mu.Lock()
	if k.closed || k.dropped[client] {
		k.mu.Unlock()
		return
	}
	cs, ok := k.clients[client]
	if !ok {
		r, w := io.Pipe()
		cs = &clientState{w: w, feed: make(chan []byte, 16), done: make(chan struct{})}
		k.clients[client] = cs
		k.wg.Add(2)
		go k.run(client, r)
		go k.bufferLoop(cs)
	}
	feed, done := cs.feed, cs.done
	k.mu.Unlock()

	// Copy: the caller may reuse its read buffer as soon as Feed returns,
	// but bufferLoop consumes this asynchronously.
	cp := append([]byte(nil), b...)
	select {
	case feed <- cp:
	case <-done:
		// Dropped/closed concurrently with this call: discard, same as the
		// closed/dropped check above catching it a moment earlier.
	}
}

// bufferLoop is the sole owner of one client's pending/timer state. It reads
// raw chunks from cs.feed and writes resolved bytes to cs.w, holding back a
// trailing incomplete escape sequence until either more bytes arrive on
// cs.feed or escapeHoldback elapses with nothing else arriving. Because
// every decision about that state is made here, sequentially, a "flush the
// timeout" decision and a "more bytes just arrived" decision can never race
// against each other for the same client.
func (k *KeyPump) bufferLoop(cs *clientState) {
	defer k.wg.Done()
	var pending []byte
	var timer *time.Timer
	var timerC <-chan time.Time
	stopTimer := func() {
		if timer != nil {
			timer.Stop()
			timer = nil
			timerC = nil
		}
	}
	defer stopTimer()

	// consume folds one more chunk of raw bytes into pending and forwards
	// whatever splitPendingEscape says is safe to send now, (re)arming the
	// holdback timer if a new incomplete tail remains.
	consume := func(b []byte) {
		stopTimer()
		data := make([]byte, 0, len(pending)+len(b))
		data = append(data, pending...)
		data = append(data, b...)
		pending = nil

		forward, newPending := splitPendingEscape(data)
		if len(newPending) > 0 {
			pending = append([]byte(nil), newPending...)
			timer = time.NewTimer(escapeHoldback)
			timerC = timer.C
		}
		if len(forward) > 0 {
			cs.w.Write(forward) //nolint:errcheck // a write error just means the reader side is gone
		}
	}

	for {
		select {
		case b := <-cs.feed:
			consume(b)

		case <-cs.done:
			return

		case <-timerC:
			// The holdback elapsed. But a Feed for this exact client can
			// have been in flight (already past the closed/dropped check,
			// already handed its bytes to the channel send) at the very
			// instant this case became ready; give cs.feed one immediate,
			// non-blocking chance to win before treating this as a genuine
			// timeout, so a chunk that's already sitting in the channel is
			// always folded in ahead of a stale flush.
			select {
			case b := <-cs.feed:
				consume(b)
			default:
				timer = nil
				timerC = nil
				if len(pending) > 0 {
					data := pending
					pending = nil
					cs.w.Write(data) //nolint:errcheck // a write error just means the reader side is gone
				}
			}
		}
	}
}

// splitPendingEscape splits data into the bytes safe to forward now and any
// trailing bytes that look like the start of an escape sequence still in
// progress: a bare trailing ESC; ESC followed by a CSI ('[') or SS3 ('O')
// introducer and then nothing but parameter or intermediate bytes
// (0x20-0x3F) with no final byte (0x40-0x7E) yet; or ESC followed by an OSC
// (']') or DCS ('P') introducer with no terminator (BEL, or ESC '\') seen
// yet.
func splitPendingEscape(data []byte) (forward, pending []byte) {
	idx := -1
	for i := len(data) - 1; i >= 0; i-- {
		if data[i] == 0x1b {
			idx = i
			break
		}
	}
	if idx < 0 {
		return data, nil
	}
	tail := data[idx:]
	if len(tail) == 1 {
		return data[:idx], tail
	}
	switch tail[1] {
	case '[', 'O':
		i := 2
		for i < len(tail) && tail[i] >= 0x20 && tail[i] <= 0x3f {
			i++
		}
		if i == len(tail) {
			// Ran out of bytes without seeing a final byte: incomplete.
			return data[:idx], tail
		}
	case ']', 'P':
		// OSC/DCS payloads are free-form text, not CSI parameter bytes; they
		// only end at a terminator (BEL, or ST = ESC '\'), never at a
		// "final byte" the way CSI/SS3 do. Reusing the CSI parameter-byte
		// range here would call a split mid-payload "complete" the moment it
		// contains any letter.
		if !hasStringTerminator(tail[2:]) {
			return data[:idx], tail
		}
	}
	return data, nil
}

// hasStringTerminator reports whether body (the bytes following an OSC or
// DCS introducer) contains a terminator: BEL (0x07), or an ESC that (once
// one more byte has arrived to confirm it isn't itself mid-arrival) starts
// an ST. It doesn't matter whether that next byte is actually '\': x/input's
// own OSC/DCS parsers resolve or cancel the sequence as soon as any byte
// follows such an ESC, so it's always safe to forward from that point.
func hasStringTerminator(body []byte) bool {
	for i := 0; i < len(body); i++ {
		switch body[i] {
		case 0x07:
			return true
		case 0x1b:
			return i+1 < len(body)
		}
	}
	return false
}

func (k *KeyPump) run(client int, r *io.PipeReader) {
	defer k.wg.Done()
	rd, err := input.NewReader(r, k.term, 0)
	if err != nil {
		r.Close()
		k.markDropped(client)
		return
	}
	defer rd.Close()
	for {
		evs, err := rd.ReadEvents()
		if err != nil {
			k.markDropped(client)
			return
		}
		for _, ev := range evs {
			m, ok := ConvertEvent(ev)
			if !ok {
				continue
			}
			switch v := m.(type) {
			case tea.KeyMsg:
				k.emit(ClientKeyMsg{Client: client, Key: v})
			case tea.MouseMsg:
				k.emit(ClientMouseMsg{Client: client, Mouse: v})
			}
		}
	}
}

// markDropped removes a client's state (if still present) and marks it
// dropped, so a stale entry left behind by a parser goroutine that exited on
// its own (a genuine read error, not a Drop/Close) doesn't silently
// swallow every later Feed forever: client ids are never reused, so once its
// parser has stopped, marking it dropped is equivalent to an explicit Drop.
// It also stops that client's bufferLoop goroutine.
func (k *KeyPump) markDropped(client int) {
	k.mu.Lock()
	cs, ok := k.clients[client]
	if ok {
		delete(k.clients, client)
	}
	k.dropped[client] = true
	k.mu.Unlock()
	if ok {
		close(cs.done)
	}
}

// Drop closes the client's pipe, which stops its parser goroutine, stops
// its buffer goroutine, and marks the client so any later Feed is a no-op.
func (k *KeyPump) Drop(client int) {
	k.mu.Lock()
	cs, ok := k.clients[client]
	delete(k.clients, client)
	k.dropped[client] = true
	k.mu.Unlock()
	if ok {
		close(cs.done)
		cs.w.Close()
	}
}

// Close marks the pump closed (so no later Feed can spawn new client
// goroutines), drops every remaining client, and waits for all parser and
// buffer goroutines to exit.
func (k *KeyPump) Close() {
	k.mu.Lock()
	k.closed = true
	for id, cs := range k.clients {
		close(cs.done)
		cs.w.Close()
		delete(k.clients, id)
	}
	k.mu.Unlock()
	k.wg.Wait()
}
