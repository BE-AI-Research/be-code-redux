package browsertest

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Handler answers one protocol command: its result, or an error the fake
// returns as a protocol error.
type Handler func(sessionID string, params json.RawMessage) (any, error)

// Call is one command the fake received, in order.
type Call struct {
	Method    string
	SessionID string
	Params    json.RawMessage
}

// Browser is a scripted DevTools endpoint: GET /json/version, and a
// WebSocket at /devtools/browser/fake answering each command from a handler
// table. Commands with no handler succeed with an empty result, so the
// enable calls and the like need no script. A new connection replaces the
// previous one, which is how a reconnect is tested. Commands on one
// connection are answered one at a time, in order, so a handler must never
// wait on another command of the same connection.
type Browser struct {
	t   testing.TB
	Srv *httptest.Server

	mu       sync.Mutex
	handlers map[string]Handler
	calls    []Call
	conn     *WSConn
	closing  bool // set by New's own cleanup, before its own Drop: no
	// connection is ever coming again, so waitForConn must not sit out its
	// full bound finding that out — every other caller still gets the
	// full wait, since only a test's very last Drop can know this.
}

// New starts a fake browser, closed when the test ends.
func New(t testing.TB) *Browser {
	b := &Browser{t: t, handlers: map[string]Handler{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"Browser":              "FakeChrome/1.0",
			"Protocol-Version":     "1.3",
			"webSocketDebuggerUrl": b.WSURL(),
		})
	})
	mux.HandleFunc("/devtools/browser/fake", b.serveWS)
	b.Srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		b.mu.Lock()
		b.closing = true
		b.mu.Unlock()
		b.Drop()
		b.Srv.Close()
	})
	return b
}

// Addr is host:port, what browser.address would be set to.
func (b *Browser) Addr() string { return strings.TrimPrefix(b.Srv.URL, "http://") }

// WSURL is the browser-level WebSocket URL.
func (b *Browser) WSURL() string { return "ws://" + b.Addr() + "/devtools/browser/fake" }

// Handle scripts one command.
func (b *Browser) Handle(method string, h Handler) {
	b.mu.Lock()
	b.handlers[method] = h
	b.mu.Unlock()
}

// Calls returns the commands received for method, in order ("" for all).
func (b *Browser) Calls(method string) []Call {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Call
	for _, c := range b.calls {
		if method == "" || c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// connWait bounds how long Emit and Drop wait for a live connection: a
// client that has just Dial-ed has not necessarily reached the point where
// serveWS has recorded it yet (b.conn is set only after the 101 response is
// written), so calling either right after Dial must not silently race that.
const connWait = 2 * time.Second

// waitForConn polls (bounded by connWait) for a live connection, returning
// it, or nil if none ever arrives — at once if the browser is already
// closing, since New's own cleanup has already said no connection is ever
// coming again.
func (b *Browser) waitForConn() *WSConn {
	deadline := time.Now().Add(connWait)
	for {
		b.mu.Lock()
		sc, closing := b.conn, b.closing
		b.mu.Unlock()
		if sc != nil {
			return sc
		}
		if closing || time.Now().After(deadline) {
			return nil
		}
		time.Sleep(time.Millisecond)
	}
}

// Emit pushes an event to the connected client now, waiting for a
// connection if one has not been recorded yet. A test that calls Emit with
// no connection ever arriving gets an error, not a silent no-op that looks
// like the event was delivered — unless the browser is already closing
// (New's own cleanup, tearing down after the test that owns it has
// returned): nothing can observe the event by then, and the test function
// that could still report a problem may already be finished, so reporting
// one here would panic the whole run instead of just failing a test.
func (b *Browser) Emit(sessionID, method string, params any) {
	sc := b.waitForConn()
	if sc == nil {
		b.mu.Lock()
		closing := b.closing
		b.mu.Unlock()
		if closing {
			return
		}
		b.t.Errorf("browsertest: Emit(%s) with no connection", method)
		return
	}
	m := map[string]any{"method": method, "params": params}
	if sessionID != "" {
		m["sessionId"] = sessionID
	}
	out, _ := json.Marshal(m)
	sc.WriteMessage(out)
}

// EmitSoon pushes an event a moment from now, after the reply to whatever
// command is being answered — the order a real browser uses.
func (b *Browser) EmitSoon(sessionID, method string, params any) {
	go func() {
		time.Sleep(5 * time.Millisecond)
		b.Emit(sessionID, method, params)
	}()
}

// Drop closes the current connection the way a killed browser does, waiting
// for one to exist first (the same race Emit waits out). Unlike Emit,
// dropping nothing is a legitimate no-op: it is not an error to call Drop
// when the browser was never connected to, or has already disconnected.
func (b *Browser) Drop() {
	sc := b.waitForConn()
	if sc == nil {
		return
	}
	b.mu.Lock()
	if b.conn == sc {
		b.conn = nil
	}
	b.mu.Unlock()
	sc.Close()
}

func (b *Browser) serveWS(w http.ResponseWriter, r *http.Request) {
	sc, err := Upgrade(w, r)
	if err != nil {
		b.t.Errorf("browsertest: upgrade: %v", err)
		return
	}
	b.mu.Lock()
	b.conn = sc
	b.mu.Unlock()
	defer func() {
		sc.Close()
		b.mu.Lock()
		if b.conn == sc {
			b.conn = nil
		}
		b.mu.Unlock()
	}()
	for {
		msg, err := sc.ReadMessage()
		if err != nil {
			return
		}
		var req struct {
			ID        int64           `json:"id"`
			Method    string          `json:"method"`
			Params    json.RawMessage `json:"params"`
			SessionID string          `json:"sessionId"`
		}
		if json.Unmarshal(msg, &req) != nil {
			continue
		}
		b.mu.Lock()
		b.calls = append(b.calls, Call{Method: req.Method, SessionID: req.SessionID, Params: req.Params})
		h := b.handlers[req.Method]
		b.mu.Unlock()
		resp := map[string]any{"id": req.ID}
		if req.SessionID != "" {
			resp["sessionId"] = req.SessionID
		}
		if h == nil {
			resp["result"] = map[string]any{}
		} else if res, err := h(req.SessionID, req.Params); err != nil {
			resp["error"] = map[string]any{"code": -32000, "message": err.Error()}
		} else {
			resp["result"] = res
		}
		out, _ := json.Marshal(resp)
		if sc.WriteMessage(out) != nil {
			return
		}
	}
}
