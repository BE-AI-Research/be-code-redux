package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
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

// ServedElsewhere is Listen's answer when a live session already serves.
type ServedElsewhere struct{ Record Record }

func (e *ServedElsewhere) Error() string {
	return fmt.Sprintf("API is served by session %s (pid %d); this session is not serving", e.Record.SessionCode, e.Record.PID)
}
func (e *ServedElsewhere) Is(target error) bool { return target == ErrServedElsewhere }

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

// Listen binds the socket. On address-in-use it connects: a refusal means
// a stale socket, removed and re-bound; a live listener means another
// session serves and the error names it from its record.
func Listen(dir string, rec Record) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Join(dir, "apps"), 0o700); err != nil {
		return nil, err
	}
	if err := CheckDir(dir); err != nil {
		return nil, err
	}
	path := SocketPath(dir)
	l, err := net.Listen("unix", path)
	if err != nil {
		if c, derr := net.DialTimeout("unix", path, time.Second); derr == nil {
			c.Close()
			r, _ := Serving(dir)
			if r.SessionCode == "" {
				r = Record{Socket: path}
			}
			return nil, &ServedElsewhere{Record: r}
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
	data, _ := json.MarshalIndent(rec, "", "  ")
	if err := os.WriteFile(recordPath(dir), data, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// Release removes this process's socket and record. Another process's
// record is left alone.
func Release(dir string) {
	if r, ok := Serving(dir); ok && r.PID != os.Getpid() {
		return
	}
	_ = os.Remove(SocketPath(dir))
	_ = os.Remove(recordPath(dir))
}
