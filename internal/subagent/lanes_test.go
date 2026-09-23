package subagent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLaneSerialisesOneServer(t *testing.T) {
	l := NewLanes()
	var inside, maxInside int32
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := l.Acquire(context.Background(), "http://a:1", false)
			if err != nil {
				t.Error(err)
				return
			}
			n := atomic.AddInt32(&inside, 1)
			for {
				m := atomic.LoadInt32(&maxInside)
				if n <= m || atomic.CompareAndSwapInt32(&maxInside, m, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&inside, -1)
			rel()
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Fatalf("lane admitted %d at once", maxInside)
	}
}

func TestLanesRunTwoServersInParallel(t *testing.T) {
	l := NewLanes()
	relA, _ := l.Acquire(context.Background(), "http://a:1", false)
	defer relA()
	done := make(chan struct{})
	go func() {
		relB, err := l.Acquire(context.Background(), "http://b:1", false)
		if err == nil {
			relB()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a second server waited on the first's lane")
	}
}

func TestPrimaryGoesFirst(t *testing.T) {
	l := NewLanes()
	rel, _ := l.Acquire(context.Background(), "s", false)
	var order []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := func(name string, primary bool) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := l.Acquire(context.Background(), "s", primary)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			r()
		}()
	}
	start("sub", false)
	time.Sleep(20 * time.Millisecond)
	start("primary", true)
	time.Sleep(20 * time.Millisecond)
	rel()
	wg.Wait()
	if len(order) != 2 || order[0] != "primary" {
		t.Fatalf("order %v", order)
	}
}

func TestAcquireHonoursCancel(t *testing.T) {
	l := NewLanes()
	rel, _ := l.Acquire(context.Background(), "s", false)
	defer rel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := l.Acquire(ctx, "s", false); err == nil {
		t.Fatal("expected a cancelled acquire to fail")
	}
	if !l.Busy("s") {
		t.Fatal("lane should still be held")
	}
}
