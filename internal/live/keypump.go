package live

import (
	"io"
	"sync"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/input"
)

// KeyPump turns each client's raw terminal bytes into tagged Bubble Tea
// messages. One x/input reader per client keeps escape sequences from two
// terminals from interleaving.
type KeyPump struct {
	term string
	emit func(tea.Msg)

	mu      sync.Mutex
	pipes   map[int]*io.PipeWriter
	dropped map[int]bool
	wg      sync.WaitGroup
}

// NewKeyPump returns a pump that parses each client's bytes as termType and
// calls emit for every converted key or mouse message, tagged with the
// client's id.
func NewKeyPump(termType string, emit func(tea.Msg)) *KeyPump {
	return &KeyPump{term: termType, emit: emit, pipes: map[int]*io.PipeWriter{}, dropped: map[int]bool{}}
}

// Feed appends raw bytes read from one client's connection. It never blocks
// the caller for long: it only writes into that client's own pipe, which its
// parser goroutine drains. Feed on a dropped client is ignored.
func (k *KeyPump) Feed(client int, b []byte) {
	k.mu.Lock()
	if k.dropped[client] {
		k.mu.Unlock()
		return
	}
	w, ok := k.pipes[client]
	if !ok {
		var r *io.PipeReader
		r, w = io.Pipe()
		k.pipes[client] = w
		k.wg.Add(1)
		go k.run(client, r)
	}
	k.mu.Unlock()
	w.Write(b) //nolint:errcheck // a write error just means the reader side is gone
}

func (k *KeyPump) run(client int, r *io.PipeReader) {
	defer k.wg.Done()
	rd, err := input.NewReader(r, k.term, 0)
	if err != nil {
		r.Close()
		return
	}
	defer rd.Close()
	for {
		evs, err := rd.ReadEvents()
		if err != nil {
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

// Drop closes the client's pipe, which stops its parser goroutine, and marks
// the client so any later Feed is a no-op.
func (k *KeyPump) Drop(client int) {
	k.mu.Lock()
	w, ok := k.pipes[client]
	delete(k.pipes, client)
	k.dropped[client] = true
	k.mu.Unlock()
	if ok {
		w.Close()
	}
}

// Close drops every remaining client and waits for all parser goroutines to
// exit.
func (k *KeyPump) Close() {
	k.mu.Lock()
	for id, w := range k.pipes {
		w.Close()
		delete(k.pipes, id)
	}
	k.mu.Unlock()
	k.wg.Wait()
}
