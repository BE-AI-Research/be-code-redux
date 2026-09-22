package inbox

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestWatchDeliversNewMessagesOnlyOnce(t *testing.T) {
	dir := t.TempDir()
	Send(dir, "alice", "bob", "before the watch") // must never be delivered
	var mu sync.Mutex
	var got []Message
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Watch(ctx, dir, 20*time.Millisecond, func(m Message) {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	})
	time.Sleep(60 * time.Millisecond)
	Send(dir, "carol", "bob", "one")
	time.Sleep(2 * time.Millisecond)
	Send(dir, "alice", "dave", "two") // a user directory that did not exist at start
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond) // a second poll must not redeliver
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0].Text != "one" || got[1].Text != "two" || got[1].To != "dave" {
		t.Fatalf("got %+v", got)
	}
}

func TestWatchStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Watch(ctx, t.TempDir(), 10*time.Millisecond, func(Message) {}); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return")
	}
}

// A poll that runs between a message's temp-file write and its rename sees
// the directory's mtime move; the rename then lands in the same coarse
// kernel timestamp tick and the directory looks unchanged for ever. The
// mtime shortcut must never skip a directory modified within the last
// second, so a message written on the poll's own tick is still found.
func TestWatchFindsAMessageWrittenOnThePollsOwnTick(t *testing.T) {
	dir := t.TempDir()
	Send(dir, "x", "bob", "history")
	var mu sync.Mutex
	got := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Watch(ctx, dir, time.Millisecond, func(Message) { mu.Lock(); got++; mu.Unlock() })
	time.Sleep(20 * time.Millisecond)
	const n = 200
	for i := 0; i < n; i++ {
		Send(dir, "alice", "bob", "m")
		time.Sleep(time.Duration(i%3) * time.Millisecond) // land on every phase of the poll
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := got == n
		mu.Unlock()
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("delivered %d of %d", got, n)
}

// Delivery is in time order across recipients, not in directory-name order:
// a message to "zack" sent before one to "abe" is delivered first.
func TestWatchDeliversAcrossUsersInTimeOrder(t *testing.T) {
	dir := t.TempDir()
	var mu sync.Mutex
	var got []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Watch(ctx, dir, 500*time.Millisecond, func(m Message) { mu.Lock(); got = append(got, m.To); mu.Unlock() })
	time.Sleep(20 * time.Millisecond) // past the baseline, well before the first poll
	Send(dir, "x", "zack", "first")
	time.Sleep(2 * time.Millisecond)
	Send(dir, "x", "abe", "second")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != "zack" || got[1] != "abe" {
		t.Fatalf("order %v", got)
	}
}
