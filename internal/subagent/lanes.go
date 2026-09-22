package subagent

import (
	"context"
	"sync"
)

// Lanes serialises model calls per server (spec §2.2). One lane is one
// LaneKey; the primary's own calls take its lane with priority, so a
// sub-agent sharing the primary's server never makes the person wait.
type Lanes struct {
	mu    sync.Mutex
	lanes map[string]*lane
}

type lane struct {
	mu             sync.Mutex
	cond           *sync.Cond
	held           bool
	primaryWaiting int
}

func NewLanes() *Lanes { return &Lanes{lanes: map[string]*lane{}} }

func (l *Lanes) laneFor(server string) *lane {
	l.mu.Lock()
	defer l.mu.Unlock()
	ln, ok := l.lanes[server]
	if !ok {
		ln = &lane{}
		ln.cond = sync.NewCond(&ln.mu)
		l.lanes[server] = ln
	}
	return ln
}

// Acquire blocks until the server's lane is free (and, for a sub-agent,
// until no primary call is waiting), or ctx ends. The returned release
// must be called exactly once.
func (l *Lanes) Acquire(ctx context.Context, server string, primary bool) (func(), error) {
	ln := l.laneFor(server)
	stop := context.AfterFunc(ctx, func() {
		ln.mu.Lock()
		ln.cond.Broadcast()
		ln.mu.Unlock()
	})
	defer stop()
	ln.mu.Lock()
	defer ln.mu.Unlock()
	if primary {
		ln.primaryWaiting++
		defer func() { ln.primaryWaiting-- }()
	}
	for ln.held || (!primary && ln.primaryWaiting > 0) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ln.cond.Wait()
	}
	ln.held = true
	var once sync.Once
	return func() {
		once.Do(func() {
			ln.mu.Lock()
			ln.held = false
			ln.cond.Broadcast()
			ln.mu.Unlock()
		})
	}, nil
}

// Busy reports whether the server's lane is currently held.
func (l *Lanes) Busy(server string) bool {
	ln := l.laneFor(server)
	ln.mu.Lock()
	defer ln.mu.Unlock()
	return ln.held
}
