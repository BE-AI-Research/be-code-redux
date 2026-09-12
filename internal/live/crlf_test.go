package live

import (
	"bytes"
	"testing"
)

func TestCRLFWriterTranslatesBareLF(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain\n", "plain\r\n"},
		{"already\r\n", "already\r\n"}, // not doubled
		{"a\nb\nc", "a\r\nb\r\nc"},     // every bare LF
		{"resume: x   (t)\n", "resume: x   (t)\r\n"},
		{"\n\n", "\r\n\r\n"},
		{"no newline", "no newline"},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		w := NewCRLFWriter(&buf)
		n, err := w.Write([]byte(c.in))
		if err != nil {
			t.Fatal(err)
		}
		if n != len(c.in) {
			t.Errorf("Write(%q) = %d, want the caller's own %d", c.in, n, len(c.in))
		}
		if got := buf.String(); got != c.want {
			t.Errorf("Write(%q) wrote %q, want %q", c.in, got, c.want)
		}
	}
}

// A CR and its LF can arrive in separate Write calls (fmt.Fprint of a string
// that ends in "\r" followed by a newline of its own), so the "already CRLF"
// state has to survive across calls.
func TestCRLFWriterCarriesCRAcrossWrites(t *testing.T) {
	var buf bytes.Buffer
	w := NewCRLFWriter(&buf)
	w.Write([]byte("line\r"))
	w.Write([]byte("\nnext\n"))
	if got, want := buf.String(), "line\r\nnext\r\n"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
