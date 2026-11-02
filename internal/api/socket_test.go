package api

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func shortDir(t *testing.T) string {
	// Unix socket paths are limited to ~104 bytes; t.TempDir is often too long.
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "beapi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestListenWritesRecordAndReleaseRemovesIt(t *testing.T) {
	dir := shortDir(t)
	l, err := Listen(dir, Record{PID: os.Getpid(), SessionCode: "abc123", Started: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	st, err := os.Stat(SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %o", st.Mode().Perm())
	}
	rec, ok := Serving(dir)
	if !ok || rec.PID != os.Getpid() || rec.SessionCode != "abc123" {
		t.Fatalf("record: %+v %v", rec, ok)
	}
	l.Close()
	Release(dir)
	if _, ok := Serving(dir); ok {
		t.Fatal("after Release nothing is serving")
	}
	if _, err := os.Stat(SocketPath(dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the socket file is removed")
	}
}

func TestSecondListenerIsToldWhoServes(t *testing.T) {
	dir := shortDir(t)
	l, err := Listen(dir, Record{PID: os.Getpid(), SessionCode: "first1"})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() { // accept so the probe's connect succeeds
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	_, err = Listen(dir, Record{PID: os.Getpid() + 1, SessionCode: "second"})
	var se *ServedElsewhere
	if !errors.As(err, &se) || se.Record.SessionCode != "first1" {
		t.Fatalf("want ServedElsewhere naming first1, got %v", err)
	}
	if !errors.Is(err, ErrServedElsewhere) {
		t.Fatal("errors.Is must match the sentinel")
	}
}

func TestStaleSocketIsReclaimed(t *testing.T) {
	dir := shortDir(t)
	_ = os.MkdirAll(dir, 0o700)
	// A socket file nobody listens on, and a record from a dead pid.
	l, _ := net.Listen("unix", SocketPath(dir))
	l.Close() // Close removes the file on Go's unix listener; recreate a dead one:
	f, _ := os.Create(SocketPath(dir))
	f.Close()
	_ = os.WriteFile(filepath.Join(dir, "server.json"), []byte(`{"pid": 999999999, "session_code": "dead"}`), 0o600)
	if _, ok := Serving(dir); ok {
		t.Fatal("a record with a dead pid is not serving")
	}
	l2, err := Listen(dir, Record{PID: os.Getpid(), SessionCode: "new"})
	if err != nil {
		t.Fatalf("a stale socket must be reclaimed: %v", err)
	}
	l2.Close()
}

func TestCheckDirRefusesOpenModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes are not enforced on Windows")
	}
	dir := shortDir(t)
	_ = os.MkdirAll(filepath.Join(dir, "apps"), 0o700)
	_ = os.Chmod(dir, 0o755)
	err := CheckDir(dir)
	if err == nil || !strings.Contains(err.Error(), "chmod 700") {
		t.Fatalf("want the chmod hint, got %v", err)
	}
	_ = os.Chmod(dir, 0o700)
	_ = os.Chmod(filepath.Join(dir, "apps"), 0o750)
	if err := CheckDir(dir); err == nil {
		t.Fatal("the apps directory is checked too")
	}
	_ = os.Chmod(filepath.Join(dir, "apps"), 0o700)
	if err := CheckDir(dir); err != nil {
		t.Fatal(err)
	}
}
