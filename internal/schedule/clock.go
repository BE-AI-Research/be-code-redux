// Package schedule is the vocabulary of scheduled events: when a schedule
// is due, what it is allowed to do, and how it is written in a project's
// schedules.md. It knows nothing of the agent, the tools or the UI and
// imports the standard library only.
package schedule

import (
	"sync"
	"time"
)

// Timer is the part of *time.Timer the scheduler uses.
type Timer interface {
	C() <-chan time.Time
	Stop() bool
}

// Clock is time for the scheduler; tests use FakeClock.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// RealClock is the wall clock.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }
func (RealClock) NewTimer(d time.Duration) Timer {
	return realTimer{time.NewTimer(d)}
}

type realTimer struct{ t *time.Timer }

func (r realTimer) C() <-chan time.Time { return r.t.C }
func (r realTimer) Stop() bool           { return r.t.Stop() }

// FakeClock is a hand-driven clock: timers fire only on Advance or Set.
type FakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	clock   *FakeClock
	at      time.Time
	ch      chan time.Time
	stopped bool
	fired   bool
}

func (f *fakeTimer) C() <-chan time.Time { return f.ch }
func (f *fakeTimer) Stop() bool {
	f.clock.mu.Lock()
	defer f.clock.mu.Unlock()
	was := !f.stopped && !f.fired
	f.stopped = true
	return was
}

func NewFakeClock(t time.Time) *FakeClock { return &FakeClock{now: t} }

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *FakeClock) NewTimer(d time.Duration) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &fakeTimer{clock: c, at: c.now.Add(d), ch: make(chan time.Time, 1)}
	c.timers = append(c.timers, t)
	c.fireLocked()
	return t
}

// Advance moves the clock forward and fires every timer now due.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.fireLocked()
}

// Set jumps the clock (either way) and fires every timer now due.
func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
	c.fireLocked()
}

func (c *FakeClock) fireLocked() {
	kept := c.timers[:0]
	for _, t := range c.timers {
		if t.stopped {
			continue
		}
		if !t.at.After(c.now) {
			t.fired = true
			t.ch <- c.now
			continue
		}
		kept = append(kept, t)
	}
	c.timers = kept
}
