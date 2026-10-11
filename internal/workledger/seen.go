package workledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// deleted is the hash recorded for a file that was seen and is now gone, so
// "somebody removed it" is a change rather than indistinguishable from never
// having seen it.
const deleted = "-"

// Seen is the hash of each file as this session last read or wrote it. It is
// the same idea internal/engine already uses for task documents, where
// state.Docs is "the hash of each document as the engine last wrote it, so an
// outside edit is recognisable" (internal/engine/store.go).
//
// A file this session has never seen is never a conflict: it is a first read.
// That makes a missed Note a detection that silently never fires, which is
// why the spec lists every call site.
type Seen struct {
	mu    sync.Mutex
	path  string
	hash  map[string]string
	dirty bool
}

// NewSeen opens the store at path, loading whatever is there. In production
// nothing is: the path is this process's own and Close removes it, so the
// store is in-memory for the life of the process. The file form stays for a
// caller that wants to carry one across a restart.
func NewSeen(path string) *Seen {
	s := &Seen{path: path, hash: map[string]string{}}
	if b, err := os.ReadFile(path); err == nil {
		var m map[string]string
		if json.Unmarshal(b, &m) == nil && m != nil {
			s.hash = m
		}
	}
	return s
}

// HashBytes is the hash both Note and the engine's own document hash use.
// SHA-256 of the exact bytes, with no normalisation: a byte that changed is
// a change.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Note records content as what this session last saw at file.
func (s *Seen) Note(file string, content []byte) { s.NoteHash(file, HashBytes(content)) }

// NoteHash records a hash computed elsewhere — the engine's state.Docs, which
// already hashes every document it writes for exactly this purpose.
func (s *Seen) NoteHash(file, hash string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hash[file] != hash {
		s.hash[file] = hash
		s.dirty = true
	}
}

// Changed reports whether file differs from what this session last saw, and
// returns what is on disk now so a caller can show the difference.
func (s *Seen) Changed(file string) (bool, []byte, error) {
	s.mu.Lock()
	was, seen := s.hash[file]
	s.mu.Unlock()
	if !seen {
		return false, nil, nil // never read it; a first write is not a conflict
	}
	b, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return was != deleted, nil, nil
		}
		return false, nil, err
	}
	if HashBytes(b) == was {
		return false, nil, nil
	}
	return true, b, nil
}

// Forget records that file is gone, so its removal is a change rather than
// looking unseen.
func (s *Seen) Forget(file string) { s.NoteHash(file, deleted) }

// Flush persists the hashes. Ledger.Close does not call it — a store named
// by pid and start time has no reader in a later process — so this is for a
// caller that keys the path by something that survives. A failure is returned
// but never fatal: in-memory detection is unaffected.
func (s *Seen) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return nil
	}
	b, err := json.Marshal(s.hash)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		os.Remove(tmp)
		return err
	}
	s.dirty = false
	return nil
}
