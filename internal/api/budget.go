package api

import (
	"sync"
	"time"
)

// budgets is the per-app rate limit (a sliding hour of request times) and
// the one-in-flight rule. In memory only: a restart resets it, which is
// fine for a rate limit.
type budgets struct {
	now      func() time.Time
	mu       sync.Mutex
	times    map[string][]time.Time
	inflight map[string]bool
}

func newBudgets(now func() time.Time) *budgets {
	return &budgets{now: now, times: map[string][]time.Time{}, inflight: map[string]bool{}}
}

// take records one request for app against perHour. When refused it says
// how long until the oldest request leaves the window.
func (b *budgets) take(app string, perHour int) (retryAfter time.Duration, ok bool) {
	if perHour <= 0 {
		return 0, true
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	cut := now.Add(-time.Hour)
	ts := b.times[app]
	i := 0
	for i < len(ts) && !ts[i].After(cut) {
		i++
	}
	ts = ts[i:]
	if len(ts) >= perHour {
		b.times[app] = ts
		return ts[0].Add(time.Hour).Sub(now), false
	}
	b.times[app] = append(ts, now)
	return 0, true
}

// begin claims the app's one in-flight slot; release gives it back.
func (b *budgets) begin(app string) (release func(), ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inflight[app] {
		return nil, false
	}
	b.inflight[app] = true
	var once sync.Once
	return func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.inflight, app)
			b.mu.Unlock()
		})
	}, true
}
