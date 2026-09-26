package browser

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/browser/browsertest"
)

func dialFake(t *testing.T, fb *browsertest.Browser) *Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, fb.WSURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestConnCallDecodesResult(t *testing.T) {
	fb := browsertest.New(t)
	fb.Handle("Browser.getVersion", func(string, json.RawMessage) (any, error) {
		return map[string]string{"product": "FakeChrome/1.0"}, nil
	})
	c := dialFake(t, fb)
	var v struct {
		Product string `json:"product"`
	}
	if err := c.Call(context.Background(), "", "Browser.getVersion", nil, &v); err != nil {
		t.Fatal(err)
	}
	if v.Product != "FakeChrome/1.0" {
		t.Fatalf("product %q", v.Product)
	}
}

func TestConnSendsSessionID(t *testing.T) {
	fb := browsertest.New(t)
	c := dialFake(t, fb)
	if err := c.Call(context.Background(), "S1", "Page.enable", nil, nil); err != nil {
		t.Fatal(err)
	}
	calls := fb.Calls("Page.enable")
	if len(calls) != 1 || calls[0].SessionID != "S1" {
		t.Fatalf("calls %+v", calls)
	}
}

func TestConnProtocolErrorNamesTheMethod(t *testing.T) {
	fb := browsertest.New(t)
	fb.Handle("DOM.getBoxModel", func(string, json.RawMessage) (any, error) {
		return nil, errors.New("Could not compute box model.")
	})
	c := dialFake(t, fb)
	err := c.Call(context.Background(), "S1", "DOM.getBoxModel", nil, nil)
	if err == nil || !strings.Contains(err.Error(), "DOM.getBoxModel") || !strings.Contains(err.Error(), "Could not compute box model.") {
		t.Fatalf("error %v", err)
	}
}

func TestConnRoutesEvents(t *testing.T) {
	fb := browsertest.New(t)
	c := dialFake(t, fb)
	got := make(chan Event, 1)
	c.Subscribe(func(ev Event) { got <- ev })
	c.Call(context.Background(), "", "Target.setDiscoverTargets", nil, nil) // the connection is live
	fb.Emit("S1", "Page.loadEventFired", map[string]any{"timestamp": 1.5})
	select {
	case ev := <-got:
		if ev.Method != "Page.loadEventFired" || ev.SessionID != "S1" || !strings.Contains(string(ev.Params), "1.5") {
			t.Fatalf("event %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no event")
	}
}

func TestConnUnsubscribe(t *testing.T) {
	fb := browsertest.New(t)
	c := dialFake(t, fb)
	var mu sync.Mutex
	n := 0
	cancel := c.Subscribe(func(Event) { mu.Lock(); n++; mu.Unlock() })
	cancel()
	c.Call(context.Background(), "", "Target.setDiscoverTargets", nil, nil) // the connection is live
	fb.Emit("", "Target.targetCreated", map[string]any{})
	c.Call(context.Background(), "", "Target.setDiscoverTargets", nil, nil) // after the event, in order
	mu.Lock()
	defer mu.Unlock()
	if n != 0 {
		t.Fatalf("an unsubscribed func saw %d events", n)
	}
}

func TestConnMultiplexesConcurrentCalls(t *testing.T) {
	fb := browsertest.New(t)
	fb.Handle("Echo", func(_ string, p json.RawMessage) (any, error) { return p, nil })
	c := dialFake(t, fb)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var r struct{ N int }
			if err := c.Call(context.Background(), "", "Echo", map[string]int{"N": i}, &r); err != nil || r.N != i {
				t.Errorf("call %d got %d, %v", i, r.N, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestConnCallHonoursContext(t *testing.T) {
	fb := browsertest.New(t)
	fb.Handle("Slow", func(string, json.RawMessage) (any, error) {
		time.Sleep(time.Second)
		return nil, nil
	})
	c := dialFake(t, fb)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := c.Call(ctx, "", "Slow", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatal("Call waited past its context")
	}
}

func TestConnDropFailsPendingCallsAndMarksDone(t *testing.T) {
	fb := browsertest.New(t)
	release := make(chan struct{})
	fb.Handle("Hang", func(string, json.RawMessage) (any, error) {
		<-release
		return nil, nil
	})
	defer close(release)
	c := dialFake(t, fb)
	errc := make(chan error, 1)
	go func() { errc <- c.Call(context.Background(), "", "Hang", nil, nil) }()
	time.Sleep(50 * time.Millisecond)
	fb.Drop()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("a pending call succeeded on a dropped connection")
		}
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("pending call error %v does not wrap ErrClosed", err)
		}
		if !strings.Contains(err.Error(), "Hang") {
			t.Fatalf("pending call error %v does not name the method", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the pending call never returned")
	}
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done never closed")
	}
	if c.Err() == nil {
		t.Fatal("Err is nil on a dead connection")
	}
	err := c.Call(context.Background(), "", "Browser.getVersion", nil, nil)
	if err == nil {
		t.Fatal("a call on a dead connection succeeded")
	}
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("dead-connection call error %v does not wrap ErrClosed", err)
	}
	if !strings.Contains(err.Error(), "Browser.getVersion") {
		t.Fatalf("dead-connection call error %v does not name the method", err)
	}
}

// TestConnCloseReleasesEverything checks Close's own path through fail:
// a pending call fails wrapping ErrClosed, Done closes, Err wraps ErrClosed,
// and a second Close is a harmless no-op (fail is idempotent).
func TestConnCloseReleasesEverything(t *testing.T) {
	fb := browsertest.New(t)
	release := make(chan struct{})
	fb.Handle("Hang", func(string, json.RawMessage) (any, error) {
		<-release
		return nil, nil
	})
	defer close(release)
	c := dialFake(t, fb)
	errc := make(chan error, 1)
	go func() { errc <- c.Call(context.Background(), "", "Hang", nil, nil) }()
	time.Sleep(50 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-errc:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("pending call error %v does not wrap ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the pending call never returned")
	}
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done never closed")
	}
	if !errors.Is(c.Err(), ErrClosed) {
		t.Fatalf("Err() %v does not wrap ErrClosed", c.Err())
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestConnFinishCallPrefersAReadyReplyOverDone pins the fix for a genuine
// race: a browser can answer a command and disconnect right after, so by
// the time a call's select runs, its reply channel and Done can both
// already be ready — and Go's select then picks between ready cases at
// random. finishCall's done-branch must peek the reply channel first rather
// than trust Done alone. Both channels are made ready before finishCall is
// ever invoked, so each iteration's select genuinely has both ready at its
// first evaluation (no goroutine parking to bias the outcome); repeating it
// gives overwhelming confidence an unfixed done-branch is caught (each
// iteration independently has about even odds of hitting it) and that the
// fixed one never fails.
func TestConnFinishCallPrefersAReadyReplyOverDone(t *testing.T) {
	fb := browsertest.New(t)
	c := dialFake(t, fb)
	c.fail(errors.New("simulated drop"))
	for i := 0; i < 200; i++ {
		ch := make(chan response, 1)
		ch <- response{result: json.RawMessage(`{"N":42}`)}
		var v struct{ N int }
		if err := c.finishCall(context.Background(), int64(i), ch, "Echo", &v); err != nil {
			t.Fatalf("iteration %d: finishCall returned %v though the reply had already arrived", i, err)
		}
		if v.N != 42 {
			t.Fatalf("iteration %d: N = %d", i, v.N)
		}
	}
}

func TestConnReaderPanicIsContained(t *testing.T) {
	fb := browsertest.New(t)
	c := dialFake(t, fb)
	c.Subscribe(func(Event) { panic("boom") })
	c.Call(context.Background(), "", "Target.setDiscoverTargets", nil, nil)
	fb.Emit("", "Target.targetCreated", map[string]any{})
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("a panicking subscriber did not end the connection")
	}
	if !strings.Contains(c.Err().Error(), "panicked") {
		t.Fatalf("err %v", c.Err())
	}
}
