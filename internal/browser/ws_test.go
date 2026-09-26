package browser

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/browser/browsertest"
)

func wsServer(t *testing.T, handle func(sc *browsertest.WSConn)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sc, err := browsertest.Upgrade(w, r)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer sc.Close()
		handle(sc)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func wsURLFor(srv *httptest.Server) string {
	return "ws" + strings.TrimPrefix(srv.URL, "http") + "/devtools/browser/x"
}

func dialTest(t *testing.T, srv *httptest.Server) *wsConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialWS(ctx, wsURLFor(srv))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestWSRoundTripMasksClientFrames(t *testing.T) {
	got := make(chan []byte, 1)
	masked := make(chan bool, 1)
	srv := wsServer(t, func(sc *browsertest.WSConn) {
		_, _, p, m, err := sc.ReadFrame()
		if err != nil {
			t.Error(err)
			return
		}
		got <- p
		masked <- m
		sc.WriteMessage([]byte("world"))
		sc.ReadMessage() // until the client closes
	})
	c := dialTest(t, srv)
	defer c.Close()
	if err := c.WriteMessage([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if p := <-got; string(p) != "hello" {
		t.Fatalf("server got %q", p)
	}
	if !<-masked {
		t.Fatal("client frame was not masked (RFC 6455 §5.3)")
	}
	msg, err := c.ReadMessage()
	if err != nil || string(msg) != "world" {
		t.Fatalf("read %q, %v", msg, err)
	}
}

func TestWSReassemblesFragmentsAndAnswersPings(t *testing.T) {
	pong := make(chan []byte, 1)
	srv := wsServer(t, func(sc *browsertest.WSConn) {
		sc.WriteFrame(browsertest.OpText, false, []byte("hel"))
		sc.WriteFrame(browsertest.OpPing, true, []byte("p1"))
		sc.WriteFrame(browsertest.OpContinuation, true, []byte("lo"))
		if _, op, p, _, err := sc.ReadFrame(); err == nil && op == browsertest.OpPong {
			pong <- p
		}
		sc.ReadMessage()
	})
	c := dialTest(t, srv)
	defer c.Close()
	msg, err := c.ReadMessage()
	if err != nil || string(msg) != "hello" {
		t.Fatalf("read %q, %v", msg, err)
	}
	select {
	case p := <-pong:
		if string(p) != "p1" {
			t.Fatalf("pong carried %q, want the ping's payload", p)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the client never answered the ping")
	}
}

func TestWSLengthEncodings(t *testing.T) {
	srv := wsServer(t, func(sc *browsertest.WSConn) {
		for {
			_, op, p, _, err := sc.ReadFrame()
			if err != nil || op == browsertest.OpClose {
				return
			}
			sc.WriteMessage(p)
		}
	})
	c := dialTest(t, srv)
	defer c.Close()
	for _, n := range []int{0, 125, 126, 65535, 65536, 200000} {
		want := strings.Repeat("x", n)
		if err := c.WriteMessage([]byte(want)); err != nil {
			t.Fatalf("write %d: %v", n, err)
		}
		got, err := c.ReadMessage()
		if err != nil || string(got) != want {
			t.Fatalf("echo of %d bytes came back as %d bytes, %v", n, len(got), err)
		}
	}
}

func TestWSCloseFrameIsEOF(t *testing.T) {
	srv := wsServer(t, func(sc *browsertest.WSConn) {
		sc.WriteFrame(browsertest.OpClose, true, []byte{0x03, 0xE8})
		sc.ReadMessage()
	})
	c := dialTest(t, srv)
	defer c.Close()
	if _, err := c.ReadMessage(); err != io.EOF {
		t.Fatalf("close frame gave %v, want io.EOF", err)
	}
}

func TestWSDroppedConnectionIsAnError(t *testing.T) {
	srv := wsServer(t, func(sc *browsertest.WSConn) {}) // returns at once: dropped
	c := dialTest(t, srv)
	defer c.Close()
	if _, err := c.ReadMessage(); err == nil {
		t.Fatal("a dropped connection read as a message")
	}
}

func TestWSRejectsBadAccept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sc, err := browsertest.UpgradeWithAccept(w, r, "not-the-right-value"); err == nil {
			defer sc.Close()
			sc.ReadMessage()
		}
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := dialWS(ctx, wsURLFor(srv))
	if err == nil || !strings.Contains(err.Error(), "Sec-WebSocket-Accept") {
		t.Fatalf("bad accept gave %v", err)
	}
}

func TestWSRefusesOversizedMessage(t *testing.T) {
	old := maxMessageBytes
	maxMessageBytes = 1024
	defer func() { maxMessageBytes = old }()
	srv := wsServer(t, func(sc *browsertest.WSConn) {
		// A frame header announcing 2^32 bytes, and no payload: the client
		// must refuse it from the header alone, never allocate it.
		sc.WriteRaw([]byte{0x81, 127, 0, 0, 0, 1, 0, 0, 0, 0})
		sc.ReadMessage()
	})
	c := dialTest(t, srv)
	defer c.Close()
	if _, err := c.ReadMessage(); err != errTooLarge {
		t.Fatalf("oversized frame gave %v, want errTooLarge", err)
	}
}

func TestWSHandshakeHonoursContext(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer c.Close()
			time.Sleep(5 * time.Second) // accept, never answer
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := dialWS(ctx, "ws://"+ln.Addr().String()+"/x"); err == nil {
		t.Fatal("a silent listener completed the handshake")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("the handshake ignored its context for %s", time.Since(start))
	}
}

func TestWSOnlyPlainWS(t *testing.T) {
	if _, err := dialWS(context.Background(), "wss://127.0.0.1:1/x"); err == nil {
		t.Fatal("wss:// was accepted")
	}
}
