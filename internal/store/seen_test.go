package store

import (
	"strings"
	"testing"
)

// Spec §10: the session file's load and save are a read site too. The
// HostPID guard already handles the dangerous half — a different live host
// owning the file disables autosave — and this only feeds the ledger what it
// saw, so a notice can name what that host is doing.
func TestSaveAndLoadReportWhatTheySaw(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var paths []string
	saved := OnSessionRead
	OnSessionRead = func(p string, _ []byte) { paths = append(paths, p) }
	t.Cleanup(func() { OnSessionRead = saved })

	s := &Session{ID: "sess-seen-test"}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("Save reported %d reads, want 1: %v", len(paths), paths)
	}
	if !strings.Contains(paths[0], "sess-seen-test") {
		t.Errorf("the path should be the session's: %q", paths[0])
	}

	paths = nil
	if _, err := Load("sess-seen-test"); err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Errorf("Load reported %d reads, want 1: %v", len(paths), paths)
	}
}

func TestNoHookIsFine(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	saved := OnSessionRead
	OnSessionRead = nil
	t.Cleanup(func() { OnSessionRead = saved })
	s := &Session{ID: "sess-nohook"}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
}
