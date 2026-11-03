// Package credentials owns ~/.be-code/credentials.json: the keys and
// sign-in results a person stored from inside the harness. config.json
// never holds a secret; this file holds nothing else, at mode 0600, and
// an environment variable named by api_key_env always wins over it
// (config.Config.ProviderKey). A leaf: stdlib only, so config may import it.
package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"
)

// KindAPIKey is the one credential kind this version writes. A later kind
// (a refresh token) adds its own fields under its own kind; an entry of an
// unknown kind is kept verbatim and never yields a key.
const KindAPIKey = "api_key"

// FileName is the file's name under the dotdir.
const FileName = "credentials.json"

// ErrLooseMode is returned by Open when the file is readable by group or
// world; the wrapped error names the file and the chmod to run.
var ErrLooseMode = errors.New("credentials.json is readable by others")

// Credential is one provider's stored secret.
type Credential struct {
	Kind    string    `json:"kind"`
	Key     string    `json:"key,omitempty"`
	Source  string    `json:"source,omitempty"` // "pasted" | "oauth"
	Created time.Time `json:"created"`
}

type file struct {
	Version   int                        `json:"version"`
	Providers map[string]json.RawMessage `json:"providers"`
}

// Store is the file as read by Open. Put and Revoke re-read the file
// before writing so two sessions on one machine do not clobber each other.
type Store struct {
	path    string
	entries map[string]json.RawMessage
	corrupt bool
}

// Open reads dir/credentials.json. A missing file is an empty store; a
// file that does not parse is an empty store that refuses to be written;
// a loose mode is ErrLooseMode.
func Open(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, FileName), entries: map[string]json.RawMessage{}}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

// Path is the file's full path.
func (s *Store) Path() string { return s.path }

func (s *Store) load() error {
	s.entries, s.corrupt = map[string]json.RawMessage{}, false
	fi, err := os.Stat(s.path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	// Windows does not enforce modes on these paths, so the check is skipped there.
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: run chmod 600 %s", ErrLooseMode, s.path)
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		s.corrupt = true
		return nil
	}
	if f.Providers != nil {
		s.entries = f.Providers
	}
	return nil
}

// Key is the stored api_key credential for provider, if any.
func (s *Store) Key(provider string) (Credential, bool) {
	raw, ok := s.entries[provider]
	if !ok {
		return Credential{}, false
	}
	var c Credential
	if err := json.Unmarshal(raw, &c); err != nil || c.Kind != KindAPIKey || c.Key == "" {
		return Credential{}, false
	}
	return c, true
}

// Unsupported names the providers whose entry has a kind this version
// does not read, for doctor to report.
func (s *Store) Unsupported() []string {
	var out []string
	for name, raw := range s.entries {
		var c Credential
		if err := json.Unmarshal(raw, &c); err != nil || c.Kind != KindAPIKey {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Put stores c for provider, replacing any entry.
func (s *Store) Put(provider string, c Credential) error {
	if c.Kind == "" {
		c.Kind = KindAPIKey
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.mutate(func(m map[string]json.RawMessage) { m[provider] = raw })
}

// Revoke deletes provider's entry; a missing entry is a no-op and never
// creates the file.
func (s *Store) Revoke(provider string) error {
	if err := s.load(); err != nil {
		return err
	}
	if _, ok := s.entries[provider]; !ok && !s.corrupt {
		return nil
	}
	return s.mutate(func(m map[string]json.RawMessage) { delete(m, provider) })
}

func (s *Store) mutate(fn func(map[string]json.RawMessage)) error {
	if err := s.load(); err != nil {
		return err
	}
	if s.corrupt {
		return fmt.Errorf("%s does not parse; fix or remove it before storing a credential", s.path)
	}
	fn(s.entries)
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(file{Version: 1, Providers: s.entries}, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".credentials-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
