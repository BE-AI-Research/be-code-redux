package tools

import (
	"strings"
	"testing"
)

// A pack's verify command is usually a build and its default timeout is 30
// minutes. A failing verbose Unreal or webpack build emits hundreds of
// megabytes, and all of it used to stay resident in one bytes.Buffer, after
// which the unrecognised-failure path split the whole thing to take 40 lines.
// Head and tail are what matter — compiler errors lead, test failures trail —
// so the middle is what gets dropped.
func TestCappedBufferKeepsHeadAndTail(t *testing.T) {
	var b cappedBuffer
	head := strings.Repeat("A", 1024)
	mid := strings.Repeat("M", 8*1024*1024)
	tail := strings.Repeat("Z", 1024)
	for _, s := range []string{head, mid, tail} {
		if _, err := b.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}

	got := b.String()
	if len(got) > maxCapturedOutput+256 { // +marker
		t.Errorf("output not bounded: %d bytes", len(got))
	}
	if !strings.HasPrefix(got, head) {
		t.Error("the head of the output was lost, which is where a compiler error is")
	}
	if !strings.HasSuffix(got, tail) {
		t.Error("the tail of the output was lost, which is where a test failure is")
	}
	if !strings.Contains(got, "elided") {
		t.Error("nothing says output was dropped, so the model cannot tell it is partial")
	}
}

// Below the cap nothing is touched at all: the common case must be byte-exact,
// since every parser reads this output.
func TestCappedBufferIsExactBelowTheCap(t *testing.T) {
	var b cappedBuffer
	want := "src/a.cpp:42:10: error: boom\nResult: Failed\n"
	if _, err := b.Write([]byte(want)); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != want {
		t.Errorf("output altered below the cap:\n got %q\nwant %q", got, want)
	}
}

// Writing a byte at a time must behave the same as one big write: both
// streams are wired to this and a child process flushes when it likes.
func TestCappedBufferHandlesManySmallWrites(t *testing.T) {
	var b cappedBuffer
	for i := 0; i < maxCapturedOutput*2; i++ {
		if _, err := b.Write([]byte{'x'}); err != nil {
			t.Fatal(err)
		}
	}
	if got := b.String(); len(got) > maxCapturedOutput+256 {
		t.Errorf("output not bounded across small writes: %d bytes", len(got))
	}
}
