package engine

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMigrationLiftsTheFlatLedger: a 0.10.0 store becomes one task with its
func TestMigrationLiftsTheFlatLedger(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	old := `{"task":"fix the parser","steps":[{"text":"find the bug","status":"done"},
	  {"text":"fix and verify","status":"doing"}],"decisions":["quotes handled in the scanner"],
	  "facts":["lexer.go is 900 lines"],"session":"s0"}`
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "ledger.json"), []byte(old), 0o600)

	s, err := OpenAt(dir, root, "s0", true, Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768})
	if err != nil {
		t.Fatal(err)
	}
	tr := s.Tree()
	if len(tr.Roots) != 1 || tr.Roots[0].Text != "fix the parser" {
		t.Fatalf("roots: %+v", tr.Roots)
	}
	if len(tr.Roots[0].Children) != 2 || tr.Roots[0].Children[0].Status != StatusDone {
		t.Fatalf("children: %+v", tr.Roots[0].Children)
	}
	notes := tr.Roots[0].Evidence.Notes
	if len(notes) != 2 || !notes[0].Decision {
		t.Fatalf("notes: %+v", notes)
	}
	if _, err := os.Stat(filepath.Join(dir, "ledger.json")); !os.IsNotExist(err) {
		t.Fatal("the migrated ledger should be removed so migration runs once")
	}
}

// TestMigrationSurvivesAnInterruptedFirstRun: the lifted tree must be on
// disk before the 0.10.0 files are removed. A normal session does not flush
// until a request completes, so a Ctrl-C at the prompt used to take the old
// store with it.
func TestMigrationSurvivesAnInterruptedFirstRun(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ledger.json"),
		[]byte(`{"task":"lift me","steps":[{"text":"one","status":"done"}],"session":"s0"}`), 0o600)

	if _, err := OpenAt(dir, root, "s0", true, testLimits()); err != nil {
		t.Fatal(err)
	}
	// Interrupted here: nothing else ever called Flush.
	again, err := OpenAt(dir, root, "s1", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	tr := again.Tree()
	if len(tr.Roots) != 1 || tr.Roots[0].Text != "lift me" {
		t.Fatalf("the migrated task did not survive the interruption: %+v", tr.Roots)
	}
	if len(tr.Roots[0].Children) != 1 || tr.Roots[0].Children[0].Status != StatusDone {
		t.Fatalf("children: %+v", tr.Roots[0].Children)
	}
}

// TestMigrationCarriesDigestsAndLookups: digests and lookups become the
// task's evidence, all three legacy files go, and the hash and outline a
// document cannot carry come back from state.json (ruling T3-b).
func TestMigrationCarriesDigestsAndLookups(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	writeFile(t, root, "lexer.go", "package lex\nfunc Scan() {}\n")
	os.WriteFile(filepath.Join(dir, "ledger.json"), []byte(`{"task":"fix","session":"s0"}`), 0o600)
	os.WriteFile(filepath.Join(dir, "digests.json"), []byte(
		`[{"path":"lexer.go","hash":"abc","outline":["Scan"],"ranges":[{"from":1,"to":2}],"turn":7,"edited":true,"note":"the lexer"}]`), 0o600)
	os.WriteFile(filepath.Join(dir, "lookups.json"), []byte(
		`[{"tool":"search","query":"search pattern=\"Scan\";","hits":[{"file":"lexer.go","line":2,"text":"func Scan"}],"turn":5}]`), 0o600)

	s, err := OpenAt(dir, root, "s0", true, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	r := s.Tree().Roots[0]
	if len(r.Evidence.Files) != 1 || r.Evidence.Files[0].Path != "lexer.go" || !r.Evidence.Files[0].Edited {
		t.Fatalf("digests were not lifted: %+v", r.Evidence.Files)
	}
	if r.Evidence.Files[0].Note != "the lexer" {
		t.Fatalf("file note lost: %+v", r.Evidence.Files[0])
	}
	if len(r.Evidence.Lookups) != 1 || len(r.Evidence.Lookups[0].Hits) != 1 {
		t.Fatalf("lookups were not lifted: %+v", r.Evidence.Lookups)
	}
	if s.Turn() != 7 {
		t.Fatalf("turn = %d, want the highest of the lifted turns", s.Turn())
	}
	for _, name := range []string{"ledger.json", "digests.json", "lookups.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s survived the migration", name)
		}
	}

	again, err := OpenAt(dir, root, "s1", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	f := again.Tree().Roots[0].Evidence.Files[0]
	if f.Hash != "abc" || len(f.Outline) != 1 || f.Turn != 7 {
		t.Fatalf("the file's hash, outline and turn did not survive the reopen: %+v", f)
	}
}

// TestMigrationIsIdempotentWithoutAnyFlag: ruling T3-d. The legacy files are
// renamed aside before anything is written, so "has this been migrated?" is
// a fact about the filesystem. Losing state.json entirely — the window in
// which a flag written after the documents had not reached disk — must not
// migrate the same ledger into a second task.
func TestMigrationIsIdempotentWithoutAnyFlag(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	ledger := `{"task":"lift me","steps":[{"text":"one","status":"done"}],"session":"s0"}`
	os.WriteFile(filepath.Join(dir, "ledger.json"), []byte(ledger), 0o600)
	if _, err := OpenAt(dir, root, "s0", true, testLimits()); err != nil {
		t.Fatal(err)
	}

	// Renamed, not removed, and readable exactly as it was.
	if _, err := os.Stat(filepath.Join(dir, "ledger.json")); !os.IsNotExist(err) {
		t.Fatal("ledger.json should have been renamed aside")
	}
	aside, _ := filepath.Glob(filepath.Join(dir, "ledger.json.migrated-*"))
	if len(aside) != 1 {
		t.Fatalf("aside files: %v", aside)
	}
	if b, _ := os.ReadFile(aside[0]); string(b) != ledger {
		t.Fatalf("the 0.10.0 ledger was not preserved intact: %s", b)
	}

	// The kill window no flag could cover: the documents are on disk and
	// state.json never was.
	os.Remove(filepath.Join(dir, "state.json"))
	again, err := OpenAt(dir, root, "s1", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Tree().Roots) != 1 {
		t.Fatalf("migrated a second time with no state.json: %+v", again.Tree().Roots)
	}
	docs, _ := filepath.Glob(filepath.Join(root, ".be-code", "tasks", "0*.md"))
	if len(docs) != 1 {
		t.Fatalf("documents: %v", docs)
	}
}

// TestARestoredLedgerIsLiftedNotLost: ruling T3-d. A ledger.json that turns
// up after a migration — a downgrade, a restored backup — has simply not
// been migrated yet. A permanent "migrated" flag deleted it unread, losing
// whatever work it carried.
func TestARestoredLedgerIsLiftedNotLost(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ledger.json"),
		[]byte(`{"task":"the first task","session":"s0"}`), 0o600)
	if _, err := OpenAt(dir, root, "s0", true, testLimits()); err != nil {
		t.Fatal(err)
	}

	// A backup is restored, carrying work this store has never seen.
	os.WriteFile(filepath.Join(dir, "ledger.json"),
		[]byte(`{"task":"work from the backup","steps":[{"text":"one","status":"doing"}],"session":"s0"}`), 0o600)
	again, err := OpenAt(dir, root, "s1", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, r := range again.Tree().Roots {
		texts = append(texts, r.Text)
	}
	found := false
	for _, x := range texts {
		if x == "work from the backup" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the restored ledger was discarded without being lifted: %v", texts)
	}
	// And it too was set aside rather than deleted.
	if aside, _ := filepath.Glob(filepath.Join(dir, "ledger.json.migrated-*")); len(aside) != 2 {
		t.Fatalf("aside files: %v", aside)
	}
}

// TestAFailedFlushAfterTheDocumentsKeepsTheAside: ruling T3-f. Once the task
// documents are on disk the lift has happened; only the dotdir state failed
// to persist. Putting the legacy files back at that point leaves ledger.json
// beside the document it was lifted into, so the next open loads the document
// *and* migrates the ledger again — two roots, two documents, one task. The
// migration therefore keeps the aside and reports the failure instead.
func TestAFailedFlushAfterTheDocumentsKeepsTheAside(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ledger.json"),
		[]byte(`{"task":"lift me","steps":[{"text":"one","status":"done"}],"session":"s0"}`), 0o600)
	// A directory where state.json goes: the documents are written first, so
	// the flush fails after the lift is on disk and before the state is.
	if err := os.Mkdir(filepath.Join(dir, "state.json"), 0o755); err != nil {
		t.Fatal(err)
	}

	s, err := OpenAt(dir, root, "s0", true, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Tree().Roots) != 1 {
		t.Fatalf("the lift was undone although its document was written: %+v", s.Tree().Roots)
	}
	docs, _ := filepath.Glob(filepath.Join(root, ".be-code", "tasks", "0*.md"))
	if len(docs) != 1 {
		t.Fatalf("documents after the failed flush: %v", docs)
	}
	if _, err := os.Stat(filepath.Join(dir, "ledger.json")); !os.IsNotExist(err) {
		t.Fatal("the legacy ledger was put back beside the document it was lifted into")
	}
	if aside, _ := filepath.Glob(filepath.Join(dir, "ledger.json.migrated-*")); len(aside) != 1 {
		t.Fatalf("aside files: %v", aside)
	}
	if orphans, _ := filepath.Glob(filepath.Join(dir, "state.json.tmp.*")); len(orphans) != 0 {
		t.Fatalf("the failed write left a temp file behind: %v", orphans)
	}

	// The disk is writable again: exactly one task and one document, not two.
	os.RemoveAll(filepath.Join(dir, "state.json"))
	again, err := OpenAt(dir, root, "s1", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Tree().Roots) != 1 {
		t.Fatalf("the ledger was migrated a second time: %+v", again.Tree().Roots)
	}
	docs, _ = filepath.Glob(filepath.Join(root, ".be-code", "tasks", "0*.md"))
	if len(docs) != 1 {
		t.Fatalf("documents after the reopen: %v", docs)
	}
}

// TestAFailedFlushBeforeTheDocumentsPutsTheLedgerBack: the other half of
// ruling T3-f. Nothing was written, so nothing has been lifted: the legacy
// files go back where they were and the next open tries the whole migration
// again.
func TestAFailedFlushBeforeTheDocumentsPutsTheLedgerBack(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	ledger := `{"task":"lift me","steps":[{"text":"one","status":"done"}],"session":"s0"}`
	os.WriteFile(filepath.Join(dir, "ledger.json"), []byte(ledger), 0o600)
	// A file where the workspace folder goes: no document can be written.
	if err := os.WriteFile(filepath.Join(root, ".be-code"), []byte("not a folder\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := OpenAt(dir, root, "s0", true, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Tree().Roots) != 0 {
		t.Fatalf("a lift that reached no disk was kept: %+v", s.Tree().Roots)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "ledger.json")); err != nil || string(b) != ledger {
		t.Fatalf("the 0.10.0 ledger was not put back: %v %q", err, b)
	}
	if aside, _ := filepath.Glob(filepath.Join(dir, "*.migrated-*")); len(aside) != 0 {
		t.Fatalf("aside files left behind: %v", aside)
	}

	// The workspace is writable again and the migration simply runs.
	os.Remove(filepath.Join(root, ".be-code"))
	again, err := OpenAt(dir, root, "s1", false, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Tree().Roots) != 1 || again.Tree().Roots[0].Text != "lift me" {
		t.Fatalf("the retried migration did not lift the ledger: %+v", again.Tree().Roots)
	}
}
