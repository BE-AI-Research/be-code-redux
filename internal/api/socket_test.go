package api

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
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

func TestProbeTimeoutNeverRemovesTheSocket(t *testing.T) {
	dir := shortDir(t)
	live, err := net.Listen("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	saved := probeDial
	defer func() { probeDial = saved }()
	probeDial = func(string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "unix", Err: syscall.ETIMEDOUT}
	}
	_, err = Listen(dir, Record{PID: os.Getpid(), SessionCode: "mine"})
	if !errors.Is(err, ErrServedElsewhere) {
		t.Fatalf("a probe timeout must be ServedElsewhere, got %v", err)
	}
	if _, err := os.Stat(SocketPath(dir)); err != nil {
		t.Fatalf("the socket must not be removed on a probe timeout: %v", err)
	}
}

func TestConcurrentListenOneWins(t *testing.T) {
	dir := shortDir(t)
	const n = 2
	type result struct {
		l   net.Listener
		err error
	}
	results := make(chan result, n)
	var start sync.WaitGroup
	start.Add(1)
	for i := 0; i < n; i++ {
		go func(i int) {
			start.Wait()
			l, err := Listen(dir, Record{PID: os.Getpid(), SessionCode: "c" + strconv.Itoa(i)})
			results <- result{l, err}
		}(i)
	}
	start.Done()
	var winners []net.Listener
	var losers []error
	for i := 0; i < n; i++ {
		r := <-results
		if r.err == nil {
			winners = append(winners, r.l)
		} else {
			losers = append(losers, r.err)
		}
	}
	defer func() {
		for _, w := range winners {
			w.Close()
		}
	}()
	if len(winners) != 1 || len(losers) != 1 {
		t.Fatalf("want exactly one winner, got %d winners and %d losers", len(winners), len(losers))
	}
	var se *ServedElsewhere
	if !errors.As(losers[0], &se) && !strings.Contains(losers[0].Error(), "another session is binding") {
		t.Fatalf("the loser must name the other session or the lock: %v", losers[0])
	}
	c, err := net.Dial("unix", SocketPath(dir))
	if err != nil {
		t.Fatalf("the winner's socket must still be connectable: %v", err)
	}
	c.Close()
}

func TestReleaseLeavesAnotherProcessesRecord(t *testing.T) {
	dir := shortDir(t)
	// The parent is alive and is not us: its record must survive our Release.
	rec := Record{PID: os.Getppid(), Socket: SocketPath(dir), SessionCode: "theirs", Started: time.Now()}
	if err := writeRecord(dir, rec); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SocketPath(dir), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	Release(dir)
	if _, err := os.Stat(SocketPath(dir)); err != nil {
		t.Fatalf("Release must not remove another process's socket: %v", err)
	}
	if got, ok := Serving(dir); !ok || got.SessionCode != "theirs" {
		t.Fatalf("Release must not remove another process's record: %+v %v", got, ok)
	}
}

func TestOldLockOfDeadHolderIsReplaced(t *testing.T) {
	dir := shortDir(t)
	_ = os.MkdirAll(filepath.Join(dir, "apps"), 0o700)
	lock := lockPath(dir)
	if err := os.WriteFile(lock, []byte("999999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-20 * time.Second)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	l, err := Listen(dir, Record{PID: os.Getpid(), SessionCode: "after"})
	if err != nil {
		t.Fatalf("an old lock with a dead holder must be replaced: %v", err)
	}
	l.Close()
	if _, err := os.Stat(lock); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the lock is released after Listen")
	}
}

func TestFreshLockOfLiveHolderIsRefused(t *testing.T) {
	dir := shortDir(t)
	_ = os.MkdirAll(filepath.Join(dir, "apps"), 0o700)
	pid := os.Getpid()
	if err := os.WriteFile(lockPath(dir), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Listen(dir, Record{PID: pid, SessionCode: "blocked"})
	if err == nil || !strings.Contains(err.Error(), strconv.Itoa(pid)) {
		t.Fatalf("a fresh lock held by a live pid must be refused naming it, got %v", err)
	}
	if _, serr := os.Stat(lockPath(dir)); serr != nil {
		t.Fatal("a refused Listen must leave the holder's lock alone")
	}
}
