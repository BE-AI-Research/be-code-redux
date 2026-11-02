package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/procattr"
)

// Record advertises the serving session, like a live record: who to name
// when a second session finds the socket taken, and what /api and doctor
// report. Pruned when its pid is gone.
type Record struct {
	PID         int       `json:"pid"`
	Socket      string    `json:"socket"`
	SessionCode string    `json:"session_code"`
	Started     time.Time `json:"started"`
}

var ErrServedElsewhere = errors.New("the API is served by another session")

// ServedElsewhere is Listen's answer when a live session already serves, or
// when the socket cannot be probed and so cannot be reclaimed.
type ServedElsewhere struct{ Record Record }

func (e *ServedElsewhere) Error() string {
	if e.Record.SessionCode == "" {
		return fmt.Sprintf("API: another session serves on %s; this session is not serving", e.Record.Socket)
	}
	return fmt.Sprintf("API is served by session %s (pid %d); this session is not serving", e.Record.SessionCode, e.Record.PID)
}
func (e *ServedElsewhere) Is(target error) bool { return target == ErrServedElsewhere }

// lockStale is how old a binding lock must be before a dead holder's lock
// is taken over. A live holder is never taken over, however old.
const lockStale = 10 * time.Second

// probeDial connects to the socket to learn whether anyone listens on it.
// A variable so tests can exercise the probe-failure branches.
var probeDial = func(path string) (net.Conn, error) {
	return net.DialTimeout("unix", path, time.Second)
}

// Dir is ~/.be-code/api.
func Dir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".be-code", "api"), nil
}

func SocketPath(dir string) string { return filepath.Join(dir, "api.sock") }
func recordPath(dir string) string { return filepath.Join(dir, "server.json") }
func lockPath(dir string) string   { return filepath.Join(dir, "api.lock") }

// CheckDir refuses to serve from a directory other users could read: the
// socket's mode is the whole boundary on Unix. Windows does not enforce
// modes on these paths, so the check is skipped there.
func CheckDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	for _, d := range []string{dir, filepath.Join(dir, "apps")} {
		st, err := os.Stat(d)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if st.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("api: %s is mode %04o; fix with chmod 700 %s", d, st.Mode().Perm(), d)
		}
	}
	return nil
}

// Serving is the live record, if any: a record whose pid is gone is pruned
// and reported as nobody.
func Serving(dir string) (Record, bool) {
	raw, err := os.ReadFile(recordPath(dir))
	if err != nil {
		return Record{}, false
	}
	var r Record
	if json.Unmarshal(raw, &r) != nil || r.PID <= 0 || !procattr.Alive(r.PID) {
		_ = os.Remove(recordPath(dir))
		return Record{}, false
	}
	return r, true
}

// lockHolder reads the pid a lock file names; 0 when it names none.
func lockHolder(path string) int {
	raw, _ := os.ReadFile(path)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	return pid
}

// takeOverStaleLock moves a dead holder's lock out of the way. The rename is
// atomic, so of several racers only one moves the file. The moved file is then
// judged again, by age and by pid: a fresh lock, or one whose pid is still
// alive, is a live binder's (it may have just been created, still empty), so it
// is put back with a link that fails rather than overwrites a newer lock.
func takeOverStaleLock(path string) bool {
	moved := fmt.Sprintf("%s.stale-%d-%d", path, os.Getpid(), time.Now().UnixNano())
	if err := os.Rename(path, moved); err != nil {
		return false
	}
	if st, err := os.Stat(moved); err == nil && time.Since(st.ModTime()) > lockStale && !procattr.Alive(lockHolder(moved)) {
		_ = os.Remove(moved)
		return true
	}
	if os.Link(moved, path) == nil {
		_ = os.Remove(moved)
	}
	return false
}

// acquireLock takes <dir>/api.lock with O_EXCL, so one binding at a time.
// A lock older than lockStale whose holder is dead is taken over once; any
// other existing lock is refused, naming its holder. The returned func
// releases the lock.
func acquireLock(dir string) (func(), error) {
	path := lockPath(dir)
	for attempt := 0; attempt < 2; attempt++ {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			_, _ = fmt.Fprintf(f, "%d\n", os.Getpid())
			_ = f.Close()
			return func() { _ = os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		st, serr := os.Stat(path)
		if serr != nil {
			continue // released between our attempt and the stat
		}
		pid := lockHolder(path)
		if attempt == 0 && time.Since(st.ModTime()) > lockStale && !procattr.Alive(pid) && takeOverStaleLock(path) {
			continue
		}
		return nil, fmt.Errorf("api: another session is binding the socket (pid %d holds %s)", pid, path)
	}
	return nil, fmt.Errorf("api: could not take %s", path)
}

// writeRecord replaces server.json atomically, so a concurrent Serving never
// reads a half-written record.
func writeRecord(dir string, rec Record) error {
	data, _ := json.MarshalIndent(rec, "", "  ")
	tmp := recordPath(dir) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(tmp, 0o600); err != nil {
			_ = os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, recordPath(dir)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// busyRecord reports whether server.json names a live pid. Listen holds the
// binding lock until its record is written, so under the lock an absent,
// unparseable or dead-pid record means no session is serving.
func busyRecord(dir string) bool {
	raw, err := os.ReadFile(recordPath(dir))
	if err != nil {
		return false
	}
	var r Record
	return json.Unmarshal(raw, &r) == nil && r.PID > 0 && procattr.Alive(r.PID)
}

// servedElsewhere names whoever serves, from the record when there is one.
func servedElsewhere(dir, path string) error {
	r, _ := Serving(dir)
	if r.PID == 0 {
		r = Record{Socket: path}
	}
	return &ServedElsewhere{Record: r}
}

// Listen binds the socket under the binding lock. When the bind fails it
// probes the socket. A successful connect means another session serves. A
// failed connect means a stale socket only when the record names no live
// pid; a live record means a busy session, and the socket is left alone.
// Errno is never consulted, because Windows reports these failures with
// codes Go's syscall package does not map.
func Listen(dir string, rec Record) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Join(dir, "apps"), 0o700); err != nil {
		return nil, err
	}
	if err := CheckDir(dir); err != nil {
		return nil, err
	}
	unlock, err := acquireLock(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()

	path := SocketPath(dir)
	l, err := net.Listen("unix", path)
	if err != nil {
		if c, derr := probeDial(path); derr == nil {
			c.Close()
			return nil, servedElsewhere(dir, path)
		}
		if busyRecord(dir) {
			return nil, servedElsewhere(dir, path)
		}
		_ = os.Remove(path)
		_ = os.Remove(recordPath(dir))
		if l, err = net.Listen("unix", path); err != nil {
			return nil, err
		}
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(path, 0o600)
	}
	rec.Socket = path
	if rec.Started.IsZero() {
		rec.Started = time.Now()
	}
	if err := writeRecord(dir, rec); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Release removes this process's socket and record. It acts only when
// server.json names this process; any other record, or none, is left alone.
// Our own listener's Close already unlinks the socket file.
func Release(dir string) {
	raw, err := os.ReadFile(recordPath(dir))
	if err != nil {
		return
	}
	var r Record
	if json.Unmarshal(raw, &r) != nil || r.PID != os.Getpid() {
		return
	}
	_ = os.Remove(SocketPath(dir))
	_ = os.Remove(recordPath(dir))
}
