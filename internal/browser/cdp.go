package browser

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// Event is one protocol event. SessionID names the page session it came
// from; "" is the browser itself.
type Event struct {
	Method    string
	SessionID string
	Params    json.RawMessage
}

// ErrClosed is what every call returns once the connection has gone —
// the browser was closed, killed or disconnected.
var ErrClosed = errors.New("browser connection closed")

type cdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpError) Error() string { return e.Message }

type response struct {
	result json.RawMessage
	err    error
}

// Conn is one DevTools connection in flat-session mode: every command and
// event carries the page session it belongs to. Commands are multiplexed
// by id; events go to every subscriber. The reader goroutine is
// panic-fenced — a panic ends the connection, never the process.
type Conn struct {
	ws     *wsConn
	nextID atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan response
	subs    map[int]func(Event)
	nextSub int
	err     error

	done chan struct{}
	once sync.Once
}

// Dial connects to a browser-level WebSocket URL.
func Dial(ctx context.Context, wsURL string) (*Conn, error) {
	ws, err := dialWS(ctx, wsURL)
	if err != nil {
		return nil, err
	}
	c := &Conn{ws: ws, pending: map[int64]chan response{}, subs: map[int]func(Event){}, done: make(chan struct{})}
	go c.readLoop()
	return c, nil
}

type envelope struct {
	ID        int64           `json:"id"`
	Result    json.RawMessage `json:"result"`
	Error     *cdpError       `json:"error"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	SessionID string          `json:"sessionId"`
}

func (c *Conn) readLoop() {
	defer func() {
		if r := recover(); r != nil {
			c.fail(fmt.Errorf("browser connection reader panicked: %v", r))
		}
	}()
	for {
		msg, err := c.ws.ReadMessage()
		if err != nil {
			c.fail(err)
			return
		}
		var env envelope
		if json.Unmarshal(msg, &env) != nil {
			continue // one unreadable message is not worth the connection
		}
		if env.ID != 0 {
			c.mu.Lock()
			ch := c.pending[env.ID]
			delete(c.pending, env.ID)
			c.mu.Unlock()
			if ch != nil {
				r := response{result: env.Result}
				if env.Error != nil {
					r.err = env.Error
				}
				ch <- r
			}
			continue
		}
		if env.Method == "" {
			continue
		}
		ev := Event{Method: env.Method, SessionID: env.SessionID, Params: env.Params}
		c.mu.Lock()
		subs := make([]func(Event), 0, len(c.subs))
		for _, fn := range c.subs {
			subs = append(subs, fn)
		}
		c.mu.Unlock()
		for _, fn := range subs {
			fn(ev)
		}
	}
}

// fail ends the connection once: records why, closes Done, drops the
// socket and fails every call still waiting.
func (c *Conn) fail(err error) {
	c.once.Do(func() {
		if err == nil || errors.Is(err, io.EOF) {
			err = ErrClosed
		}
		c.mu.Lock()
		c.err = err
		pending := c.pending
		c.pending = map[int64]chan response{}
		c.mu.Unlock()
		close(c.done)
		c.ws.c.Close()
		for _, ch := range pending {
			ch <- response{err: ErrClosed}
		}
	})
}

// Call sends one command and waits for its reply, ctx, or the end of the
// connection. result, when non-nil, receives the decoded result. A
// subscriber must never call Call: subscribers run on the reader goroutine,
// which is the one that would have to read the reply.
func (c *Conn) Call(ctx context.Context, sessionID, method string, params, result any) error {
	select {
	case <-c.done:
		return c.Err()
	default:
	}
	if params == nil {
		params = struct{}{}
	}
	id := c.nextID.Add(1)
	req := map[string]any{"id": id, "method": method, "params": params}
	if sessionID != "" {
		req["sessionId"] = sessionID
	}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	ch := make(chan response, 1)
	c.mu.Lock()
	c.pending[id] = ch
	c.mu.Unlock()
	if err := c.ws.WriteMessage(b); err != nil {
		c.fail(err)
		return ErrClosed
	}
	select {
	case r := <-ch:
		if r.err != nil {
			return fmt.Errorf("%s: %w", method, r.err)
		}
		if result != nil && len(r.result) > 0 {
			if err := json.Unmarshal(r.result, result); err != nil {
				return fmt.Errorf("%s: decoding the result: %w", method, err)
			}
		}
		return nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case <-c.done:
		return c.Err()
	}
}

// Subscribe adds an event handler. It runs on the reader goroutine: it must
// return quickly and must never call Call.
func (c *Conn) Subscribe(fn func(Event)) (cancel func()) {
	c.mu.Lock()
	id := c.nextSub
	c.nextSub++
	c.subs[id] = fn
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		delete(c.subs, id)
		c.mu.Unlock()
	}
}

// Done is closed when the connection has ended.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err is why the connection ended, or nil while it is open.
func (c *Conn) Err() error {
	select {
	case <-c.done:
	default:
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// Close ends the connection with a normal closure.
func (c *Conn) Close() error {
	_ = c.ws.writeFrame(opClose, []byte{0x03, 0xE8})
	c.fail(ErrClosed)
	return nil
}
