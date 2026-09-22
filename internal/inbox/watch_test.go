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
