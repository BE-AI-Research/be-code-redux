package diff

import (
	"strings"
	"testing"
)

func TestSimpleEdit(t *testing.T) {
	oldT := "a\nb\nc\nd\ne\nf\ng\n"
	newT := "a\nb\nc\nX\ne\nf\ng\n"
	hunks := Unified(oldT, newT, 2)
	if len(hunks) != 1 {
		t.Fatalf("hunks = %d", len(hunks))
	}
	out := Render(hunks, false)
	if !strings.Contains(out, "-d") || !strings.Contains(out, "+X") {
		t.Fatalf("diff missing change:\n%s", out)
	}
	adds, dels := Stats(hunks)
	if adds != 1 || dels != 1 {
		t.Fatalf("stats +%d -%d", adds, dels)
	}
}

func TestSeparateHunks(t *testing.T) {
	var a, b []string
	for i := 0; i < 40; i++ {
		a = append(a, "line")
		b = append(b, "line")
	}
	b[2] = "top-change"
	b[37] = "bottom-change"
	hunks := Unified(strings.Join(a, "\n"), strings.Join(b, "\n"), 2)
	if len(hunks) != 2 {
		t.Fatalf("expected 2 hunks, got %d:\n%s", len(hunks), Render(hunks, false))
	}
}

func TestNewFilePreview(t *testing.T) {
	out := Preview("x.go", "", "package x\nfunc F() {}\n", false)
	if !strings.Contains(out, "+package x") || !strings.Contains(out, "new file") {
		t.Fatalf("preview:\n%s", out)
	}
}

func TestNoChanges(t *testing.T) {
	out := Preview("x.go", "same\n", "same\n", false)
	if !strings.Contains(out, "no changes") {
		t.Fatalf("preview: %s", out)
	}
}

func TestIdenticalRoundTrip(t *testing.T) {
	oldT := "one\ntwo\nthree\n"
	if hunks := Unified(oldT, oldT, 3); len(hunks) != 0 {
		t.Fatalf("identical inputs produced %d hunks", len(hunks))
	}
}

func TestApplyReconstruction(t *testing.T) {
	// The diff must describe the exact transformation: replaying + and ' '
	// lines yields the new text; replaying - and ' ' yields the old text
	// (within hunk ranges). Sanity-check counts line up.
	oldT := "a\nb\nc\nd\n"
	newT := "a\nc\nd\ne\nf\n"
	hunks := Unified(oldT, newT, 1)
	adds, dels := Stats(hunks)
	oldLines, newLines := 4, 5
	if oldLines-dels+adds != newLines {
		t.Fatalf("line accounting broken: %d - %d + %d != %d\n%s",
			oldLines, dels, adds, newLines, Render(hunks, false))
	}
}
