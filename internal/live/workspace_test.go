package live

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// The same folder reached from two shells on Windows can differ in case and
// in a trailing separator; it is one workspace, so the second launch joins
// the first one's live session instead of starting a copy of it.
func TestSameWorkspaceFoldsCaseOnWindows(t *testing.T) {
	if !sameWorkspace("/Users/Shayne/Unreal/MyGame", "/users/shayne/unreal/mygame/", true) {
		t.Fatal("case and trailing separator must not make two workspaces on Windows")
	}
	if sameWorkspace("/Users/Shayne/Unreal/MyGame", "/users/shayne/unreal/mygame", false) {
		t.Fatal("case matters where the file system says it does")
	}
	if sameWorkspace("/a/b", "/a/c", true) {
		t.Fatal("different folders matched")
	}
}

// Two spellings of one folder that only the file system can equate (a
// symlink here; an 8.3 short name on Windows) are one workspace too.
func TestSameWorkspaceResolvesLinks(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "ws")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("no symlinks here")
	}
	if !sameWorkspace(real, link, false) {
		t.Fatal("a link to the workspace is the workspace")
	}
}

// Launched from an SSH session on Windows, the host must outlive the session:
// the first try breaks away from the session's job; a job that forbids it
// gets a second, plain try and the caller is told it will not survive.
func TestStartDetachedFallsBackWhenBreakawayIsRefused(t *testing.T) {
	var tries []bool
	broke, err := startDetached(func(breakaway bool) error {
		tries = append(tries, breakaway)
		if breakaway {
			return errors.New("Access is denied.")
		}
		return nil
	})
	if err != nil || broke || len(tries) != 2 || !tries[0] || tries[1] {
		t.Fatalf("broke=%v err=%v tries=%v", broke, err, tries)
	}
	tries = nil
	broke, err = startDetached(func(breakaway bool) error { tries = append(tries, breakaway); return nil })
	if err != nil || !broke || len(tries) != 1 {
		t.Fatalf("breakaway allowed: broke=%v err=%v tries=%v", broke, err, tries)
	}
	if _, err := startDetached(func(bool) error { return errors.New("no such file") }); err == nil {
		t.Fatal("a start that fails both ways must report it")
	}
}
