package workledger

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sync"
)

// Seen is the hash of each file as this session last read or wrote it. It is
// the same idea internal/engine already uses for task documents, where
// state.Docs is "the hash of each document as the engine last wrote it, so an
// outside edit is recognisable" (internal/engine/store.go).
//
// A file this session has never seen is never a conflict: it is a first read.
// That makes a missed Note a detection that silently never fires, which is
// why the spec lists every call site.
//
// It lives in memory for the life of the process. A file form existed once,
// keyed by pid and start time — which no later process ever has, so it could
// never be read back and only accumulated (ledger, 2026-10-10).
type Seen struct {
	mu   sync.Mutex
	hash map[string]string
}

// NewSeen is an empty store: nothing seen yet.
func NewSeen() *Seen {
	return &Seen{hash: map[string]string{}}
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
	s.hash[file] = hash
}

// Changed reports whether file differs from what this session last saw, and
// returns what is on disk now so a caller can show the difference. A seen
// file that is now gone is a change: somebody removed it.
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
			return true, nil, nil
		}
		return false, nil, err
	}
	if HashBytes(b) == was {
		return false, nil, nil
	}
	return true, b, nil
}
