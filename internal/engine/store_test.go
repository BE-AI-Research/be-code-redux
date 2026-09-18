package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testLimits is what every store test opens with: generous enough that no
// cap bites unless the test is about a cap.
func testLimits() Limits { return Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768} }

func openTest(t *testing.T, session string, resumed bool) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(t.TempDir(), "engine")
	s, err := OpenAt(dir, root, session, resumed, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	return s, root
}

// writeFile puts a file in the workspace the store is watching.
func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// numbered renders content the way read_file does, so seenRange sees the
// line numbers the model was actually shown.
func numbered(content string, from int) string {
	var b strings.Builder
	for i, l := range strings.Split(strings.TrimRight(content, "\n"), "\n") {
		fmt.Fprintf(&b, "%5d\t%s\n", from+i, l)
	}
	return b.String()
}

func TestKeyIsStableAndShort(t *testing.T) {
	a, b := Key("/tmp/x/../y"), Key("/tmp/y")
	if a != b || len(a) != 16 {
		t.Fatalf("key %q vs %q", a, b)
	}
}

func TestNotesCapTrimsOldestLines(t *testing.T) {
	s, _ := openTest(t, "s1", false)
	s.lim.NotesCap = 40
	s.AddNoteLine("first line that is long enough")
	s.AddNoteLine("second line that is long enough")
	n := s.Notes()
	if strings.Contains(n, "first") || !strings.Contains(n, "second") || len(n) > 40 {
		t.Fatalf("cap: %q", n)
	}
	if err := s.DropNote(1); err != nil || s.Notes() != "" {
		t.Fatalf("drop: %v %q", err, s.Notes())
	}
	if err := s.DropNote(1); err == nil {
		t.Fatal("drop past end must error")
	}
}

func TestAddNoteLineDedupsWholeLinesOnly(t *testing.T) {
	s, _ := openTest(t, "s1", false)
	s.AddNoteLine("increase timeout for tests")
	s.AddNoteLine("timeout for tests")
	n := s.Notes()
	lines := strings.Split(strings.TrimRight(n, "\n"), "\n")
	if len(lines) != 2 || lines[0] != "increase timeout for tests" || lines[1] != "timeout for tests" {
		t.Fatalf("notes: %q", n)
	}
}

// TestTreeSurvivesReopen: the workspace document is the source of truth, so
// a fresh Store on the same root sees the same tree.
func TestTreeSurvivesReopen(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	s, err := OpenAt(dir, root, "s1", false, Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768})
	if err != nil {
		t.Fatal(err)
	}
	id := s.Plan("fix the parser", []string{"find the bug", "fix and verify"})
	if err := s.SetStatus(id+".1", StatusDoing, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenAt(dir, root, "s2", false, Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768})
	if err != nil {
		t.Fatal(err)
	}
	tr := s2.Tree()
	if len(tr.Roots) != 1 || len(tr.Roots[0].Children) != 2 {
		t.Fatalf("tree: %+v", tr.Roots)
	}
	if tr.Find(id+".1").Status != StatusDoing {
		t.Fatal("status did not survive")
	}
}

// TestQuarantineRatherThanOverwrite: a document edited into nonsense is
// moved aside with its content intact, and the session continues.
func TestQuarantineRatherThanOverwrite(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	s, _ := OpenAt(dir, root, "s1", false, Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768})
	s.Plan("a task", []string{"one"})
	s.Flush()
	path := filepath.Join(root, ".be-code", "tasks", "001-a-task.md")
	os.WriteFile(path, []byte("# 001 — a task\n\n- [?] not a status\n"), 0o644)
	if _, err := OpenAt(dir, root, "s2", false, Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768}); err != nil {
		t.Fatalf("a broken document must not fail the open: %v", err)
	}
	glob, _ := filepath.Glob(filepath.Join(root, ".be-code", "tasks", "*.broken-*.md"))
	if len(glob) != 1 {
		t.Fatalf("quarantine files: %v", glob)
	}
	b, _ := os.ReadFile(glob[0])
	if !strings.Contains(string(b), "not a status") {
		t.Fatal("quarantined content was not preserved")
	}
}

// TestGitignoreGetsTheToolFolderOnce: the record stays out of the user's
// commits unless they choose otherwise, and we never write the line twice.
func TestGitignoreGetsTheToolFolderOnce(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/\n"), 0o644)
	dir := t.TempDir()
	s, _ := OpenAt(dir, root, "s1", false, Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768})
	s.Plan("a task", nil)
	s.Flush()
	s.Plan("another", nil)
	s.Flush()
	b, _ := os.ReadFile(filepath.Join(root, ".gitignore"))
	if strings.Count(string(b), ".be-code/") != 1 {
		t.Fatalf(".gitignore: %q", b)
	}
	if !strings.Contains(string(b), "node_modules/") {
		t.Fatal("existing .gitignore content was lost")
	}
}
