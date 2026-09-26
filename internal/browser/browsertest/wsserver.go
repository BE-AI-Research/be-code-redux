// Package browsertest is a scripted stand-in for a Chromium's DevTools
// endpoint, for the tests of internal/browser and internal/tools. It speaks
// the server side of RFC 6455 and a scriptable slice of the protocol. It
// imports nothing from package browser, so it checks that package from the
// outside of its wire format.
package browsertest

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Opcodes, exported so a test can send control frames.
const (
	OpContinuation = 0x0
	OpText         = 0x1
	OpClose        = 0x8
	OpPing         = 0x9
	OpPong         = 0xA
)

// WSConn is the server side of one WebSocket.
type WSConn struct {
	c   net.Conn
	br  *bufio.Reader
	wmu sync.Mutex
}

// Accept computes Sec-WebSocket-Accept for a client key (RFC 6455 §4.2.2).
func Accept(key string) string {
	h := sha1.Sum([]byte(key + wsGUID))
	return base64.StdEncoding.EncodeToString(h[:])
}

// Upgrade completes the handshake with the correct accept value.
func Upgrade(w http.ResponseWriter, r *http.Request) (*WSConn, error) {
	return UpgradeWithAccept(w, r, Accept(r.Header.Get("Sec-WebSocket-Key")))
}

// UpgradeWithAccept completes the handshake answering with accept, which a
// test may get wrong on purpose.
func UpgradeWithAccept(w http.ResponseWriter, r *http.Request, accept string) (*WSConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "not a websocket upgrade", http.StatusBadRequest)
		return nil, errors.New("not a websocket upgrade")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		return nil, errors.New("response writer cannot hijack")
	}
	c, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	resp := "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
	if _, err := io.WriteString(c, resp); err != nil {
		c.Close()
		return nil, err
	}
	return &WSConn{c: c, br: rw.Reader}, nil
}

// ReadFrame reads one client frame and unmasks it; masked reports whether
// the client masked it, which RFC 6455 §5.3 requires.
func (s *WSConn) ReadFrame() (fin bool, op byte, payload []byte, masked bool, err error) {
	var h [2]byte
	if _, err = io.ReadFull(s.br, h[:]); err != nil {
		return
	}
	fin, op, masked = h[0]&0x80 != 0, h[0]&0x0F, h[1]&0x80 != 0
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(s.br, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(s.br, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(s.br, mask[:]); err != nil {
			return
		}
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(s.br, payload); err != nil {
		return
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%4]
		}
	}
	return
}

// ReadMessage returns the next whole message. A close frame ends it with
// io.EOF; pings and pongs are skipped (the client never pings).
func (s *WSConn) ReadMessage() ([]byte, error) {
	var msg []byte
	for {
		fin, op, p, _, err := s.ReadFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case OpClose:
			return nil, io.EOF
		case OpPing, OpPong:
			continue
		}
		msg = append(msg, p...)
		if fin {
			return msg, nil
		}
	}
}

// WriteFrame writes one unmasked server frame.
func (s *WSConn) WriteFrame(op byte, fin bool, payload []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	b0 := op
	if fin {
		b0 |= 0x80
	}
	hdr := []byte{b0}
	n := len(payload)
	switch {
	case n <= 125:
		hdr = append(hdr, byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	_, err := s.c.Write(append(hdr, payload...))
	return err
}

// WriteMessage writes one whole text message.
func (s *WSConn) WriteMessage(p []byte) error { return s.WriteFrame(OpText, true, p) }

// WriteRaw writes bytes as they are, for malformed-frame tests.
func (s *WSConn) WriteRaw(b []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	_, err := s.c.Write(b)
	return err
}

// Close drops the connection without a close frame — what a killed browser
// does.
func (s *WSConn) Close() error { return s.c.Close() }
