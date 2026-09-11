package checkpoint

import (
	"os"
	"path/filepath"
	"testing"
)

func setup(t *testing.T) (string, *Checkpointer) {
	t.Helper()
	ws := t.TempDir()
	cp, err := New(ws, filepath.Join(t.TempDir(), "cps"))
	if err != nil {
		t.Fatal(err)
	}
	return ws, cp
}

func write(t *testing.T, ws, rel, content string) {
	t.Helper()
	p := filepath.Join(ws, rel)
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, ws, rel string) string {
	data, _ := os.ReadFile(filepath.Join(ws, rel))
	return string(data)
}

func TestUndoRestoresModifiedAndDeletesCreated(t *testing.T) {
	ws, cp := setup(t)
	write(t, ws, "existing.txt", "original")

	cp.BeginTurn("turn 1")
	// Simulate the tool flow: record, then modify.
	cp.Record(filepath.Join(ws, "existing.txt"))
	write(t, ws, "existing.txt", "changed")
	cp.Record(filepath.Join(ws, "sub/new.txt"))
	write(t, ws, "sub/new.txt", "brand new")

	if got := cp.ChangedLast(); len(got) != 2 {
		t.Fatalf("changed = %v", got)
	}
	restored, err := cp.Undo()
	if err != nil {
		t.Fatal(err)
	}
	if len(restored) != 2 {
		t.Fatalf("restored = %v", restored)
	}
	if read(t, ws, "existing.txt") != "original" {
		t.Fatal("modified file not restored")
	}
	if _, err := os.Stat(filepath.Join(ws, "sub/new.txt")); !os.IsNotExist(err) {
		t.Fatal("created file not removed on undo")
	}
	if cp.Depth() != 0 {
		t.Fatalf("depth = %d", cp.Depth())
	}
}

func TestUndoIsPerTurnLIFO(t *testing.T) {
	ws, cp := setup(t)
	write(t, ws, "f.txt", "v1")

	cp.BeginTurn("t1")
	cp.Record(filepath.Join(ws, "f.txt"))
	write(t, ws, "f.txt", "v2")

	cp.BeginTurn("t2")
	cp.Record(filepath.Join(ws, "f.txt"))
	write(t, ws, "f.txt", "v3")

	if cp.Depth() != 2 {
		t.Fatalf("depth = %d", cp.Depth())
	}
	if _, err := cp.Undo(); err != nil {
		t.Fatal(err)
	}
	if read(t, ws, "f.txt") != "v2" {
		t.Fatalf("after undo1: %q", read(t, ws, "f.txt"))
	}
	if _, err := cp.Undo(); err != nil {
		t.Fatal(err)
	}
	if read(t, ws, "f.txt") != "v1" {
		t.Fatalf("after undo2: %q", read(t, ws, "f.txt"))
	}
	if _, err := cp.Undo(); err == nil {
		t.Fatal("undo past empty should error")
	}
}

func TestEmptyTurnsAreDiscarded(t *testing.T) {
	ws, cp := setup(t)
	cp.BeginTurn("empty1")
	cp.BeginTurn("empty2")
	cp.BeginTurn("real")
	cp.Record(filepath.Join(ws, "a.txt"))
	write(t, ws, "a.txt", "x")
	if cp.Depth() != 1 {
		t.Fatalf("depth = %d", cp.Depth())
	}
}

func TestRecordOnlyFirstPerTurn(t *testing.T) {
	ws, cp := setup(t)
	write(t, ws, "f.txt", "first")
	cp.BeginTurn("t")
	cp.Record(filepath.Join(ws, "f.txt"))
	write(t, ws, "f.txt", "second")
	cp.Record(filepath.Join(ws, "f.txt")) // must NOT overwrite the backup
	write(t, ws, "f.txt", "third")
	cp.Undo()
	if read(t, ws, "f.txt") != "first" {
		t.Fatalf("got %q", read(t, ws, "f.txt"))
	}
}

func TestNilSafety(t *testing.T) {
	var cp *Checkpointer
	cp.BeginTurn("x")
	if err := cp.Record("/anything"); err != nil {
		t.Fatal("nil checkpointer must be a no-op")
	}
}

// A file whose name merely starts with ".." is inside the workspace and must
// be recordable; only real parent traversal is rejected.
func TestRecordAcceptsDotDotPrefixedName(t *testing.T) {
	root := t.TempDir()
	cp, err := New(root, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cp.BeginTurn("t")
	p := filepath.Join(root, "..foo.txt")
	os.WriteFile(p, []byte("x"), 0o644)
	if err := cp.Record(p); err != nil {
		t.Fatalf("..foo.txt rejected: %v", err)
	}
	if got := cp.ChangedLast(); len(got) != 1 || got[0] != "..foo.txt" {
		t.Fatalf("..foo.txt not tracked: %v", got)
	}
	// Real parent traversal is silently ignored (not ours to track).
	_ = cp.Record(filepath.Join(root, "..", "outside.txt"))
	if got := cp.ChangedLast(); len(got) != 1 {
		t.Fatalf("outside path tracked: %v", got)
	}
}
