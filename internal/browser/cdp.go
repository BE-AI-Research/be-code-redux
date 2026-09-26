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

// ErrClosed is wrapped into the error every call returns once the
// connection has gone — the browser was closed, killed or disconnected —
// so callers can test the reason with errors.Is(err, ErrClosed).
var ErrClosed = errors.New("browser connection closed")

// errReaderPanicked is why a connection whose reader goroutine panicked
// ended (spec §5): the session names it in its reconnect notice.
var errReaderPanicked = errors.New("its reader panicked")

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
			c.fail(fmt.Errorf("browser connection reader panicked: %w: %v", errReaderPanicked, r))
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
// socket and fails every call still waiting. The recorded error always
// wraps ErrClosed (nil or io.EOF collapse to ErrClosed itself; a cause that
// already wraps it, such as Close's own, is kept as is; anything else is
// wrapped as ErrClosed: cause), so errors.Is(err, ErrClosed) holds for
// every error this connection ever hands back once it has ended.
func (c *Conn) fail(err error) {
	c.once.Do(func() {
		switch {
		case err == nil || errors.Is(err, io.EOF):
			err = ErrClosed
		case errors.Is(err, ErrClosed):
			// already carries ErrClosed; keep it as is.
		default:
			err = fmt.Errorf("%w: %w", ErrClosed, err)
		}
		c.mu.Lock()
		c.err = err
		pending := c.pending
		c.pending = map[int64]chan response{}
		c.mu.Unlock()
		close(c.done)
		c.ws.c.Close()
		for _, ch := range pending {
			ch <- response{err: c.err}
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
		return fmt.Errorf("%s: %w", method, c.Err())
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
		return fmt.Errorf("%s: %w", method, c.Err())
	}
	return c.finishCall(ctx, id, ch, method, result)
}

// finishCall waits for ch, ctx, or the end of the connection. A browser can
// answer a command and disconnect right after, so by the time this select
// runs, ch and Done may both already be ready, and select picks between
// ready cases at random: the done-branch must not simply trust Done over an
// already-delivered reply, so it peeks ch once more, non-blockingly, before
// reporting the connection closed.
func (c *Conn) finishCall(ctx context.Context, id int64, ch chan response, method string, result any) error {
	select {
	case r := <-ch:
		return decodeResponse(method, result, r)
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return ctx.Err()
	case <-c.done:
		select {
		case r := <-ch:
			return decodeResponse(method, result, r)
		default:
		}
		return fmt.Errorf("%s: %w", method, c.Err())
	}
}

// decodeResponse turns one reply into a Call result: a protocol error named
// after the method, a decoding error, or the decoded result.
func decodeResponse(method string, result any, r response) error {
	if r.err != nil {
		return fmt.Errorf("%s: %w", method, r.err)
	}
	if result != nil && len(r.result) > 0 {
		if err := json.Unmarshal(r.result, result); err != nil {
			return fmt.Errorf("%s: decoding the result: %w", method, err)
		}
	}
	return nil
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
