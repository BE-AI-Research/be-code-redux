package live

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
)

// Record advertises a live session host.
type Record struct {
	Code      string    `json:"code"`
	PID       int       `json:"pid"`
	Socket    string    `json:"socket"`
	Workspace string    `json:"workspace"`
	Model     string    `json:"model"`
	StartedAt time.Time `json:"startedAt"`
	Token     string    `json:"token"`
}

// Dir is ~/.be-code/live, created on demand.
func Dir() (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(base, "live")
	return d, os.MkdirAll(d, 0o700)
}

func SocketPath(dir, code string) string { return filepath.Join(dir, code+".sock") }
func recordPath(dir, code string) string { return filepath.Join(dir, code+".json") }

// Save writes the record atomically with mode 0600.
func (r Record) Save(dir string) error {
	b, err := json.MarshalIndent(r, "", " ")
	if err != nil {
		return err
	}
	p := recordPath(dir, r.Code)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func Load(dir, code string) (*Record, error) {
	b, err := os.ReadFile(recordPath(dir, code))
	if err != nil {
		return nil, err
	}
	var r Record
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	if r.Code == "" || r.Socket == "" {
		return nil, errors.New("live: malformed record")
	}
	return &r, nil
}

// List returns live records; records whose process is gone are removed
// along with their stale socket files.
func List(dir string) ([]Record, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Record
	alive := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		code := strings.TrimSuffix(e.Name(), ".json")
		r, err := Load(dir, code)
		if err != nil || r.PID <= 0 || !processAlive(r.PID) {
			_ = Remove(dir, code)
			continue
		}
		alive[code] = true
		out = append(out, *r)
	}
	pruneLogs(dir, entries, alive, time.Now().Add(-LogMaxAge))
	return out, nil
}

// LogMaxAge is how long a dead host's log is kept: long enough to read
// after a bad session, short enough that the directory does not grow
// without bound.
const LogMaxAge = 7 * 24 * time.Hour

// pruneLogs removes <code>.log files whose host is gone and whose last
// write is older than cutoff. A live host's log is never touched.
func pruneLogs(dir string, entries []os.DirEntry, alive map[string]bool, cutoff time.Time) {
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		code := strings.TrimSuffix(e.Name(), ".log")
		if alive[code] {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
}

func Remove(dir, code string) error {
	err := os.Remove(recordPath(dir, code))
	_ = os.Remove(SocketPath(dir, code))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// LiveCode returns the record for code when a host is live under it, or nil
// when there is none. List prunes records whose process is gone, so a stale
// record never passes for a live session.
func LiveCode(dir, code string) *Record {
	lives, _ := List(dir)
	for i := range lives {
		if lives[i].Code == code {
			return &lives[i]
		}
	}
	return nil
}
