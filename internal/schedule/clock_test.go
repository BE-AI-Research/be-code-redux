package schedule

import (
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
