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

// An old client's Hello has none of the identity fields and must still
// decode; a new one carries them.
func TestHelloIdentityFieldsAreOptional(t *testing.T) {
	var h Hello
	if err := json.Unmarshal([]byte(`{"token":"t","cols":80,"rows":24,"label":"ssh from 1.2.3.4 (pid 9)"}`), &h); err != nil {
		t.Fatal(err)
	}
	if h.IP != "" || h.PID != 0 {
		t.Fatalf("%+v", h)
	}
	b, _ := json.Marshal(Hello{Token: "t", IP: "1.2.3.4", Login: "sb", PID: 9, User: "alice"})
	var back Hello
	json.Unmarshal(b, &back)
	if back.IP != "1.2.3.4" || back.Login != "sb" || back.PID != 9 || back.User != "alice" {
		t.Fatalf("%+v", back)
	}
}

func TestClientIPFromSSHConnection(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "192.168.1.38 51234 192.168.1.237 22")
	if got := ClientIP(); got != "192.168.1.38" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("SSH_CONNECTION", "")
	if got := ClientIP(); got != "127.0.0.1" {
		t.Fatalf("local: got %q", got)
	}
}

func TestClientInfoCarriesIdentity(t *testing.T) {
	c := newClient(Hello{Label: "x", IP: "1.2.3.4", Login: "sb", PID: 9, User: "alice"}, nil)
	c.id = 1
	h := &Host{clients: []*client{c}}
	infos := h.infosLocked()
	if len(infos) != 1 || infos[0].IP != "1.2.3.4" || infos[0].Login != "sb" || infos[0].PID != 9 || infos[0].User != "alice" {
		t.Fatalf("%+v", infos)
	}
}

// An unset config name stays unset: "" is what tells the chat layer to offer
// or ask. sanitizeLabel's "client" fallback is for labels, which must never
// be blank on screen, and would have named every unnamed terminal "client".
func TestAnUnsetUserStaysEmpty(t *testing.T) {
	c := newClient(Hello{Label: "x"}, nil)
	if c.userID != "" {
		t.Fatalf("userID %q", c.userID)
	}
	if c := newClient(Hello{Label: "x", User: "alice"}, nil); c.userID != "alice" {
		t.Fatalf("userID %q", c.userID)
	}
}
