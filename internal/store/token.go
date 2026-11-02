package store

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"time"
)

// tokenPrefix tags the token format so a later shape can be told apart.
const tokenPrefix = "sess_1_"

// MaxTokenGrace bounds how long a rotated-out token stays valid.
const MaxTokenGrace = 24 * time.Hour

// NewToken is a fresh session token: the format tag plus 16 bytes from the
// OS random source, base64url without padding. A random source that fails
// is a token nobody may trust, so it panics.
func NewToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("store: random source failed: " + err.Error())
	}
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(b[:])
}

// AccessToken is the session's token, minted on first use.
func (s *Session) AccessToken() string {
	if s.Token == "" {
		s.Token = NewToken()
	}
	return s.Token
}

// Rotate mints a new token. With a grace, the old token keeps working until
// now+grace so a caller can hand over; with zero grace it stops at once.
// A grace outside 0 to MaxTokenGrace is refused.
func (s *Session) Rotate(grace time.Duration) error {
	if grace < 0 || grace > MaxTokenGrace {
		return fmt.Errorf("grace %s is outside 0 to 24h", grace)
	}
	old := s.AccessToken()
	if grace == 0 {
		s.PrevToken, s.PrevUntil = "", time.Time{}
	} else {
		s.PrevToken, s.PrevUntil = old, time.Now().Add(grace)
	}
	s.Token = NewToken()
	s.TokenRotatedAt = time.Now()
	return nil
}

// Accepts reports whether tok addresses this session: the current token,
// or the previous one while its grace lasts. It compares in constant time
// and clears an expired grace token on the way.
func (s *Session) Accepts(tok string) bool {
	if tok == "" || s.Token == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(tok), []byte(s.Token)) == 1 {
		return true
	}
	if s.PrevToken == "" {
		return false
	}
	if !time.Now().Before(s.PrevUntil) {
		s.PrevToken, s.PrevUntil = "", time.Time{}
		return false
	}
	return subtle.ConstantTimeCompare([]byte(tok), []byte(s.PrevToken)) == 1
}
