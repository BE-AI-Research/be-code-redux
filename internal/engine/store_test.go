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

// TestClearKeepsTheDocumentsAndDropsTheTasks: ruling T3-a. "/task clear"
// closes the work; it does not delete files in the user's project.
func TestClearKeepsTheDocumentsAndDropsTheTasks(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	s, err := OpenAt(dir, root, "s1", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	id := s.Plan("a task", []string{"one"})
	if err := s.SetStatus(id+".1", StatusDoing, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".be-code", "tasks", "001-a-task.md")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("document not written: %v", err)
	}

	s.ClearSession()
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("/task clear deleted the document: %v", err)
	}
	tr := s.Tree()
	if len(tr.Roots) != 1 || tr.Roots[0].Status != StatusDropped || tr.Roots[0].Reason != "cleared" {
		t.Fatalf("roots after clear: %+v", tr.Roots)
	}
	if tr.Find(id+".1").Status != StatusDropped {
		t.Fatalf("step after clear: %+v", tr.Find(id+".1"))
	}
	// The old block has nothing left to describe, which is what the UI
	// asserts when the user types "/task clear".
	if s.Ledger().Task != "" {
		t.Fatalf("clear left a task line: %q", s.Ledger().Task)
	}
	// And the record is still there to read tomorrow.
	again, err := OpenAt(dir, root, "s2", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Tree().Roots) != 1 {
		t.Fatalf("cleared record did not survive a reopen: %+v", again.Tree().Roots)
	}
}

// TestASecondTaskAddedByHandKeepsTheFirstsDocument: two roots arriving from
// one document must not end up writing over each other.
func TestASecondTaskAddedByHandKeepsTheFirstsDocument(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	tasks := filepath.Join(root, ".be-code", "tasks")
	os.MkdirAll(tasks, 0o755)
	os.WriteFile(filepath.Join(tasks, "001-alpha.md"),
		[]byte("# 001 — alpha\n\n- [ ] 1. alpha\n- [ ] 2. beta\n"), 0o644)

	s, err := OpenAt(dir, root, "s1", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Tree().Roots) != 2 {
		t.Fatalf("roots: %+v", s.Tree().Roots)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	again, err := OpenAt(dir, root, "s2", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	tr := again.Tree()
	if len(tr.Roots) != 2 || tr.Roots[0].Text != "alpha" || tr.Roots[1].Text != "beta" {
		t.Fatalf("a task was lost writing its sibling: %+v", tr.Roots)
	}
}

// TestATaskKeepsItsDocumentWhenItsTitleChanges: the engine never renames or
// removes a document, so a retitled task cannot leave a second file behind
// to reload as a duplicate.
func TestATaskKeepsItsDocumentWhenItsTitleChanges(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	s, _ := OpenAt(dir, root, "s1", false, testLimits())
	s.StartTask("first thing")
	s.Flush()
	s.StartTask("second thing")
	s.Flush()

	ents, _ := os.ReadDir(filepath.Join(root, ".be-code", "tasks"))
	var docs []string
	for _, e := range ents {
		if e.Name() != "README.md" {
			docs = append(docs, e.Name())
		}
	}
	if len(docs) != 1 || docs[0] != "001-first-thing.md" {
		t.Fatalf("documents: %v", docs)
	}
	b, _ := os.ReadFile(filepath.Join(root, ".be-code", "tasks", docs[0]))
	if !strings.Contains(string(b), "# 001 — second thing") {
		t.Fatalf("heading did not follow the title:\n%s", b)
	}
	again, _ := OpenAt(dir, root, "s2", false, testLimits())
	if tr := again.Tree(); len(tr.Roots) != 1 || tr.Roots[0].Text != "second thing" {
		t.Fatalf("roots: %+v", tr.Roots)
	}
}

// TestAnEditedFileIsNotReportedAsAlreadyRead: edit_file replaces a fragment
// of a file the model may never have read, so it must not answer a later
// read with a footer. write_file does imply the whole file.
func TestAnEditedFileIsNotReportedAsAlreadyRead(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	s, _ := OpenAt(dir, root, "s1", false, testLimits())
	src := "package a\nvar X = 1\n"
	writeFile(t, root, "a.go", src)
	writeFile(t, root, "b.go", src)
	s.EnsureRoot("touch some files")
	s.NextTurn()

	s.Observe(Event{Tool: "edit_file", Args: map[string]any{"path": "a.go"}, Content: "edited a.go"})
	s.NextTurn()
	if f := s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "a.go"}, Content: numbered(src, 1)}); f != "" {
		t.Fatalf("a file that was only edited was reported as already read: %q", f)
	}
	s.Observe(Event{Tool: "write_file", Args: map[string]any{"path": "b.go"}, Content: "wrote b.go"})
	s.NextTurn()
	if f := s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "b.go"}, Content: numbered(src, 1)}); f == "" {
		t.Fatal("a file the model wrote itself should read as already known")
	}
}

// TestTheRedundantReadFooterSurvivesAReopen: ruling T3-b is what makes
// ruling T2-b's cross-session promise true — the hash and the outline a
// Markdown document cannot carry come back from state.json.
func TestTheRedundantReadFooterSurvivesAReopen(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	src := "package a\nfunc A() {}\n"
	writeFile(t, root, "a.go", src)
	s, _ := OpenAt(dir, root, "s1", false, testLimits())
	s.EnsureRoot("look at a.go")
	s.NextTurn()
	if f := s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "a.go"}, Content: numbered(src, 1)}); f != "" {
		t.Fatalf("first read footer: %q", f)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	again, err := OpenAt(dir, root, "s2", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	again.NextTurn()
	f := again.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "a.go"}, Content: numbered(src, 1)})
	if !strings.Contains(f, "already read at turn 1") {
		t.Fatalf("footer lost across the reopen: %q", f)
	}
	if ds := again.Digests(); len(ds) != 1 || len(ds[0].Outline) == 0 {
		t.Fatalf("outline lost across the reopen: %+v", ds)
	}
}

// TestGitignoreEdgeCases: the line we add must not join theirs, we never
// invent a .gitignore outside a repository, and another spelling of the
// same entry is recognised rather than duplicated.
func TestGitignoreEdgeCases(t *testing.T) {
	planAndFlush := func(root string) {
		t.Helper()
		s, err := OpenAt(t.TempDir(), root, "s1", false, testLimits())
		if err != nil {
			t.Fatal(err)
		}
		s.Plan("a task", nil)
		if err := s.Flush(); err != nil {
			t.Fatal(err)
		}
	}

	noNewline := t.TempDir()
	os.WriteFile(filepath.Join(noNewline, ".gitignore"), []byte("node_modules/"), 0o644)
	planAndFlush(noNewline)
	if b, _ := os.ReadFile(filepath.Join(noNewline, ".gitignore")); string(b) != "node_modules/\n.be-code/\n" {
		t.Fatalf("no trailing newline: %q", b)
	}

	notARepo := t.TempDir()
	planAndFlush(notARepo)
	if _, err := os.Stat(filepath.Join(notARepo, ".gitignore")); !os.IsNotExist(err) {
		t.Fatal("a .gitignore was invented in a folder that is not a repository")
	}

	repo := t.TempDir()
	os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	planAndFlush(repo)
	if b, err := os.ReadFile(filepath.Join(repo, ".gitignore")); err != nil || string(b) != ".be-code/\n" {
		t.Fatalf("repository without a .gitignore: %q %v", b, err)
	}

	otherSpelling := t.TempDir()
	os.WriteFile(filepath.Join(otherSpelling, ".gitignore"), []byte("/.be-code\n"), 0o644)
	planAndFlush(otherSpelling)
	if b, _ := os.ReadFile(filepath.Join(otherSpelling, ".gitignore")); string(b) != "/.be-code\n" {
		t.Fatalf("an existing entry was duplicated: %q", b)
	}
}
