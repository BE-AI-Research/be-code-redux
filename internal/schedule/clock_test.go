package schedule

import (
	"sync"
	"testing"
	"time"
)

func TestFakeClockFiresOnAdvance(t *testing.T) {
	c := NewFakeClock(time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC))
	tm := c.NewTimer(10 * time.Minute)
	c.Advance(9 * time.Minute)
	select {
	case <-tm.C():
		t.Fatal("fired early")
	default:
	}
	c.Advance(time.Minute)
	select {
	case <-tm.C():
	default:
		t.Fatal("did not fire")
	}
	stopped := c.NewTimer(time.Minute)
	stopped.Stop()
	c.Advance(time.Hour)
	select {
	case <-stopped.C():
		t.Fatal("stopped timer fired")
	default:
	}
	if immediate := c.NewTimer(0); len(immediate.C()) != 1 {
		t.Fatal("a zero timer fires at once")
	}
}

func TestFakeClockStopConcurrent(t *testing.T) {
	// Verify Stop() is safe to call concurrently with Advance().
	// This test is meant to be run with go test -race.
	c := NewFakeClock(time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC))
	tm := c.NewTimer(1 * time.Hour)

	var wg sync.WaitGroup
	var stopped int
	var mu sync.Mutex

	// Goroutine 1: Call Stop() repeatedly
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if tm.Stop() {
				mu.Lock()
				stopped++
				mu.Unlock()
			}
		}
	}()

	// Goroutine 2: Call Advance() repeatedly to trigger fireLocked()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			c.Advance(1 * time.Second)
		}
	}()

	wg.Wait()

	// At most one Stop() should succeed (timer can only be stopped once)
	if stopped > 1 {
		t.Fatalf("Stop() succeeded %d times, want at most 1", stopped)
	}
}

// TestFakeClockSuspend: a suspended machine's wall clock moves on while its
// timers (monotonic) do not elapse — pending timers move with the clock.
func TestFakeClockSuspend(t *testing.T) {
	c := NewFakeClock(time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC))
	tm := c.NewTimer(time.Minute)
	c.Suspend(3 * time.Hour)
	select {
	case <-tm.C():
		t.Fatal("a timer does not elapse while suspended")
	default:
	}
	if got := c.Now(); !got.Equal(time.Date(2026, 9, 26, 13, 0, 0, 0, time.UTC)) {
		t.Fatalf("wall clock moved: %v", got)
	}
	c.Advance(time.Minute)
	select {
	case <-tm.C():
	default:
		t.Fatal("it fires a minute of awake time later")
	}
}
