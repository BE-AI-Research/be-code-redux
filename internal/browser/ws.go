// Package browser drives a Chromium over the DevTools protocol: a client-side
// WebSocket, a multiplexed protocol connection, discovery and launch, and a
// Page the model acts on through accessibility-tree snapshots (spec
// docs/superpowers/specs/2026-09-25-browser-design.md). It has no UI and
// imports nothing from the agent, the tools or the UIs.
package browser

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// maxMessageBytes bounds one assembled message. A whole accessibility tree
// of a large page runs to a few megabytes; anything past this is refused
// from its header rather than allocated. A var so a test can lower it.
var maxMessageBytes int64 = 64 << 20

var errTooLarge = errors.New("websocket: message larger than the limit")

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	opContinuation = 0x0
	opText         = 0x1
	opBinary       = 0x2
	opClose        = 0x8
	opPing         = 0x9
	opPong         = 0xA
)

// aLongTimeAgo is a deadline in the past: setting it unblocks any read or
// write on the connection at once.
var aLongTimeAgo = time.Unix(1, 0)

// wsConn is the client side of one WebSocket (RFC 6455): text messages out,
// whole messages in, pings answered. Reads come from one goroutine (the
// protocol connection's reader); writes may come from any.
type wsConn struct {
	c   net.Conn
	br  *bufio.Reader
	wmu sync.Mutex
}

// dialWS opens ws://host[:port]/path. Only ws:// is supported: a DevTools
// endpoint is plain, and one off this machine must be allowed explicitly
// (browser.allow_remote). The handshake honours ctx, so a listener that
// accepts and never answers cannot hang a tool call.
func dialWS(ctx context.Context, rawURL string) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("websocket url %q: %w", rawURL, err)
	}
	if u.Scheme != "ws" {
		return nil, fmt.Errorf("websocket url %q: only ws:// is supported", rawURL)
	}
	hostport := u.Host
	if u.Port() == "" {
		hostport = net.JoinHostPort(u.Hostname(), "80")
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", hostport)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { c.SetDeadline(aLongTimeAgo) })

	keyBytes := make([]byte, 16)
	if _, err := rand.Read(keyBytes); err != nil {
		stop()
		c.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyBytes)
	req := "GET " + u.RequestURI() + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	fail := func(err error) (*wsConn, error) {
		stop()
		c.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	if _, err := io.WriteString(c, req); err != nil {
		return fail(err)
	}
	br := bufio.NewReader(c)
	// A 101 response has no body (Go treats 1xx as bodiless), so br keeps
	// whatever frames the server sent straight after the handshake.
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodGet})
	if err != nil {
		return fail(fmt.Errorf("websocket handshake: %w", err))
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return fail(fmt.Errorf("websocket handshake: %s", resp.Status))
	}
	if resp.Header.Get("Sec-WebSocket-Accept") != acceptKey(key) {
		return fail(errors.New("websocket handshake: bad Sec-WebSocket-Accept"))
	}
	// stop() false means ctx has already fired and its deadline is set (or
	// about to be): the connection is unusable, so report the context.
	if !stop() {
		c.Close()
		return nil, ctx.Err()
	}
	c.SetDeadline(time.Time{})
	return &wsConn{c: c, br: br}, nil
}

func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// WriteMessage sends one whole text message.
func (w *wsConn) WriteMessage(p []byte) error { return w.writeFrame(opText, p) }

func (w *wsConn) writeFrame(op byte, p []byte) error {
	w.wmu.Lock()
	defer w.wmu.Unlock()
	n := len(p)
	hdr := make([]byte, 0, 14)
	hdr = append(hdr, 0x80|op)
	switch {
	case n <= 125:
		hdr = append(hdr, 0x80|byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 0x80|126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 0x80|127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return err
	}
	frame := append(hdr, mask[:]...)
	for i := 0; i < n; i++ {
		frame = append(frame, p[i]^mask[i%4])
	}
	_, err := w.c.Write(frame)
	return err
}

// ReadMessage returns the next whole message, reassembling fragments and
// answering pings on the way. A close frame from the server is io.EOF.
func (w *wsConn) ReadMessage() ([]byte, error) {
	var msg []byte
	started := false
	for {
		fin, op, payload, err := w.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case opPing:
			if err := w.writeFrame(opPong, payload); err != nil {
				return nil, err
			}
			continue
		case opPong:
			continue
		case opClose:
			_ = w.writeFrame(opClose, nil) // echo, best effort
			return nil, io.EOF
		case opText, opBinary:
			if started {
				return nil, errors.New("websocket: a new message inside a fragmented one")
			}
			started, msg = true, payload
		case opContinuation:
			if !started {
				return nil, errors.New("websocket: a continuation with no message")
			}
			if int64(len(msg))+int64(len(payload)) > maxMessageBytes {
				return nil, errTooLarge
			}
			msg = append(msg, payload...)
		default:
			return nil, fmt.Errorf("websocket: unknown opcode %#x", op)
		}
		if fin {
			return msg, nil
		}
	}
}

func (w *wsConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(w.br, h[:]); err != nil {
		return
	}
	fin, op = h[0]&0x80 != 0, h[0]&0x0F
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(w.br, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(w.br, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > uint64(maxMessageBytes) {
		err = errTooLarge
		return
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(w.br, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(w.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

// Close sends a normal-closure frame and drops the connection.
func (w *wsConn) Close() error {
	_ = w.writeFrame(opClose, []byte{0x03, 0xE8})
	return w.c.Close()
}
