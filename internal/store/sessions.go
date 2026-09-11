// Package store persists BE-Code conversations to ~/.be-code/sessions,
// enabling /resume and --resume — the BE-CLI persistence habit applied to
// agent conversations.
package store

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// Session is one saved conversation.
type Session struct {
	ID        string             `json:"id"`
	Code      string             `json:"code,omitempty"` // short resume code
	Title     string             `json:"title"`
	CreatedAt time.Time          `json:"created_at"`
	UpdatedAt time.Time          `json:"updated_at"`
	Provider  string             `json:"provider"`
	Model     string             `json:"model"`
	Workspace string             `json:"workspace"`
	Messages  []provider.Message `json:"messages"`
	// Handoff is a compact briefing written when the session ends: task,
	// user-stated requirements, decisions, files changed, outstanding work.
	// It is injected into the system prompt on resume.
	Handoff string `json:"handoff,omitempty"`
}

// Meta is the listing view of a session (messages not loaded).
type Meta struct {
	ID        string    `json:"id"`
	Code      string    `json:"code"`
	Title     string    `json:"title"`
	UpdatedAt time.Time `json:"updated_at"`
	Model     string    `json:"model"`
	Workspace string    `json:"workspace"`
	Turns     int       `json:"turns"`
}

func dir() (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(base, "sessions")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}

// NewSession creates an unsaved session with a time-ordered ID.
func NewSession(providerName, model, workspace string) *Session {
	now := time.Now()
	id := now.Format("20060102-150405") + fmt.Sprintf("-%03d", now.Nanosecond()/1e6)
	return &Session{
		ID:        id,
		Code:      CodeFor(id),
		CreatedAt: now,
		Provider:  providerName,
		Model:     model,
		Workspace: workspace,
	}
}

// codeAlphabet omits 0/O and 1/I so codes survive being read aloud or typed.
const codeAlphabet = "23456789ABCDEFGHJKLMNPQRSTUVWXYZ"

// CodeFor derives a stable 6-character resume code from a session ID.
func CodeFor(id string) string {
	sum := sha256.Sum256([]byte(id))
	var b [6]byte
	for i := range b {
		b[i] = codeAlphabet[int(sum[i])%len(codeAlphabet)]
	}
	return string(b[:])
}

// ResumeCode returns the session's resume code, deriving one for sessions
// saved before codes existed.
func (s *Session) ResumeCode() string {
	if s.Code != "" {
		return s.Code
	}
	return CodeFor(s.ID)
}

// TitleFrom derives a session title from the first user input.
func TitleFrom(input string) string {
	t := strings.Join(strings.Fields(input), " ")
	if len(t) > 60 {
		t = t[:60] + "..."
	}
	return t
}

// Save writes the session atomically.
func (s *Session) Save() error {
	d, err := dir()
	if err != nil {
		return err
	}
	s.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(s, "", " ")
	if err != nil {
		return err
	}
	p := filepath.Join(d, s.ID+".json")
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Load reads a full session by ID or resume code. "last" resolves to the
// most recent one.
func Load(id string) (*Session, error) {
	if id == "last" || id == "latest" {
		metas, err := List()
		if err != nil {
			return nil, err
		}
		if len(metas) == 0 {
			return nil, fmt.Errorf("no saved sessions")
		}
		id = metas[0].ID
	}
	d, err := dir()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(d, id+".json"))
	if err != nil {
		// Not an ID: try it as a resume code.
		if byCode, cerr := loadByCode(id); cerr == nil {
			return byCode, nil
		}
		return nil, fmt.Errorf("session %s: %w", id, err)
	}
	var s Session
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("session %s: %w", id, err)
	}
	return &s, nil
}

func loadByCode(code string) (*Session, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) != 6 {
		return nil, fmt.Errorf("not a resume code")
	}
	metas, err := List()
	if err != nil {
		return nil, err
	}
	for _, m := range metas {
		if m.Code == code {
			return Load(m.ID)
		}
	}
	return nil, fmt.Errorf("no session with code %s", code)
}

// List returns session metadata, newest first.
func List() ([]Meta, error) {
	d, err := dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(d)
	if err != nil {
		return nil, err
	}
	var metas []Meta
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(d, e.Name()))
		if err != nil {
			continue
		}
		var s Session
		if json.Unmarshal(data, &s) != nil {
			continue
		}
		turns := 0
		for _, m := range s.Messages {
			if m.Role == provider.RoleUser && !strings.HasPrefix(m.Content, "<tool_result") {
				turns++
			}
		}
		metas = append(metas, Meta{
			ID: s.ID, Code: s.ResumeCode(), Title: s.Title, UpdatedAt: s.UpdatedAt,
			Model: s.Model, Workspace: s.Workspace, Turns: turns,
		})
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].UpdatedAt.After(metas[j].UpdatedAt) })
	return metas, nil
}

// Delete removes a saved session.
func Delete(id string) error {
	d, err := dir()
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(d, id+".json"))
}
