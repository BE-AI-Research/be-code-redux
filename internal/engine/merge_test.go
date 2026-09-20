package engine

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func readDoc(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, workspaceDir, "tasks", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeDocFile(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, workspaceDir, "tasks", name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestAHandEditMidSessionSurvivesTheNextFlush is C2. The README the engine
// writes tells the user to edit these files freely; Flush used to render its
// own tree straight over them, so a retitle, a moved status mark or a
// paragraph of their own lasted until the end of the request in flight.
func TestAHandEditMidSessionSurvivesTheNextFlush(t *testing.T) {
	s, root := openTest(t, "s1", false)
	id := s.Plan("fix the parser", []string{"find the bug", "rewrite the scanner"})
	s.SetStatus(id+".1", StatusDoing, "")
	s.Note(id+".1", "the bug is in quote handling", "", false, false)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	// The user, in their editor, while the session runs: a retitle, a step
	// they decide against, a note they delete, and prose of their own.
	doc := readDoc(t, root, "001-fix-the-parser.md")
	doc = strings.Replace(doc, "1. fix the parser", "1. fix the lexer", 1)
	doc = strings.Replace(doc, "- [ ] 1.2. rewrite the scanner", "- [-] 1.2. rewrite the scanner — dropped: not worth it", 1)
	doc = strings.Replace(doc, "    - note: the bug is in quote handling\n", "", 1)
	doc += "\nMy own notes: ask Priya about the grammar before touching this.\n"
	writeDocFile(t, root, "001-fix-the-parser.md", doc)

	// The engine, meanwhile, carries on recording.
	s.Observe(Event{Tool: "shell", Args: map[string]any{"command": "go test ./lexer"}, Content: "FAIL: TestQuote\n", IsError: true})
	s.Note(id+".1", "use a state machine", "", true, false)
	s.SetStatus(id+".1", StatusDone, "")
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	got := readDoc(t, root, "001-fix-the-parser.md")
	for _, want := range []string{
		"1. fix the lexer", // their retitle
		"rewrite the scanner — dropped: not worth it", // their status and reason
		"My own notes: ask Priya",                     // their prose
		"decision: use a state machine",               // the engine's new evidence
		"cmds: go test ./lexer — failed",
		"- [x] 1.1. find the bug", // the engine's own status change
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the flush lost %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "the bug is in quote handling") {
		t.Fatalf("a note the user deleted came back:\n%s", got)
	}
	if strings.Contains(got, "1. fix the parser") {
		t.Fatalf("the old title came back:\n%s", got)
	}
	tr := s.Tree()
	if tr.Roots[0].Text != "fix the lexer" || tr.Find(id+".2").Status != StatusDropped {
		t.Fatalf("the tree in memory did not take the edit: %s", s.TreeText())
	}
	if w := s.TakeWarnings(); len(w) != 0 {
		t.Fatalf("a clean merge raised a warning: %v", w)
	}
	if aside, _ := filepath.Glob(filepath.Join(root, workspaceDir, "tasks", "*"+editedSuffix+"*")); len(aside) != 0 {
		t.Fatalf("a clean merge set a copy aside: %v", aside)
	}
}

// TestReloadTakesAnEditInBeforeTheNextRequest: the agent reloads at the top
// of run(), so the request works from the user's edit, not over it — and a
// reload by itself writes nothing.
func TestReloadTakesAnEditInBeforeTheNextRequest(t *testing.T) {
	s, root := openTest(t, "s1", false)
	id := s.Plan("fix the parser", []string{"find the bug"})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	doc := readDoc(t, root, "001-fix-the-parser.md")
	doc = strings.Replace(doc, "- [ ] 1.1. find the bug", "- [ ] 1.1. find the bug\n  - [>] 1.2. bisect it first", 1)
	writeDocFile(t, root, "001-fix-the-parser.md", doc)

	s.Reload()
	if got := readDoc(t, root, "001-fix-the-parser.md"); got != doc {
		t.Fatalf("a reload rewrote the document:\n%s", got)
	}
	n := s.Tree().Find(id + ".2")
	if n == nil || n.Text != "bisect it first" || n.Status != StatusDoing {
		t.Fatalf("the step added by hand was not taken in: %s", s.TreeText())
	}
	// Evidence now goes where the user pointed.
	s.Observe(Event{Tool: "shell", Args: map[string]any{"command": "git bisect start"}, Content: "ok\n"})
	if n := s.Tree().Find(id + ".2"); len(n.Evidence.Raw) != 1 {
		t.Fatalf("evidence did not follow the user's doing mark: %s", s.TreeText())
	}
}

// TestTwoStoresOnOneWorkspaceLoseNeithersNote is C2's second half: two live
// sessions on one project write the same documents, and whichever flushed
// second used to erase what the first had recorded since they both loaded.
func TestTwoStoresOnOneWorkspaceLoseNeithersNote(t *testing.T) {
	root := t.TempDir()
	seed, err := OpenAt(t.TempDir(), root, "s0", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	id := seed.Plan("fix the parser", []string{"find the bug"})
	if err := seed.Flush(); err != nil {
		t.Fatal(err)
	}

	a, err := OpenAt(t.TempDir(), root, "sa", true, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	b, err := OpenAt(t.TempDir(), root, "sb", true, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	b.Note(id, "B decided: use a trie", "", true, false)
	if err := b.Flush(); err != nil {
		t.Fatal(err)
	}
	a.Note(id, "A decided: keep the public API", "", true, false)
	if err := a.Flush(); err != nil {
		t.Fatal(err)
	}
	b.Note(id, "B learned: the fuzzer finds it in seconds", "", false, false)
	if err := b.Flush(); err != nil {
		t.Fatal(err)
	}

	got := readDoc(t, root, "001-fix-the-parser.md")
	for _, want := range []string{"B decided: use a trie", "A decided: keep the public API", "B learned: the fuzzer finds it in seconds"} {
		if n := strings.Count(got, want); n != 1 {
			t.Fatalf("%q appears %d times, want once:\n%s", want, n, got)
		}
	}
	// And neither session's own picture is missing the other's note.
	a.Reload()
	if !strings.Contains(a.TreeText(), "the fuzzer finds it") || !strings.Contains(b.TreeText(), "keep the public API") {
		t.Fatalf("a session cannot see the other's note:\nA:\n%s\nB:\n%s", a.TreeText(), b.TreeText())
	}
}

// TestAnEditThatCannotBeMergedIsSetAside: when the document on disk cannot
// be parsed there is nothing to merge with, and the record still has to be
// written. The user's version is copied aside first, and they are told.
func TestAnEditThatCannotBeMergedIsSetAside(t *testing.T) {
	s, root := openTest(t, "s1", false)
	id := s.Plan("fix the parser", []string{"find the bug"})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	broken := "# 001 — fix the parser\n\n- [?] 1. half an edit, saved mid-thought\n"
	writeDocFile(t, root, "001-fix-the-parser.md", broken)
	s.Note(id, "the engine's own note", "", false, false)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}

	aside, _ := filepath.Glob(filepath.Join(root, workspaceDir, "tasks", "001-fix-the-parser"+editedSuffix+"*.md"))
	if len(aside) != 1 {
		t.Fatalf("the user's version was not set aside: %v", aside)
	}
	if b, _ := os.ReadFile(aside[0]); string(b) != broken {
		t.Fatalf("the copy set aside is not what the user wrote: %q", b)
	}
	if got := readDoc(t, root, "001-fix-the-parser.md"); !strings.Contains(got, "the engine's own note") {
		t.Fatalf("the record was not written:\n%s", got)
	}
	w := strings.Join(s.TakeWarnings(), "\n")
	if !strings.Contains(w, "001-fix-the-parser.md") || !strings.Contains(w, filepath.Base(aside[0])) {
		t.Fatalf("nobody was told: %q", w)
	}
	// The copy is the user's to read; it is never loaded as a task.
	again, err := OpenAt(t.TempDir(), root, "s2", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(again.Tree().Roots); n != 1 {
		t.Fatalf("the set-aside copy was loaded as a task (%d roots)", n)
	}
	if left, _ := filepath.Glob(filepath.Join(root, workspaceDir, "tasks", "*.broken-*")); len(left) != 0 {
		t.Fatalf("the set-aside copy was quarantined as a broken document: %v", left)
	}
}

// TestAnUnchangedDocumentIsNotRewritten: a request that touches one task
// must not rewrite every document in the project. Every needless write is a
// window for exactly the overwrite this file exists to prevent.
func TestAnUnchangedDocumentIsNotRewritten(t *testing.T) {
	s, root := openTest(t, "s1", false)
	first := s.Plan("first task", []string{"one"})
	s.SetStatus(first, StatusDone, "")
	second := s.Plan("second task", []string{"two"})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, workspaceDir, "tasks", "001-first-task.md")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	s.Note(second, "only the second task changed", "", false, false)
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(old) {
		t.Fatalf("an unchanged document was rewritten (mtime %v, was %v)", info.ModTime(), old)
	}
	if !strings.Contains(readDoc(t, root, "002-second-task.md"), "only the second task changed") {
		t.Fatal("the changed document was not written")
	}
}

// TestASplitRootIsWrittenBeforeItsSourceIsRewritten is ruling T3-g,
// corrected. A task added by hand to another task's document is moved to a
// file of its own; the source used to be rewritten without it *first*, so a
// failure on the second write erased the hand-added task from the only file
// that held it.
func TestASplitRootIsWrittenBeforeItsSourceIsRewritten(t *testing.T) {
	root, dir := t.TempDir(), t.TempDir()
	tasks := filepath.Join(root, workspaceDir, "tasks")
	os.MkdirAll(tasks, 0o755)
	source := "# 001 — alpha\n\n- [ ] 1. alpha\n- [ ] 2. gamma\n  - note: written by hand, and nowhere else\n"
	os.WriteFile(filepath.Join(tasks, "001-alpha.md"), []byte(source), 0o644)
	// The file gamma would be split into cannot be written.
	if err := os.Mkdir(filepath.Join(tasks, "002-gamma.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	s, err := OpenAt(dir, root, "s1", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err == nil {
		t.Fatal("the flush should have failed on the obstructed document")
	}
	if got := readDoc(t, root, "001-alpha.md"); !strings.Contains(got, "gamma") || !strings.Contains(got, "written by hand, and nowhere else") {
		t.Fatalf("the hand-added task was erased from the only file that held it:\n%s", got)
	}

	// Unobstructed, the retry completes the split, and a reopen sees each
	// task exactly once.
	os.Remove(filepath.Join(tasks, "002-gamma.md"))
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := readDoc(t, root, "001-alpha.md"); strings.Contains(got, "gamma") {
		t.Fatalf("the source document still holds the split task:\n%s", got)
	}
	again, err := OpenAt(dir, root, "s2", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, r := range again.Tree().Roots {
		texts = append(texts, r.Text)
	}
	if strings.Join(texts, ",") != "alpha,gamma" {
		t.Fatalf("after the split: %v", texts)
	}
}

// TestADeathBetweenTheSplitsTwoWritesDoesNotDuplicateTheTask: writing the
// split root first trades a lost task for a task that is, for an instant, in
// two files. A process that dies in that instant must not come back with it
// twice.
func TestADeathBetweenTheSplitsTwoWritesDoesNotDuplicateTheTask(t *testing.T) {
	root, dir := t.TempDir(), t.TempDir()
	tasks := filepath.Join(root, workspaceDir, "tasks")
	os.MkdirAll(tasks, 0o755)
	os.WriteFile(filepath.Join(tasks, "001-alpha.md"),
		[]byte("# 001 — alpha\n\n- [ ] 1. alpha\n- [ ] 2. gamma\n  - note: by hand\n"), 0o644)
	os.WriteFile(filepath.Join(tasks, "002-gamma.md"),
		[]byte("# 002 — gamma\n\n- [ ] 2. gamma\n  - note: by hand\n"), 0o644)
	// A third task that merely shares a title is somebody's real task.
	os.WriteFile(filepath.Join(tasks, "003-gamma.md"),
		[]byte("# 003 — gamma\n\n- [ ] 3. gamma\n  - note: a different gamma\n"), 0o644)

	s, err := OpenAt(dir, root, "s1", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, r := range s.Tree().Roots {
		texts = append(texts, r.Text)
	}
	if strings.Join(texts, ",") != "alpha,gamma,gamma" {
		t.Fatalf("roots after the interrupted split: %v", texts)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := readDoc(t, root, "001-alpha.md"); strings.Contains(got, "gamma") {
		t.Fatalf("the source document was not finished:\n%s", got)
	}
	if got := readDoc(t, root, "003-gamma.md"); !strings.Contains(got, "a different gamma") {
		t.Fatalf("the unrelated task of the same title was touched:\n%s", got)
	}
}

// TestASymlinkedTasksFolderIsRefused is I7. Every path the engine writes,
// rewrites or renames is built under .be-code/tasks, none of it goes through
// the approval seam, and a link at either name sends all of it outside the
// workspace the session was confined to.
func TestASymlinkedTasksFolderIsRefused(t *testing.T) {
	for _, which := range []string{workspaceDir, filepath.Join(workspaceDir, "tasks")} {
		t.Run(which, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			// Something the engine would quarantine — a rename — if it read it.
			os.WriteFile(filepath.Join(outside, "001-bait.md"), []byte("# 001 — bait\n\n- [?] 1. bait\n"), 0o644)
			os.MkdirAll(filepath.Join(outside, "tasks"), 0o755)
			os.WriteFile(filepath.Join(outside, "tasks", "001-bait.md"), []byte("# 001 — bait\n\n- [?] 1. bait\n"), 0o644)
			if which != workspaceDir {
				os.MkdirAll(filepath.Join(root, workspaceDir), 0o755)
			}
			if err := os.Symlink(outside, filepath.Join(root, which)); err != nil {
				t.Skip("symlinks unavailable:", err)
			}
			before := listTree(t, outside)

			s, err := OpenAt(t.TempDir(), root, "s1", false, testLimits())
			if err != nil {
				t.Fatalf("a symlinked folder must not fail the open: %v", err)
			}
			id := s.Plan("a task", []string{"one"})
			s.SetStatus(id+".1", StatusDoing, "")
			if err := s.Flush(); err != nil {
				t.Fatalf("a refused workspace must not fail the flush: %v", err)
			}
			if after := listTree(t, outside); after != before {
				t.Fatalf("the engine wrote through the link:\nbefore:\n%s\nafter:\n%s", before, after)
			}
			if w := strings.Join(s.TakeWarnings(), "\n"); !strings.Contains(w, "symbolic link") {
				t.Fatalf("no warning: %q", w)
			}
			// Working memory still works for the session.
			if !strings.Contains(s.Render(4096, nil), "a task") {
				t.Fatal("the tree stopped rendering")
			}
		})
	}
}

// listTree is every path under dir with its size, as one string.
func listTree(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil {
			rel, _ := filepath.Rel(dir, p)
			b.WriteString(rel + " " + info.Mode().String() + " ")
			if !info.IsDir() {
				data, _ := os.ReadFile(p)
				b.WriteString(hashBytes(data))
			}
			b.WriteByte('\n')
		}
		return nil
	})
	return b.String()
}

// TestConcurrentFlushesLeaveTheNewestSnapshot is M2. Flush releases the lock
// for its writes, so two of them could finish in either order and leave the
// older snapshot on disk with nothing dirty left to correct it.
func TestConcurrentFlushesLeaveTheNewestSnapshot(t *testing.T) {
	s, root := openTest(t, "s1", false)
	id := s.Plan("a task", []string{"one"})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s.Note(id, "note "+strings.Repeat("x", i+1), "", false, false)
			if err := s.Flush(); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	got := readDoc(t, root, "001-a-task.md")
	for i := 0; i < 8; i++ {
		if want := "note: note " + strings.Repeat("x", i+1) + "\n"; !strings.Contains(got, want) {
			t.Fatalf("the document on disk is an older snapshot; missing %q:\n%s", want, got)
		}
	}
}

// TestSpentUnfiledNeedsAdoptionsOwnMark is ruling T5-d: a real task somebody
// titled "unfiled" is a task, with a document and a report like any other.
func TestSpentUnfiledNeedsAdoptionsOwnMark(t *testing.T) {
	s, root := openTest(t, "s1", false)
	id := s.Plan("unfiled", nil)
	s.SetStatus(id, StatusDropped, "changed my mind")
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, workspaceDir, "tasks", "001-unfiled.md")); err != nil {
		t.Fatalf("a task titled unfiled got no document: %v", err)
	}
	if !strings.Contains(s.Render(4096, nil), "changed my mind") {
		t.Fatalf("a task titled unfiled got no report:\n%s", s.Render(4096, nil))
	}
	if spentUnfiled(&Node{Text: unfiledText, Status: StatusDoing}) {
		t.Fatal("an unfiled root still doing counts as spent")
	}
	if !spentUnfiled(&Node{Text: unfiledText, Status: StatusDropped, Reason: adoptedBy + "2.1"}) {
		t.Fatal("the node adoption emptied no longer counts as spent")
	}
}
