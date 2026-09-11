package ide

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// writeLock stores a lock as <pid>-<port>.json (Discover reads every
// *.json, so two test locks may share a pid).
func writeLock(t *testing.T, dir string, l Lock, mtime time.Time) string {
	t.Helper()
	b, _ := json.Marshal(l)
	p := filepath.Join(dir, strconv.Itoa(l.PID)+"-"+strconv.Itoa(l.Port)+".json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, mtime, mtime)
	return p
}

// Picks the lock whose workspace folder contains the workspace; ignores
// and deletes locks whose process is gone.
func TestDiscoverPrefersMatchingWorkspaceAndCleansStale(t *testing.T) {
	dir := t.TempDir()
	me := os.Getpid()
	now := time.Now()
	writeLock(t, dir, Lock{PID: me, Port: 1, Token: "a", WorkspaceFolders: []string{"/tmp/other"}}, now)
	want := writeLock(t, dir, Lock{PID: me, Port: 2, Token: "b", WorkspaceFolders: []string{"/tmp/proj"}}, now.Add(-time.Hour))
	stale := writeLock(t, dir, Lock{PID: 999999999, Port: 3, Token: "c", WorkspaceFolders: []string{"/tmp/proj"}}, now)
	l, err := Discover(dir, "/tmp/proj/sub")
	if err != nil || l == nil || l.Port != 2 {
		t.Fatalf("got %+v err=%v", l, err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale lock not removed")
	}
	_ = want
}

func TestDiscoverFallsBackToNewest(t *testing.T) {
	dir := t.TempDir()
	me := os.Getpid()
	writeLock(t, dir, Lock{PID: me, Port: 1, Token: "a", WorkspaceFolders: []string{"/a"}}, time.Now().Add(-time.Hour))
	writeLock(t, dir, Lock{PID: me, Port: 2, Token: "b", WorkspaceFolders: []string{"/b"}}, time.Now())
	l, err := Discover(dir, "/elsewhere")
	if err != nil || l == nil || l.Port != 2 {
		t.Fatalf("got %+v err=%v", l, err)
	}
}

func TestDiscoverNoneIsNilNil(t *testing.T) {
	l, err := Discover(t.TempDir(), "/x")
	if err != nil || l != nil {
		t.Fatalf("got %+v err=%v", l, err)
	}
}
