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

// A pack's verify command is deny-checked and never prompted for, so for a
// pack check the deny list is the only control there is — unlike a model's
// shell command, which has an approval prompt behind it. argv[0] was matched
// only as written, so a deny glob aimed at a program name missed every other
// spelling of the same program.
func TestCheckDeniedMatchesTheProgramHoweverItIsSpelled(t *testing.T) {
	r := &Registry{ShellDeny: []string{"sudo *", "*curl*"}}

	for _, argv := range [][]string{
		{"sudo", "rm", "-rf", "/"},
		{"/usr/bin/sudo", "rm", "-rf", "/"},
		{"./sudo", "rm", "-rf", "/"},
		{"../bin/sudo", "rm", "-rf", "/"},
	} {
		display := strings.Join(argv, " ")
		if !r.CheckDenied(display, argv) {
			t.Errorf("not denied: %v", argv)
		}
	}
}

// A glob that names a path still works on the path, and an unrelated program
// is still allowed: widening the match must not start denying everything.
func TestCheckDeniedStillAllowsWhatItShould(t *testing.T) {
	r := &Registry{ShellDeny: []string{"sudo *", "*curl*"}}
	for _, argv := range [][]string{
		{"/home/me/pack/tools/ue-build", "--project", "/ws"},
		{"go", "build", "./..."},
		{"npm", "test"},
	} {
		if r.CheckDenied(strings.Join(argv, " "), argv) {
			t.Errorf("wrongly denied: %v", argv)
		}
	}
}

// A path-shaped deny glob must keep matching the full path, which is how the
// existing rules are written.
func TestCheckDeniedStillMatchesAPathGlob(t *testing.T) {
	r := &Registry{ShellDeny: []string{"*/tools/ue-build*"}}
	argv := []string{"/home/me/pack/tools/ue-build", "--project", "/ws"}
	if !r.CheckDenied(strings.Join(argv, " "), argv) {
		t.Error("a path glob stopped matching the full path")
	}
}
