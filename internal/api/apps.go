// Package api serves the application API (spec
// docs/superpowers/specs/2026-11-02-app-api-design.md): registered programs
// on this machine ask a running session for an answer over a unix socket.
// The package is a leaf — it knows nothing about agents — and talks to the
// session through the Backend interface in backend.go.
package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	keyPrefix      = "bek_1_"
	DefaultTokens  = 4096
	DefaultPerHour = 120
)

var (
	ErrExists   = errors.New("an app of that name is already registered")
	ErrNotFound = errors.New("no app of that name is registered")
	nameRe      = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	tiers       = map[string]bool{"query": true, "completions": true}
)

// Budget bounds what one app may ask for: Tokens is the most a reply may be
// asked to produce, PerHour the requests allowed in any sliding hour. Zero
// is never unbounded: Add and every read put the default in its place
// (DefaultTokens, DefaultPerHour; spec §2.1).
type Budget struct {
	Tokens  int `json:"tokens"`
	PerHour int `json:"per_hour"`
}

// Profile is what an app may do (spec §2.1).
type Profile struct {
	Tiers    []string `json:"tiers"`
	Models   []string `json:"models,omitempty"`
	Packs    []string `json:"packs,omitempty"`
	Sessions string   `json:"sessions"`
	Budget   Budget   `json:"budget"`
}

// App is one registered program.
type App struct {
	Name    string     `json:"name"`
	KeyHash string     `json:"key_hash"`
	Created time.Time  `json:"created"`
	Revoked *time.Time `json:"revoked,omitempty"`
	Profile Profile    `json:"profile"`
}

func (p Profile) AllowsTier(t string) bool {
	for _, x := range p.Tiers {
		if x == t {
			return true
		}
	}
	return false
}

// AllowsModel: "" and "default" are always the serving session's model.
func (p Profile) AllowsModel(m string) bool {
	if m == "" || m == "default" {
		return true
	}
	for _, x := range p.Models {
		if x == m {
			return true
		}
	}
	return false
}

func (p Profile) AllowsPack(name string) bool {
	for _, x := range p.Packs {
		if x == name {
			return true
		}
	}
	return false
}

// Validate refuses a profile the server could not honour.
func (p Profile) Validate() error {
	for _, t := range p.Tiers {
		if !tiers[t] {
			return fmt.Errorf("tier %q is not one of query, completions", t)
		}
	}
	if p.Sessions != "none" && p.Sessions != "named" {
		return fmt.Errorf("sessions %q is not one of none, named", p.Sessions)
	}
	if p.Budget.Tokens < 0 || p.Budget.PerHour < 0 {
		return errors.New("budget values must not be negative")
	}
	return nil
}

// withDefaults is p with the spec's defaults where nothing was given.
func (p Profile) withDefaults() Profile {
	if p.Sessions == "" {
		p.Sessions = "none"
	}
	if p.Budget.Tokens == 0 {
		p.Budget.Tokens = DefaultTokens
	}
	if p.Budget.PerHour == 0 {
		p.Budget.PerHour = DefaultPerHour
	}
	if p.Tiers == nil {
		p.Tiers = []string{}
	}
	return p
}

func ValidName(name string) bool { return nameRe.MatchString(name) }

// NewKey is a fresh app key: the format tag plus 24 random bytes, base64url
// unpadded. Printed once by `apps add`; only its hash is kept.
func NewKey() string {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("api: random source failed: " + err.Error())
	}
	return keyPrefix + base64.RawURLEncoding.EncodeToString(b[:])
}

func HashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// Registry is the app files under Dir. Lookup reads every file on each call,
// so a rotate or revoke from any process takes effect at once: no cache can
// outlive a write, and no timestamp decides whether a file changed.
type Registry struct {
	Dir  string
	Warn func(string) // one line per unreadable file, once; nil is silent

	mu     sync.Mutex
	warned map[string]bool
}

func NewRegistry(dir string) *Registry {
	return &Registry{Dir: dir, warned: map[string]bool{}}
}

func (r *Registry) path(name string) string { return filepath.Join(r.Dir, name+".json") }

func (r *Registry) write(a App) error {
	if err := os.MkdirAll(r.Dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return err
	}
	p := r.path(a.Name)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Add registers name with p (defaults filled, validated) and returns the one
// copy of its key that will ever exist.
func (r *Registry) Add(name string, p Profile) (string, error) {
	if !ValidName(name) {
		return "", fmt.Errorf("app name %q must match %s", name, nameRe)
	}
	p = p.withDefaults()
	if err := p.Validate(); err != nil {
		return "", err
	}
	if _, err := os.Stat(r.path(name)); err == nil {
		return "", fmt.Errorf("%w: %s", ErrExists, r.path(name))
	}
	key := NewKey()
	a := App{Name: name, KeyHash: HashKey(key), Created: time.Now(), Profile: p}
	if err := r.write(a); err != nil {
		return "", err
	}
	return key, nil
}

func (r *Registry) Get(name string) (App, error) {
	raw, err := os.ReadFile(r.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return App{}, ErrNotFound
	}
	if err != nil {
		return App{}, err
	}
	var a App
	if err := json.Unmarshal(raw, &a); err != nil {
		return App{}, fmt.Errorf("%s: %w", r.path(name), err)
	}
	return a, nil
}

func (r *Registry) List() ([]App, error) {
	entries, err := os.ReadDir(r.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []App
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		a, err := r.Get(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (r *Registry) Rotate(name string) (string, error) {
	a, err := r.Get(name)
	if err != nil {
		return "", err
	}
	key := NewKey()
	a.KeyHash = HashKey(key)
	a.Revoked = nil
	return key, r.write(a)
}

func (r *Registry) Revoke(name string) error {
	a, err := r.Get(name)
	if err != nil {
		return err
	}
	now := time.Now()
	a.Revoked = &now
	return r.write(a)
}

func (r *Registry) Remove(name string) error {
	err := os.Remove(r.path(name))
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

// Lookup finds the live app whose key this is: constant-time against every
// hash, never a partial match, never a revoked app.
func (r *Registry) Lookup(key string) (App, bool) {
	if !strings.HasPrefix(key, keyPrefix) {
		return App{}, false
	}
	want := []byte(HashKey(key))
	r.mu.Lock()
	defer r.mu.Unlock()
	entries, err := os.ReadDir(r.Dir)
	if err != nil {
		return App{}, false
	}
	var found App
	ok := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		a, err := r.readApp(e.Name())
		if err != nil {
			r.warnOnce(e.Name(), err)
			continue
		}
		// Every candidate is compared so timing does not say which exists.
		if subtle.ConstantTimeCompare([]byte(a.KeyHash), want) == 1 && a.Revoked == nil {
			found, ok = a, true
		}
	}
	return found, ok
}

// readApp parses one app file. A file that will not parse, or has no key
// hash, is an error: Lookup treats it as absent (fail closed).
func (r *Registry) readApp(file string) (App, error) {
	raw, err := os.ReadFile(filepath.Join(r.Dir, file))
	if err != nil {
		return App{}, err
	}
	var a App
	if err := json.Unmarshal(raw, &a); err != nil {
		return App{}, err
	}
	if a.KeyHash == "" {
		return App{}, errors.New("no key_hash")
	}
	// A hand-edited 0 means the default, as it does in Add: never unbounded.
	a.Profile = a.Profile.withDefaults()
	return a, nil
}

// warnOnce reports an unreadable file once per registry, then stays quiet.
func (r *Registry) warnOnce(file string, err error) {
	if r.warned[file] {
		return
	}
	r.warned[file] = true
	if r.Warn != nil {
		r.Warn(fmt.Sprintf("api: app file %s could not be read and is ignored: %v", filepath.Join(r.Dir, file), err))
	}
}
