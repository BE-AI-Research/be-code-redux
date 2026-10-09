package workledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The record carries what this session is working on, which is what makes the
// startup notice worth reading at all (§4, §5, and §14's named mitigation for
// the notice being ignorable). It is pulled when the record is written rather
// than pushed at a turn boundary: Events is owned by the UIs, so anything
// buildAgent assigned there would be overwritten.
func TestTheRecordCarriesTaskAndWrittenFiles(t *testing.T) {
	ws := gitRepo(t)
	l := openFor(t, ws)
	turn := 1
	l.SetState(func() (string, int) { return "003", turn })

	l.OnWrote(filepath.Join(ws, "internal", "tui", "chat.go"))
	l.OnWrote(filepath.Join(ws, ".be-code", "tasks", "003-thing.md"))

	if l.rec.Task != "003" {
		t.Errorf("task = %q, want 003", l.rec.Task)
	}
	if len(l.rec.Writing) != 2 {
		t.Fatalf("writing = %v, want both paths", l.rec.Writing)
	}
	for _, want := range []string{"internal/tui/chat.go", ".be-code/tasks/003-thing.md"} {
		if !containsPath(l.rec.Writing, want) {
			t.Errorf("writing should hold %q, got %v", want, l.rec.Writing)
		}
	}
	// Relative, not absolute: a notice naming /home/…/internal/tui/chat.go is
	// noise, and the reader already knows the workspace.
	for _, p := range l.rec.Writing {
		if filepath.IsAbs(p) {
			t.Errorf("writing holds an absolute path: %q", p)
		}
	}
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339) }

func containsPath(have []string, want string) bool {
	for _, p := range have {
		if p == want {
			return true
		}
	}
	return false
}

// The engine's turn counter is the turn boundary. When it moves, what this
// session is writing starts again.
func TestWritingResetsWhenTheTurnChanges(t *testing.T) {
	ws := gitRepo(t)
	l := openFor(t, ws)
	turn := 1
	l.SetState(func() (string, int) { return "", turn })

	l.OnWrote(filepath.Join(ws, "a.go"))
	l.OnWrote(filepath.Join(ws, "b.go"))
	if len(l.rec.Writing) != 2 {
		t.Fatalf("writing = %v, want two", l.rec.Writing)
	}

	turn = 2
	l.OnWrote(filepath.Join(ws, "c.go"))
	if len(l.rec.Writing) != 1 || l.rec.Writing[0] != "c.go" {
		t.Errorf("writing = %v, want only the new turn's file", l.rec.Writing)
	}
	if l.rec.Recent {
		t.Error("with a turn counter the list is this turn's, not merely recent")
	}
}

// With the engine disabled there is no turn counter, so the list cannot be
// per-turn. It is capped and says so, rather than claiming turn semantics it
// does not have.
func TestWithoutATurnCounterTheListIsCappedAndSaysSo(t *testing.T) {
	ws := gitRepo(t)
	l := openFor(t, ws)
	// No SetState at all: the engine is off.
	for i := 0; i < writingCap+5; i++ {
		l.OnWrote(filepath.Join(ws, "f"+string(rune('a'+i))+".go"))
	}
	if len(l.rec.Writing) != writingCap {
		t.Errorf("writing holds %d paths, want the cap of %d", len(l.rec.Writing), writingCap)
	}
	if !l.rec.Recent {
		t.Error("without a turn counter the list must be marked as merely recent")
	}
	// The cap keeps the newest, not the oldest.
	if !containsPath(l.rec.Writing, "fo.go") {
		t.Errorf("the cap dropped the newest instead of the oldest: %v", l.rec.Writing)
	}
}

// The same path twice in one turn is one entry.
func TestWritingDoesNotRepeatAPath(t *testing.T) {
	ws := gitRepo(t)
	l := openFor(t, ws)
	turn := 1
	l.SetState(func() (string, int) { return "", turn })
	l.OnWrote(filepath.Join(ws, "a.go"))
	l.OnWrote(filepath.Join(ws, "a.go"))
	if len(l.rec.Writing) != 1 {
		t.Errorf("writing = %v, want one entry", l.rec.Writing)
	}
}

// And the notice reads as the spec's §5 example rather than a bare pid.
func TestNoticeNamesTheTaskAndTheFiles(t *testing.T) {
	ws := gitRepo(t)
	mine := openFor(t, ws)
	dir, _ := Dir()
	stubAlive(t, map[int]bool{4242: true, 99: true})

	other := Record{
		PID: 99, Workspace: ws, Branch: "feat/thing", Task: "003",
		Writing: []string{"internal/tui/chat.go"},
		Started: nowStamp(), Updated: nowStamp(),
	}
	if err := Save(dir, other); err != nil {
		t.Fatal(err)
	}
	line := mine.NoticeLine()
	for _, want := range []string{"99", "feat/thing", "003", "internal/tui/chat.go"} {
		if !strings.Contains(line, want) {
			t.Errorf("the notice should mention %q: %q", want, line)
		}
	}
	// A record with no turn counter behind it must not be reported as if it
	// were this-turn truth.
	other.PID, other.Recent = 98, true
	if err := Save(dir, other); err != nil {
		t.Fatal(err)
	}
	stubAlive(t, map[int]bool{4242: true, 98: true})
	if l := mine.NoticeLine(); !strings.Contains(l, "recently") {
		t.Errorf("a recent-only list must be worded as such: %q", l)
	}
	_ = os.Remove(Record{PID: 98, Started: other.Started}.Path(dir))
}
