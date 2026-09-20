package engine

import (
	"fmt"
	"io"
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
	// Resumed: the same work carries on, doing mark and all. (A session that
	// continues nothing takes that mark back — see
	// TestAFreshSessionOpensItsOwnTask.)
	s2, err := OpenAt(dir, root, "s2", true, Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768})
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

// TestAFreshSessionOpensItsOwnTask is I1. OpenAt's own comment said a session
// id mismatch drops the active node; the code only skipped the verbatim
// buffers, so a new session filed its evidence under — and retitled — the
// task the last one left open.
func TestAFreshSessionOpensItsOwnTask(t *testing.T) {
	root, dir := t.TempDir(), t.TempDir()
	s, _ := OpenAt(dir, root, "s1", false, testLimits())
	id := s.Plan("fix the parser", []string{"find the bug", "fix and verify"})
	s.SetStatus(id+".1", StatusDoing, "")
	s.Observe(Event{Tool: "shell", Args: map[string]any{"command": "go test ./parser"}, Content: "ok\n"})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	fresh, err := OpenAt(dir, root, "s2", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if d := fresh.tree.Doing(); d != nil {
		t.Fatalf("a fresh session inherited the doing node %s", d.ID)
	}
	fresh.StartTask("add a changelog entry")
	got := fresh.EnsureRoot("add a changelog entry")
	if got == id {
		t.Fatalf("the fresh session's request was filed under the previous session's task %s", id)
	}
	fresh.Observe(Event{Tool: "shell", Args: map[string]any{"command": "git log"}, Content: "abc\n"})
	tr := fresh.Tree()
	if old := tr.Find(id); old.Text != "fix the parser" || old.Status.terminal() {
		t.Fatalf("the previous task was retitled or closed: %+v", old)
	}
	if old := tr.Find(id + ".1"); old.Status != StatusTodo || len(old.Evidence.Raw) != 0 {
		t.Fatalf("the previous step is %s with %d raw items; it should be todo and untouched", old.Status, len(old.Evidence.Raw))
	}
	if d := fresh.tree.Doing(); d == nil || !strings.HasPrefix(d.ID, got+".") && d.ID != got {
		t.Fatalf("the new evidence is not under the new task %s: %s", got, fresh.TreeText())
	}
	// Only one new root, however many of the three entry points run.
	if n := len(tr.Roots); n != 2 {
		t.Fatalf("%d roots, want the old task and one new one:\n%s", n, fresh.TreeText())
	}

	// A resumed session is the same work: it keeps its task and its step.
	resumed, err := OpenAt(dir, root, "s3", true, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if d := resumed.tree.Doing(); d == nil {
		t.Fatal("a resumed session lost its doing node")
	}
	before := len(resumed.Tree().Roots)
	resumed.StartTask("carry on")
	resumed.EnsureRoot("carry on")
	if after := len(resumed.Tree().Roots); after != before {
		t.Fatalf("a resumed session opened a new task (%d roots, was %d)", after, before)
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

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = orig
	w.Close()
	b, _ := io.ReadAll(r)
	r.Close()
	return string(b)
}

// TestWarnsOnMultipleDoingMarks (ruling T6-a): a hand-edited status is the
// user's own intent (spec 5.2), so unlike a repaired id or a quarantined
// document, the engine must not touch either document over two [>] marks —
// it only says so, on stderr, naming both documents.
func TestWarnsOnMultipleDoingMarks(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	s, _ := OpenAt(dir, root, "s1", false, testLimits())
	s.Plan("first task", []string{"one", "two"})
	s.Plan("second task", []string{"three"})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	docA := filepath.Join(root, ".be-code", "tasks", "001-first-task.md")
	docB := filepath.Join(root, ".be-code", "tasks", "002-second-task.md")
	beforeA, err := os.ReadFile(docA)
	if err != nil {
		t.Fatal(err)
	}
	beforeB, err := os.ReadFile(docB)
	if err != nil {
		t.Fatal(err)
	}
	// Hand-mark the task itself doing in each document, the way a person
	// would with an editor — the first "[ ]" in each file is that line.
	os.WriteFile(docA, []byte(strings.Replace(string(beforeA), "[ ] ", "[>] ", 1)), 0o644)
	os.WriteFile(docB, []byte(strings.Replace(string(beforeB), "[ ] ", "[>] ", 1)), 0o644)
	afterA, _ := os.ReadFile(docA)
	afterB, _ := os.ReadFile(docB)

	// The warning is handed to whoever owns the store (TakeWarnings), not
	// printed: in a hosted session stderr is a log file nobody opens (I4).
	var s2 *Store
	stderr := captureStderr(t, func() {
		s2, err = OpenAt(dir, root, "s2", false, testLimits())
		if err != nil {
			t.Fatal(err)
		}
	})
	if stderr != "" {
		t.Fatalf("the engine wrote to stderr: %q", stderr)
	}
	warned := strings.Join(s2.TakeWarnings(), "\n")
	if !strings.Contains(warned, "001-first-task.md") || !strings.Contains(warned, "002-second-task.md") {
		t.Fatalf("no warning naming both documents:\n%s", warned)
	}
	if again := s2.TakeWarnings(); len(again) != 0 {
		t.Fatalf("a warning was delivered twice: %v", again)
	}
	// Neither document is touched: no id-repair note, no rewritten status —
	// the files on disk are exactly what the hand edit left them as.
	stillA, _ := os.ReadFile(docA)
	stillB, _ := os.ReadFile(docB)
	if string(stillA) != string(afterA) || string(stillB) != string(afterB) {
		t.Fatal("the engine rewrote a document over a hand-marked doing status")
	}
	n := 0
	tr := s2.Tree()
	tr.Walk(func(node *Node, _ int) {
		if node.Status == StatusDoing {
			n++
		}
	})
	if n != 2 {
		t.Fatalf("both hand-marked steps should still read back doing, got %d", n)
	}
}

// TestHasTaskDocuments: false until a document actually reaches disk, true
// once Flush has written at least one — the test "/task open" uses to tell
// a truthful "nothing here yet" from the directory it will use.
func TestHasTaskDocuments(t *testing.T) {
	s, _ := openTest(t, "s1", false)
	if s.HasTaskDocuments() {
		t.Fatal("no document has been written yet")
	}
	s.Plan("a task", []string{"one"})
	if s.HasTaskDocuments() {
		t.Fatal("a planned-but-unflushed task has not reached disk yet")
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if !s.HasTaskDocuments() {
		t.Fatal("a flushed task should count")
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
	// Nothing is left open, which is what the block and the UI listing
	// both key on when the user types "/task clear".
	if strings.Contains(s.Render(4096, nil), "Active task:") {
		t.Fatalf("clear left an active task:\n%s", s.Render(4096, nil))
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
	// A fresh session files the second read under a task of its own (I1), so
	// the file is recorded once per task; every record of it has its outline.
	fs := filesIn(again)
	if len(fs) == 0 {
		t.Fatal("no file record after the reopen")
	}
	for _, f := range fs {
		if len(f.Outline) == 0 {
			t.Fatalf("outline lost across the reopen: %+v", fs)
		}
	}
}

// TestTheRedundantReadFooterFiresOnAFileBiggerThanTheItemCap: item_cap
// bounds the verbatim buffer, not the record of what was read. A file whose
// numbered output is larger than the cap is the ordinary case, not the
// exotic one, so if the cap could shorten the recorded range the footer
// would effectively never fire in a real session.
func TestTheRedundantReadFooterFiresOnAFileBiggerThanTheItemCap(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	var src strings.Builder
	src.WriteString("package a\n")
	for i := 1; i <= 300; i++ {
		fmt.Fprintf(&src, "// line %d\n", i)
	}
	writeFile(t, root, "a.go", src.String())
	s, _ := OpenAt(dir, root, "s1", false, testLimits())
	s.EnsureRoot("look at a.go")
	s.NextTurn()
	read := Event{Tool: "read_file", Args: map[string]any{"path": "a.go"}, Content: numbered(src.String(), 1)}
	if len(read.Content) <= testLimits().ItemCap {
		t.Fatalf("the test file is not larger than the item cap (%d bytes)", len(read.Content))
	}
	if f := s.Observe(read); f != "" {
		t.Fatalf("first read footer: %q", f)
	}
	s.NextTurn()
	if f := s.Observe(read); !strings.Contains(f, "already read at turn 1") {
		t.Fatalf("a re-read of the whole file was not recognised as redundant: %q", f)
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

// TestAProseOnlyDocumentIsNotOverwritten: a document the user has emptied of
// bullets, keeping their own notes, contributes no task — but the name is
// still theirs, and the next task with that slug must not land on it.
func TestAProseOnlyDocumentIsNotOverwritten(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	tasks := filepath.Join(root, ".be-code", "tasks")
	os.MkdirAll(tasks, 0o755)
	prose := "# 001 — a task\n\nI finished this one by hand. Keeping the notes:\nthe parser was fine, the scanner was not.\n"
	os.WriteFile(filepath.Join(tasks, "001-a-task.md"), []byte(prose), 0o644)

	s, err := OpenAt(dir, root, "s1", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Tree().Roots) != 0 {
		t.Fatalf("prose is not a task: %+v", s.Tree().Roots)
	}
	s.Plan("a task", nil) // the same slug
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(tasks, "001-a-task.md"))
	if err != nil || string(b) != prose {
		t.Fatalf("the user's prose-only document was overwritten:\n%s", b)
	}
}

// TestDocumentNumbersAreNotReused: two files sharing an NNN read as one task
// split in half, so a number is claimed as firmly as a name.
func TestDocumentNumbersAreNotReused(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	tasks := filepath.Join(root, ".be-code", "tasks")
	os.MkdirAll(tasks, 0o755)
	os.WriteFile(filepath.Join(tasks, "001-alpha.md"), []byte("# 001 — alpha\n\n- [ ] 1. alpha\n"), 0o644)
	os.WriteFile(filepath.Join(tasks, "003-beta-two.md"), []byte("# 003 — beta two\n\n- [ ] 2. beta two\n"), 0o644)

	s, err := OpenAt(dir, root, "s1", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	s.Plan("gamma", nil)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(tasks)
	byNum := map[string]string{}
	for _, e := range ents {
		if e.Name() == "README.md" {
			continue
		}
		num := e.Name()[:3]
		if prev, dup := byNum[num]; dup {
			t.Fatalf("number %s used by both %s and %s", num, prev, e.Name())
		}
		byNum[num] = e.Name()
	}
	if len(byNum) != 3 {
		t.Fatalf("documents: %v", byNum)
	}
}

// TestAnEditedFileIsNotReportedAsAlreadyReadAcrossAReopen: the reopen must
// not hand the newest hash to an older node's reference. Those ranges were
// measured against content that has since been edited away, and dressing
// them in the current hash is the false "already read" ruling T2-b exists to
// prevent — the same failure as the edit_file fix, one reopen later.
func TestAnEditedFileIsNotReportedAsAlreadyReadAcrossAReopen(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	old := "package a\nvar X = 1\n"
	writeFile(t, root, "a.go", old)

	s, err := OpenAt(dir, root, "s1", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	id := s.Plan("change a.go", []string{"read it", "edit it"})
	if err := s.SetStatus(id+".1", StatusDoing, ""); err != nil {
		t.Fatal(err)
	}
	s.NextTurn()
	s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "a.go"}, Content: numbered(old, 1)})

	// A different step does the editing, so the read and the edit end up on
	// two different nodes' references to the same file.
	if err := s.SetStatus(id+".2", StatusDoing, ""); err != nil {
		t.Fatal(err)
	}
	s.NextTurn()
	writeFile(t, root, "a.go", "package a\nvar X = 2\n")
	s.Observe(Event{Tool: "edit_file", Args: map[string]any{"path": "a.go"}, Content: "edited a.go"})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	again, err := OpenAt(dir, root, "s2", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	again.NextTurn()
	f := again.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "a.go"},
		Content: numbered("package a\nvar X = 2\n", 1)})
	if f != "" {
		t.Fatalf("the reopen revived ranges describing text that is gone: %q", f)
	}
}

// TestAHandDeletedStepDoesNotInheritAnothersHash: node ids are positional,
// so deleting a step in an editor shifts every later sibling down. A file
// memo pinned by id alone then lands on a different node's reference, and
// dresses ranges measured against content that has since changed in the
// current hash — "already read" for lines nobody saw. The ranges have to
// match too.
func TestAHandDeletedStepDoesNotInheritAnothersHash(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	oldSrc := "package a\nvar X = 1\n"
	newSrc := "package a\nvar X = 2\n"

	s, err := OpenAt(dir, root, "s1", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	id := s.Plan("work", []string{"first", "current", "stale"})

	// The third step read one line of the file as it was then.
	writeFile(t, root, "a.go", oldSrc)
	if err := s.SetStatus(id+".3", StatusDoing, ""); err != nil {
		t.Fatal(err)
	}
	s.NextTurn()
	s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "a.go"},
		Content: numbered("package a\n", 1)})

	// The second step read the whole file after it changed, so it is the
	// newest reference and owns the memo.
	writeFile(t, root, "a.go", newSrc)
	if err := s.SetStatus(id+".2", StatusDoing, ""); err != nil {
		t.Fatal(err)
	}
	s.NextTurn()
	s.NextTurn()
	s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "a.go"},
		Content: numbered(newSrc, 1)})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	// The user deletes the first step in their editor; "stale" shifts into
	// the id the memo recorded for "current".
	path := filepath.Join(root, ".be-code", "tasks", "001-work.md")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.Contains(line, "1.1. first") {
			kept = append(kept, line)
		}
	}
	os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o644)

	again, err := OpenAt(dir, root, "s2", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	again.NextTurn()
	f := again.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "a.go"},
		Content: numbered("package a\n", 1)})
	if f != "" {
		t.Fatalf("a shifted step inherited another's hash: %q", f)
	}
}

// filesIn is every file reference in the tree, in walk order. It replaces
// the flat Digests() projection compat.go used to offer.
func filesIn(s *Store) []FileRef {
	var out []FileRef
	tr := s.Tree()
	tr.Walk(func(n *Node, _ int) { out = append(out, n.Evidence.Files...) })
	return out
}

// TestUnfiledEvidenceIsAdoptedByTheNextDoingNode: spec 4.2's other half.
// Work done before the model said what it was doing belongs to the step it
// then named, verbatim — not to a permanent "unfiled" bucket.
func TestUnfiledEvidenceIsAdoptedByTheNextDoingNode(t *testing.T) {
	s, err := OpenAt(t.TempDir(), t.TempDir(), "s1", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	id := s.Plan("fix the parser", []string{"find it", "fix it"})
	s.NextTurn()
	// Nothing is doing yet, so this lands on the unfiled node.
	s.Observe(Event{Tool: "shell", Args: map[string]any{"command": "go test ./..."},
		Content: "FAIL: TestLex\n", IsError: true})
	if !strings.Contains(s.TreeText(), unfiledText) {
		t.Fatalf("no unfiled node to adopt:\n%s", s.TreeText())
	}
	if err := s.SetStatusText(id+".1", "doing", ""); err != nil {
		t.Fatal(err)
	}
	n := s.Tree().Find(id + ".1")
	if len(n.Evidence.Raw) != 1 || n.Evidence.Raw[0].Args != "go test ./..." {
		t.Fatalf("evidence was not adopted: %+v", n.Evidence)
	}
	if strings.Contains(s.TreeText(), unfiledText) {
		t.Fatalf("the unfiled node survived adoption:\n%s", s.TreeText())
	}
	// And it distills with its new owner, into that step's report.
	if err := s.SetStatusText(id+".1", "done", ""); err != nil {
		t.Fatal(err)
	}
	got := s.Tree().Find(id + ".1")
	if len(got.Evidence.Cmds) != 1 || got.Evidence.Cmds[0].Cmd != "go test ./..." {
		t.Fatalf("distilled cmds: %+v", got.Evidence.Cmds)
	}
	if len(got.Evidence.Errors) != 1 {
		t.Fatalf("the failure came with it: %+v", got.Evidence.Errors)
	}
}

// TestAdoptionNeverHandsOutALiveSiblingsID: the unfiled node is removed
// from the middle of its parent's children, so the next add must count
// past the highest index, not past the child count.
func TestAdoptionNeverHandsOutALiveSiblingsID(t *testing.T) {
	s, err := OpenAt(t.TempDir(), t.TempDir(), "s1", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	id := s.Plan("a task", []string{"one"})
	s.NextTurn()
	s.Observe(Event{Tool: "shell", Args: map[string]any{"command": "ls"}, Content: "a\n"}) // unfiled -> 1.2
	later, err := s.Add(id, "two")                                                         // 1.3
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatusText(later, "doing", ""); err != nil { // removes 1.2
		t.Fatal(err)
	}
	next, err := s.Add(id, "three")
	if err != nil {
		t.Fatal(err)
	}
	if next == later {
		t.Fatalf("a new node took a live sibling's id: %q", next)
	}
	if s.Tree().Find(later) == nil || s.Tree().Find(next) == nil {
		t.Fatalf("both nodes must exist: %q %q\n%s", later, next, s.TreeText())
	}
	// The same rule has to hold for roots, or a later Plan would return an
	// id that already names a live task and Find would resolve the wrong
	// one. The store no longer removes a root, but Tree.Remove is general.
	var tr Tree
	tr.Add("", "one")
	gone := tr.Add("", "two")
	three := tr.Add("", "three")
	tr.Remove(gone)
	fresh := tr.Add("", "four")
	if fresh.ID == three.ID {
		t.Fatalf("a new root took live root %q's id", three.ID)
	}
	if got := tr.Find(three.ID); got != three {
		t.Fatalf("Find(%q) resolved to %q, not the live root", three.ID, got.Text)
	}
}

// TestAdoptionNeverRemovesARoot: s.files and s.extra are index-parallel to
// Roots, so taking a root out of the middle shifts every later task onto
// the previous one's document. A root-level unfiled node is therefore
// emptied and dropped, never removed.
func TestAdoptionNeverRemovesARoot(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	s, err := OpenAt(dir, root, "s1", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	first := s.Plan("first thing", []string{"a"})
	s.SetStatusText(first+".1", "done", "")
	s.SetStatusText(first, "done", "")
	s.NextTurn()
	// Every root is terminal, so this opens a root-level unfiled node.
	s.Observe(Event{Tool: "shell", Args: map[string]any{"command": "ls"}, Content: "a\n"})
	// A flush while that root exists is what pins s.files/s.extra to the
	// root positions a removal would then shift.
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	second := s.Plan("second thing", []string{"b"})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	// Adoption here used to take root 2 out, shifting "second thing" onto
	// the unfiled node's document and orphaning its own.
	if err := s.SetStatusText(second+".1", "doing", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	again, err := OpenAt(dir, root, "s2", true, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, r := range again.Tree().Roots {
		texts = append(texts, r.Text)
	}
	seen := map[string]bool{}
	for _, tx := range texts {
		if seen[tx] {
			t.Fatalf("a task was duplicated across the reload: %v", texts)
		}
		seen[tx] = true
	}
	if !seen["first thing"] || !seen["second thing"] {
		t.Fatalf("roots after the reload: %v", texts)
	}
}

// TestAnAdoptedRootLeavesNoDocumentAndNoReport: a root-level unfiled node
// is kept in the tree (removing one shifts every later task's document),
// but it is an artefact of the harness, not a task the user asked for. It
// must not become a file in their repository, and it must not become a
// Task Report in the model's prompt — and by ruling T3-a nothing would
// ever delete either.
func TestAnAdoptedRootLeavesNoDocumentAndNoReport(t *testing.T) {
	root := t.TempDir()
	s, err := OpenAt(t.TempDir(), root, "s1", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	first := s.Plan("first thing", []string{"a"})
	s.SetStatusText(first+".1", "done", "")
	s.SetStatusText(first, "done", "")
	s.NextTurn()
	// Every root is terminal, so this opens a root-level unfiled node.
	s.Observe(Event{Tool: "shell", Args: map[string]any{"command": "ls"}, Content: "a\n"})
	second := s.Plan("second thing", []string{"b"})
	if err := s.SetStatusText(second+".1", "doing", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	ents, err := os.ReadDir(filepath.Join(root, workspaceDir, "tasks"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
		if strings.Contains(e.Name(), unfiledText) {
			t.Fatalf("adoption left a document the user never created: %v", names)
		}
	}
	if len(names) < 3 { // README.md plus the two real tasks
		t.Fatalf("the real tasks lost their documents: %v", names)
	}
	if block := s.Render(4096, nil); strings.Contains(block, unfiledText) {
		t.Fatalf("a phantom task reached the prompt:\n%s", block)
	}
	// The evidence itself is still on the node that adopted it. Plan
	// closed the unfiled node on its way past, so it arrives distilled
	// rather than raw; either way it must not have gone with the document.
	if n := s.Tree().Find(second + ".1"); n == nil || len(n.Evidence.Cmds) != 1 {
		t.Fatalf("adoption lost the evidence: %s", s.TreeText())
	}
}
