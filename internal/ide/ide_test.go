package ide

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// A zero (or negative) PID can never be a live process; Discover must treat
// it like a dead-process lock and remove the file rather than reporting it
// as an editor session forever (processAlive(0) targets the caller's own
// process group on Linux and would otherwise report "alive").
func TestDiscoverRemovesZeroPIDLock(t *testing.T) {
	dir := t.TempDir()
	p := writeLock(t, dir, Lock{PID: 0, Port: 5, Token: "z", WorkspaceFolders: []string{"/tmp/proj"}}, time.Now())
	l, err := Discover(dir, "/tmp/proj")
	if err != nil || l != nil {
		t.Fatalf("got %+v err=%v", l, err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("zero-pid lock not removed")
	}
}

// DiscoverCovering never falls back to an unrelated workspace's lock, and
// returns every covering lock newest first.
func TestDiscoverCoveringReturnsOnlyCoveringLocksNewestFirst(t *testing.T) {
	dir := t.TempDir()
	me := os.Getpid()
	now := time.Now()
	writeLock(t, dir, Lock{PID: me, Port: 1, Token: "a", IDEName: "vscode", WorkspaceFolders: []string{"/tmp/other"}}, now)
	writeLock(t, dir, Lock{PID: me, Port: 2, Token: "b", IDEName: "visualstudio", WorkspaceFolders: []string{"/tmp/proj"}}, now.Add(-time.Hour))
	writeLock(t, dir, Lock{PID: me, Port: 3, Token: "c", IDEName: "vscode", WorkspaceFolders: []string{"/tmp/proj"}}, now)

	locks, err := DiscoverCovering(dir, "/tmp/proj/sub")
	if err != nil {
		t.Fatal(err)
	}
	if len(locks) != 2 || locks[0].Port != 3 || locks[1].Port != 2 {
		t.Fatalf("got %+v", locks)
	}
}

func TestDiscoverCoveringEmptyWhenNoneCover(t *testing.T) {
	dir := t.TempDir()
	me := os.Getpid()
	writeLock(t, dir, Lock{PID: me, Port: 1, Token: "a", WorkspaceFolders: []string{"/tmp/other"}}, time.Now())
	locks, err := DiscoverCovering(dir, "/tmp/proj")
	if err != nil || len(locks) != 0 {
		t.Fatalf("got %+v err=%v", locks, err)
	}
}

func TestLockCovers(t *testing.T) {
	l := &Lock{WorkspaceFolders: []string{"/tmp/proj"}}
	if !l.Covers("/tmp/proj") || !l.Covers("/tmp/proj/sub") {
		t.Fatal("expected coverage")
	}
	if l.Covers("/tmp/projother") || l.Covers("/tmp/other") {
		t.Fatal("expected no coverage")
	}
}

// Windows paths are case-insensitive and the two sides get theirs from
// different places: Visual Studio reports the solution as it is on disk
// (C:\Dev\MyApp), a PowerShell cd hands the process whatever was typed
// (C:\dev\myapp), and VS Code lower-cases the drive letter (c:\...). On the
// path that attaches without --ide, coverage is the whole decision and a miss
// is silent, so the comparison has to fold case there. It cannot be tried on
// a real Windows filesystem from here, hence the pure function.
func TestCoversFoldsCaseOnWindowsOnly(t *testing.T) {
	const win, unix = '\\', '/'
	for _, c := range []struct {
		name       string
		ws, folder string
		sep        byte
		fold       bool
		want       bool
	}{
		{"windows, same case", `C:\Dev\MyApp\src`, `C:\Dev\MyApp`, win, true, true},
		{"windows, typed in lower case", `C:\dev\myapp\src`, `C:\Dev\MyApp`, win, true, true},
		{"windows, lower-case drive letter", `c:\Dev\MyApp`, `C:\Dev\MyApp`, win, true, true},
		{"windows, the folder itself", `c:\dev\myapp`, `C:\Dev\MyApp`, win, true, true},
		{"windows, a sibling with the same prefix", `C:\Dev\MyApp2`, `C:\dev\myapp`, win, true, false},
		{"windows, another project", `C:\Dev\Other`, `C:\Dev\MyApp`, win, true, false},
		{"unix stays case-sensitive", `/home/u/Proj/src`, `/home/u/proj`, unix, false, false},
		{"unix, nested", `/home/u/proj/src`, `/home/u/proj`, unix, false, true},
	} {
		if got := covers(c.ws, c.folder, c.sep, c.fold); got != c.want {
			t.Errorf("%s: covers(%q, %q) = %v, want %v", c.name, c.ws, c.folder, got, c.want)
		}
	}
}

func TestLockDirCreatesDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	dir, err := LockDir()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("LockDir did not create %q: %v", dir, err)
	}
}

func TestConnectDialsLoopbackWithToken(t *testing.T) {
	port, got := fakeIDEServer(t, fakeOpts{token: "tok"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sess, err := Connect(ctx, &Lock{Port: port, Token: "tok", IDEName: "vscode"})
	if err != nil {
		t.Fatal(err)
	}
	if *got != "tok" {
		t.Fatalf("token received = %q", *got)
	}
	if len(sess.Client.Tools()) != 1 || sess.Client.Tools()[0].Name != "ide_diagnostics" {
		t.Fatalf("tools = %+v", sess.Client.Tools())
	}
	sess.Close()
	sess.Close() // idempotent

	badPort, _ := fakeIDEServer(t, fakeOpts{token: "tok"})
	if _, err := Connect(ctx, &Lock{Port: badPort, Token: "wrong", IDEName: "vscode"}); err == nil || !strings.Contains(err.Error(), "bad token") {
		t.Fatalf("expected bad-token error, got %v", err)
	}
}
