package live

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteFrame(&buf, FInput, []byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(&buf, FSize, Size{Cols: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	typ, payload, err := ReadFrame(&buf)
	if err != nil || typ != FInput || string(payload) != "abc" {
		t.Fatalf("first frame: %v %v %q", typ, err, payload)
	}
	typ, payload, err = ReadFrame(&buf)
	var s Size
	if err != nil || typ != FSize || json.Unmarshal(payload, &s) != nil || s.Cols != 80 || s.Rows != 24 {
		t.Fatalf("second frame: %v %v %q", typ, err, payload)
	}
	if _, _, err = ReadFrame(&buf); err != io.EOF {
		t.Fatalf("expected EOF, got %v", err)
	}
}

func TestFrameRejectsOversize(t *testing.T) {
	var buf bytes.Buffer
	buf.Write([]byte{byte(FInput), 0xff, 0xff, 0xff, 0xff})
	if _, _, err := ReadFrame(&buf); err == nil || err == io.EOF {
		t.Fatal("oversize frame must be rejected")
	}
}
