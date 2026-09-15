// Package live hosts a running BE-Code TUI session over a local socket so
// other terminals (local, IDE, SSH) can attach to it by resume code. The
// TUI process is the session; clients are thin raw-mode pipes.
package live

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
)

type FrameType byte

const (
	FHello FrameType = iota + 1
	FInput
	FResize
	FDetach
	FOverlay // unused since 0.8.0: was host→client, one client's private input rows
	FQuit
	FOutput
	FSize // unused since 0.8.0: was host→client, the shared minimum size
	FClients
	FBye
)

const maxFrame = 16 << 20

type Hello struct {
	Token string `json:"token"`
	Cols  int    `json:"cols"`
	Rows  int    `json:"rows"`
	Label string `json:"label"`
	UTF8  bool   `json:"utf8"`
	// Control marks a connection that only carries a request (a quit from
	// `be-code sessions kill`) and is never a terminal: the host does not
	// register it, so it never shows up as attached or detached.
	Control bool `json:"control,omitempty"`
}

type Size struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

type ClientInfo struct {
	ID    int    `json:"id"`
	Label string `json:"label"`
	Cols  int    `json:"cols"`
	Rows  int    `json:"rows"`
	UTF8  bool   `json:"utf8"`
}

type Bye struct {
	Reason string `json:"reason"`
}

// WriteFrame writes one frame: type byte, big-endian uint32 length, payload.
func WriteFrame(w io.Writer, t FrameType, payload []byte) error {
	hdr := make([]byte, 5)
	hdr[0] = byte(t)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// WriteJSON writes a frame whose payload is v as JSON.
func WriteJSON(w io.Writer, t FrameType, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return WriteFrame(w, t, b)
}

// ReadFrame reads one frame. A clean end of stream is io.EOF.
func ReadFrame(r io.Reader) (FrameType, []byte, error) {
	hdr := make([]byte, 5)
	if _, err := io.ReadFull(r, hdr); err != nil {
		if err == io.ErrUnexpectedEOF {
			err = io.EOF
		}
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxFrame {
		return 0, nil, errors.New("live: frame too large")
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return FrameType(hdr[0]), payload, nil
}
