package workledger

import (
	"os"
	"path/filepath"
	"testing"
)

func TestChangedAfterSomebodyElseWrites(t *testing.T) {
	ws := t.TempDir()
	f := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(f, []byte("ours\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewSeen()
	s.Note(f, []byte("ours\n"))

	changed, theirs, err := s.Changed(f)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("nothing changed yet")
	}

	if err := os.WriteFile(f, []byte("theirs\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, theirs, err = s.Changed(f)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("an outside write was not noticed")
	}
	if string(theirs) != "theirs\n" {
		t.Errorf("theirs = %q, want the content on disk", theirs)
	}
}

// Review Focus 4: a file this session never read is a first write, not a
// conflict. Treating it as one would prompt on every new file.
func TestAnUnseenFileIsNotAConflict(t *testing.T) {
	ws := t.TempDir()
	f := filepath.Join(ws, "new.txt")
	if err := os.WriteFile(f, []byte("somebody else made this\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewSeen()
	changed, _, err := s.Changed(f)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("a file this session has never seen must not be a conflict")
	}
}

// What we wrote is what we last saw, or our own write would look like
// somebody else's on the next one.
func TestOurOwnWriteIsNotAConflict(t *testing.T) {
	ws := t.TempDir()
	f := filepath.Join(ws, "a.txt")
	s := NewSeen()
	if err := os.WriteFile(f, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Note(f, []byte("v1\n"))
	if err := os.WriteFile(f, []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.Note(f, []byte("v2\n"))
	changed, _, err := s.Changed(f)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("our own second write was reported as somebody else's change")
	}
}

// A file that was seen and is now gone is a change: somebody removed it.
func TestADeletedFileIsAChange(t *testing.T) {
	ws := t.TempDir()
	f := filepath.Join(ws, "a.txt")
	if err := os.WriteFile(f, []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewSeen()
	s.Note(f, []byte("v1\n"))
	if err := os.Remove(f); err != nil {
		t.Fatal(err)
	}
	changed, theirs, err := s.Changed(f)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Error("a file that was seen and is now gone is a change")
	}
	if theirs != nil {
		t.Errorf("theirs should be nil for a deleted file, got %q", theirs)
	}
}

// NoteHash exists so the engine can feed the hash it already computed for a
// task document rather than hashing the same bytes twice.
func TestNoteHashMatchesNote(t *testing.T) {
	ws := t.TempDir()
	f := filepath.Join(ws, "a.txt")
	body := []byte("v1\n")
	if err := os.WriteFile(f, body, 0o644); err != nil {
		t.Fatal(err)
	}
	s := NewSeen()
	s.NoteHash(f, HashBytes(body))
	changed, _, err := s.Changed(f)
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Error("a hash noted directly must compare equal to the file it came from")
	}
}
