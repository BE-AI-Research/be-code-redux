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
// reaches the program).
const escapeHoldback = 50 * time.Millisecond

// clientState is the per-client parsing state: the pipe feeding its
// x/input.Reader, any trailing bytes held back because they look like the
// start of an escape sequence that hasn't finished arriving, and the timer
// that flushes them if nothing more arrives. gen guards against a timer
// that fired concurrently with a newer Feed/flush for the same client.
type clientState struct {
	w       *io.PipeWriter
	pending []byte
	timer   *time.Timer
	gen     int
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
// the caller for long: it only writes into that client's own pipe, which its
// parser goroutine drains. Feed on a dropped client, or after Close, is a
// no-op.
func (k *KeyPump) Feed(client int, b []byte) {
	k.mu.Lock()
	if k.closed || k.dropped[client] {
		k.mu.Unlock()
		return
	}
	cs, ok := k.clients[client]
	if !ok {
		r, w := io.Pipe()
		cs = &clientState{w: w}
		k.clients[client] = cs
		k.wg.Add(1)
		go k.run(client, r)
	}
	if cs.timer != nil {
		cs.timer.Stop()
		cs.timer = nil
	}

	data := make([]byte, 0, len(cs.pending)+len(b))
	data = append(data, cs.pending...)
	data = append(data, b...)
	cs.pending = nil

	forward, pending := splitPendingEscape(data)
	if len(pending) > 0 {
		cs.pending = append([]byte(nil), pending...)
		cs.gen++
		gen := cs.gen
		cs.timer = time.AfterFunc(escapeHoldback, func() { k.flushPending(client, gen) })
	}
	w := cs.w
	k.mu.Unlock()

	if len(forward) > 0 {
		w.Write(forward) //nolint:errcheck // a write error just means the reader side is gone
	}
}

// flushPending writes out a client's held-back bytes once escapeHoldback has
// elapsed with nothing more arriving. gen guards against firing after a
// newer Feed (or an earlier flush) has already consumed or replaced the
// pending bytes.
func (k *KeyPump) flushPending(client, gen int) {
	k.mu.Lock()
	cs, ok := k.clients[client]
	if !ok || cs.gen != gen || len(cs.pending) == 0 {
		k.mu.Unlock()
		return
	}
	data := cs.pending
	cs.pending = nil
	cs.timer = nil
	w := cs.w
	k.mu.Unlock()

	w.Write(data) //nolint:errcheck // a write error just means the reader side is gone
}

// splitPendingEscape splits data into the bytes safe to forward now and any
// trailing bytes that look like the start of an escape sequence still in
// progress: a bare trailing ESC, or ESC followed by one of the CSI/SS3/OSC/
// DCS introducers ('[', 'O', ']', 'P') and then nothing but parameter or
// intermediate bytes (0x20-0x3F) with no final byte (0x40-0x7E) yet.
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
	case '[', 'O', ']', 'P':
		i := 2
		for i < len(tail) && tail[i] >= 0x20 && tail[i] <= 0x3f {
			i++
		}
		if i == len(tail) {
			// Ran out of bytes without seeing a final byte: incomplete.
			return data[:idx], tail
		}
	}
	return data, nil
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
func (k *KeyPump) markDropped(client int) {
	k.mu.Lock()
	if cs, ok := k.clients[client]; ok {
		if cs.timer != nil {
			cs.timer.Stop()
		}
		delete(k.clients, client)
	}
	k.dropped[client] = true
	k.mu.Unlock()
}

// Drop closes the client's pipe, which stops its parser goroutine, and marks
// the client so any later Feed is a no-op.
func (k *KeyPump) Drop(client int) {
	k.mu.Lock()
	cs, ok := k.clients[client]
	delete(k.clients, client)
	k.dropped[client] = true
	if ok && cs.timer != nil {
		cs.timer.Stop()
	}
	k.mu.Unlock()
	if ok {
		cs.w.Close()
	}
}

// Close marks the pump closed (so no later Feed can spawn a new client
// goroutine), drops every remaining client, and waits for all parser
// goroutines to exit.
func (k *KeyPump) Close() {
	k.mu.Lock()
	k.closed = true
	for id, cs := range k.clients {
		if cs.timer != nil {
			cs.timer.Stop()
		}
		cs.w.Close()
		delete(k.clients, id)
	}
	k.mu.Unlock()
	k.wg.Wait()
}
