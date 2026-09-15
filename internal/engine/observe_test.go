package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func numbered(content string, from int) string {
	var b strings.Builder
	for i, l := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		fmt.Fprintf(&b, "%5d\t%s\n", from+i, l)
	}
	return b.String()
}

func TestObserveReadDigestsAndFlagsRedundantReads(t *testing.T) {
	s, root := openTest(t, "s1", false)
	src := "package x\n\nfunc A() {}\n\nfunc B() {}\n"
	writeFile(t, root, "x.go", src)
	s.NextTurn()
	ev := Event{Tool: "read_file", Args: map[string]any{"path": "x.go"}, Content: numbered(src, 1)}
	if f := s.Observe(ev); f != "" {
		t.Fatalf("first read footer %q", f)
	}
	d := s.Digests()[0]
	if d.Path != "x.go" || len(d.Outline) != 2 || d.Ranges[0] != (Range{1, 5}) || d.Turn != 1 {
		t.Fatalf("digest %+v", d)
	}
	s.NextTurn()
	if f := s.Observe(ev); f != "already read at turn 1 (unchanged); outline and notes are in your context" {
		t.Fatalf("second read footer %q", f)
	}
	// A range not yet seen is not redundant.
	writeFile(t, root, "big.go", strings.Repeat("x\n", 50))
	s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "big.go", "offset": 1, "limit": 10}, Content: numbered(strings.Repeat("x\n", 10), 1)})
	if f := s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "big.go", "offset": 11, "limit": 10}, Content: numbered(strings.Repeat("x\n", 10), 11)}); f != "" {
		t.Fatalf("unseen range flagged: %q", f)
	}
	if f := s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "big.go", "offset": 5, "limit": 10}, Content: numbered(strings.Repeat("x\n", 10), 5)}); f == "" {
		t.Fatal("covered range not flagged")
	}
	// A changed file is never redundant and the digest is refreshed.
	writeFile(t, root, "x.go", src+"\nfunc C() {}\n")
	if f := s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "x.go"}, Content: numbered(src+"\nfunc C() {}\n", 1)}); f != "" {
		t.Fatalf("changed file flagged: %q", f)
	}
	if len(s.Digests()[0].Outline) != 3 {
		t.Fatal("outline not refreshed")
	}
	// Errors are ignored.
	if f := s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "nope.go"}, Content: "open: no such file", IsError: true}); f != "" {
		t.Fatal("error observed")
	}
}

func TestObserveWriteMarksEditedAndResetsRanges(t *testing.T) {
	s, root := openTest(t, "s1", false)
	writeFile(t, root, "y.go", "package y\n")
	s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "y.go"}, Content: numbered("package y\n", 1)})
	writeFile(t, root, "y.go", "package y\n\nfunc New() {}\n")
	s.Observe(Event{Tool: "write_file", Args: map[string]any{"path": "y.go", "content": "package y\n\nfunc New() {}\n"}, Content: "wrote y.go"})
	d := s.Digests()[0]
	if !d.Edited || d.Outline[0] != "New" || d.Ranges[0] != (Range{1, 3}) {
		t.Fatalf("digest after write %+v", d)
	}
}

func TestLookupCacheServesUnchangedRepeats(t *testing.T) {
	s, root := openTest(t, "s1", false)
	writeFile(t, root, "a.go", "package a\nfunc Hit() {}\n")
	args := map[string]any{"pattern": "Hit", "glob": "*.go"}
	if _, ok := s.Cached("search", args); ok {
		t.Fatal("cache hit before any lookup")
	}
	s.Observe(Event{Tool: "search", Args: args, Content: "a.go:2:func Hit() {}"})
	got, ok := s.Cached("search", map[string]any{"glob": "*.go", "pattern": "Hit"}) // key order irrelevant
	if !ok || !strings.HasSuffix(got, "\n(cached; files unchanged)") || !strings.HasPrefix(got, "a.go:2:") {
		t.Fatalf("cached: %v %q", ok, got)
	}
	writeFile(t, root, "a.go", "package a\nfunc Hit() {}\nfunc Hit2() {}\n")
	if _, ok := s.Cached("search", args); ok {
		t.Fatal("cache served after the file changed")
	}
	if len(s.Lookups()) != 1 || s.Lookups()[0].Hits[0].Line != 2 {
		t.Fatalf("lookups %+v", s.Lookups())
	}
	// A zero-hit search is never cached: a repeat is not served from it,
	// even though nothing invalidates an empty Hashes map.
	zargs := map[string]any{"pattern": "NoSuchSymbol"}
	s.Observe(Event{Tool: "search", Args: zargs, Content: "no matches"})
	if _, ok := s.Cached("search", zargs); ok {
		t.Fatal("zero-hit search served from cache")
	}
}

func TestObserveReadRebuildKeepsEditedFlag(t *testing.T) {
	s, root := openTest(t, "s1", false)
	writeFile(t, root, "y.go", "package y\n")
	s.Observe(Event{Tool: "write_file", Args: map[string]any{"path": "y.go", "content": "package y\n"}, Content: "wrote y.go"})
	if !s.Digests()[0].Edited {
		t.Fatal("write did not mark edited")
	}
	// The file changes again (e.g. a verify/format pass) and is re-read;
	// the digest is rebuilt but must still remember it was edited.
	writeFile(t, root, "y.go", "package y\n\nfunc New() {}\n")
	s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "y.go"}, Content: numbered("package y\n\nfunc New() {}\n", 1)})
	if !s.Digests()[0].Edited {
		t.Fatal("rebuilt digest lost the edited flag")
	}
}

func TestAddNoteWithFileTouchesAndCapsDigests(t *testing.T) {
	s, root := openTest(t, "s1", false)
	writeFile(t, root, "old.go", "package old\n")
	s.NextTurn()
	s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "old.go"}, Content: numbered("package old\n", 1)})
	// A note-created digest at the same turn must sort ahead of the read,
	// since it was touched later.
	if err := s.AddNote("new fact", "new.go", false, false); err != nil {
		t.Fatal(err)
	}
	if got := s.Digests()[0].Path; got != "new.go" {
		t.Fatalf("touch order: got %q first, want new.go", got)
	}
	// Note-created digests are routed through putDigest, so the cap applies.
	for i := 0; i < maxDigests+5; i++ {
		if err := s.AddNote("f", fmt.Sprintf("n%d.go", i), false, false); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.Digests()) != maxDigests {
		t.Fatalf("digests = %d, want capped at %d", len(s.Digests()), maxDigests)
	}
}

func TestParseHitsIgnoresContextLines(t *testing.T) {
	hits := parseHits("a.go:2:func Hit() {}\na.go-3-// context, not a hit\n")
	if len(hits) != 1 || hits[0].Line != 2 {
		t.Fatalf("hits %+v", hits)
	}
}
