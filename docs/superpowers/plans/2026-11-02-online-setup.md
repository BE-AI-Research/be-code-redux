# Online Provider Setup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A person adds an online provider from inside the harness (TUI, plain mode, `be-code setup`, `be-code online add`) by env var, pasted key, or OpenRouter's browser sign-in, with the key kept in a 0600 credential file that `config.json` never holds.

**Architecture:** Two new leaf packages, `internal/credentials` (the file) and `internal/signin` (OpenRouter PKCE against injectable endpoints), one implementation of "add a provider" in `internal/setup` (`Validate`, `AddOnline`) that every UI calls with its own prompts, and key resolution moved to `config.Config.ProviderKey(name)` (env var, then stored). The TUI flow is assembled from the existing picker, a masked `textinput` for the key step, the shared ask modal, and the `/provider` switch.

**Tech Stack:** Go 1.25, stdlib (`crypto/rand`, `crypto/sha256`, `net/http`, `encoding/json`), cobra, Bubble Tea with `bubbles/textinput`, `charmbracelet/x/term` (already in the module graph, for echo-off reading), `internal/procattr` for the browser launch.

**Spec:** `docs/superpowers/specs/2026-11-02-online-setup-design.md`

## Global Constraints

- Branch `feature/online-setup` off `main`. Version stays 1.3.1 (`build.mk`), unreleased.
- Commit with `--no-gpg-sign`. Never `git add -A` or `git add .`: the checkout holds untracked `k.pgp` and `x.txt` that must never be committed. Add files by name.
- Never touch the real `~/.be-code`: every test sets `HOME` to a temp dir (`t.Setenv("HOME", t.TempDir())`) before anything that reads `config.Dir()`.
- A key, code or verifier never appears in a log line, notice, error, transcript entry, the input history file, `run --json`, `doctor` output or a test failure message.
- `credentials.json` is written at mode 0600 by atomic rename; read and write are refused when `mode&0o077 != 0`.
- Resolution order is fixed: the env var named by `api_key_env` when set and non-empty, then the stored credential, then `""`.
- `config.json` never holds a secret; `ProviderConfig` gains no key field.
- No `--key` flag on any command.
- `internal/credentials` and `internal/signin` import nothing from `internal/` except `signin` → `internal/procattr`. `config` may import `credentials`; `credentials` never imports `config`.
- `make -f build.mk verify` green before any task is reported complete (`go vet`, build, `go test ./...`).
- Exact user-visible strings are in the spec's §7 table and in each task below; use them verbatim.

## Review Focus

Inputs the spec implies but no single task's happy path exercises; each has a pinning test in the task that owns the code:

1. A pasted key with surrounding whitespace or a trailing newline (a copy from a web page) must be trimmed before validation and storage, never stored with the newline. (Task 3, `TestValidateTrimsKey`.)
2. A pasted redirect URL whose `code` is URL-encoded, or a bare code with whitespace, must decode to the same code the callback would carry. (Task 4, `TestPastedURLDecodesCode`.)
3. A credential file whose directory does not exist yet (fresh install, first `Put`) must be created with the dotdir at 0700 and the file at 0600, not fail. (Task 1, `TestPutCreatesDotdir`.)
4. `/online add openrouter` while `$OPENROUTER_API_KEY` is set in the environment: the stored key is written but the env var still wins, and the flow must say so (`note: $OPENROUTER_API_KEY is set and takes precedence`), or a person will "fix" a key and see no change. (Task 3, `TestAddOnlineNotesEnvPrecedence`; surfaced in Task 6 and Task 7.)
5. The models listing of a provider that returns 200 with an HTML body (a login page behind a proxy) must classify as unreachable, not as an empty listing that silently accepts a typed model. (Task 3, `TestValidateHTMLIsUnreachable`.)

---

### Task 1: The credential store (`internal/credentials`)

**Files:**
- Create: `internal/credentials/credentials.go`
- Create: `internal/credentials/credentials_test.go`

**Interfaces:**
- Consumes: nothing from the repo.
- Produces:
  ```go
  package credentials
  type Credential struct {
      Kind    string    `json:"kind"`              // "api_key"
      Key     string    `json:"key,omitempty"`
      Source  string    `json:"source,omitempty"`  // "pasted" | "oauth"
      Created time.Time `json:"created"`
  }
  const KindAPIKey = "api_key"
  type Store struct { /* unexported: path string */ }
  var ErrLooseMode = errors.New("credentials.json is readable by others")
  func Open(dir string) (*Store, error)
  func (s *Store) Key(provider string) (Credential, bool)
  func (s *Store) Put(provider string, c Credential) error
  func (s *Store) Revoke(provider string) error
  func (s *Store) Path() string
  func (s *Store) Unsupported() []string // provider names whose kind is not "api_key"
  ```
  `Open` returns `ErrLooseMode` wrapped with the path and the chmod text when the mode is loose; a corrupt file opens as empty but `Put`/`Revoke` then return an error naming the file.

- [ ] **Step 1: Write the failing tests**

```go
// internal/credentials/credentials_test.go
package credentials

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRoundTripAnd0600(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Key("groq"); ok {
		t.Fatal("empty store has a key")
	}
	c := Credential{Kind: KindAPIKey, Key: "gsk_test", Source: "pasted", Created: time.Date(2026, 11, 2, 20, 0, 0, 0, time.UTC)}
	if err := s.Put("groq", c); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o, want 0600", fi.Mode().Perm())
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s2.Key("groq")
	if !ok || got != c {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
}

func TestPutCreatesDotdir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dot")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("groq", Credential{Kind: KindAPIKey, Key: "k", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("dotdir: %v mode %v", err, fi.Mode())
	}
}

func TestLooseModeRefusedBothWays(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(p, []byte(`{"version":1,"providers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := Open(dir)
	if !errors.Is(err, ErrLooseMode) || !strings.Contains(err.Error(), "chmod 600 "+p) {
		t.Fatalf("open: %v", err)
	}
}

func TestCorruptReadsEmptyWriteRefused(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "credentials.json")
	if err := os.WriteFile(p, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Key("x"); ok {
		t.Fatal("corrupt file yielded a key")
	}
	err = s.Put("x", Credential{Kind: KindAPIKey, Key: "k", Created: time.Now()})
	if err == nil || !strings.Contains(err.Error(), p) {
		t.Fatalf("put on corrupt file: %v", err)
	}
	if b, _ := os.ReadFile(p); string(b) != `{not json` {
		t.Fatal("corrupt file was overwritten")
	}
}

func TestUnknownKindKeptAndReported(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "credentials.json")
	raw := `{"version":1,"providers":{"future":{"kind":"oauth_token","access":"a","refresh":"r"},"groq":{"kind":"api_key","key":"k","created":"2026-11-02T20:00:00Z"}}}`
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Unsupported(); len(got) != 1 || got[0] != "future" {
		t.Fatalf("unsupported: %v", got)
	}
	if _, ok := s.Key("future"); ok {
		t.Fatal("an unknown kind must not yield a key")
	}
	if err := s.Revoke("groq"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	provs := m["providers"].(map[string]any)
	if _, ok := provs["groq"]; ok {
		t.Fatal("groq not revoked")
	}
	f := provs["future"].(map[string]any)
	if f["refresh"] != "r" {
		t.Fatalf("unknown kind not kept verbatim: %v", f)
	}
}

func TestRevokeMissingIsNoop(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Revoke("nobody"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Path()); !os.IsNotExist(err) {
		t.Fatal("revoke of a missing entry created the file")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/credentials/ 2>&1 | head -5`
Expected: build failure, `undefined: Open` (package has no non-test file yet; create an empty `credentials.go` with `package credentials` first if the toolchain refuses a test-only package).

- [ ] **Step 3: Implement the store**

```go
// internal/credentials/credentials.go
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
	if fi.Mode().Perm()&0o077 != 0 {
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/credentials/ -v 2>&1 | tail -12`
Expected: all six PASS. `TestPutCreatesDotdir` asserts the directory mode is 0700: `os.MkdirAll` applies the umask, so if it fails on a machine with a loose umask, add an explicit `os.Chmod(dir, 0o700)` after `MkdirAll` in `mutate`.

- [ ] **Step 5: Commit**

```bash
git add internal/credentials/credentials.go internal/credentials/credentials_test.go
git commit --no-gpg-sign -m "feat(credentials): the 0600 credential file"
```

---

### Task 2: Key resolution through the config (`Config.ProviderKey`)

**Files:**
- Modify: `internal/config/config.go` (after `func (pc ProviderConfig) APIKey()`, line 784)
- Modify: `internal/provider/registry.go:20-22` (`FromConfig`)
- Modify: `internal/setup/probe.go:50-52` (`Probe`)
- Test: `internal/config/credentials_test.go` (new), `internal/provider/registry_test.go` (new test function appended, or new file)

**Interfaces:**
- Consumes: `credentials.Open(dir) (*Store, error)`, `(*Store).Key(name) (Credential, bool)` from Task 1.
- Produces:
  ```go
  // ProviderKey resolves the key for the provider named name: the env var
  // named by its api_key_env when set and non-empty, else the stored
  // credential in ~/.be-code/credentials.json, else "".
  func (c *Config) ProviderKey(name string) string
  // KeySource says where ProviderKey's answer came from, for /online and
  // doctor, never the key: "env" | "stored" | "".
  func (c *Config) KeySource(name string) (source string, stored credentials.Credential)
  ```
  `FromConfig` and `setup.Probe` build providers with `cfg.ProviderKey(name)`; `ProviderConfig.APIKey()` stays env-only and keeps its callers in tests.

- [ ] **Step 1: Write the failing tests**

```go
// internal/config/credentials_test.go
package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/credentials"
)

func storeKey(t *testing.T, home, provider, key string) {
	t.Helper()
	s, err := credentials.Open(filepath.Join(home, ".be-code"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put(provider, credentials.Credential{Kind: credentials.KindAPIKey, Key: key, Source: "pasted", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
}

func TestProviderKeyPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := Default()
	cfg.Providers["groq"] = ProviderConfig{Type: "openai", BaseURL: "https://api.groq.com/openai/v1", APIKeyEnv: "BE_TEST_GROQ_KEY", Online: true}
	t.Setenv("BE_TEST_GROQ_KEY", "")
	if got := cfg.ProviderKey("groq"); got != "" {
		t.Fatalf("nothing set: %q", got)
	}
	storeKey(t, home, "groq", "stored-key")
	if got := cfg.ProviderKey("groq"); got != "stored-key" {
		t.Fatalf("stored: %q", got)
	}
	if src, _ := cfg.KeySource("groq"); src != "stored" {
		t.Fatalf("source %q", src)
	}
	t.Setenv("BE_TEST_GROQ_KEY", "env-key")
	if got := cfg.ProviderKey("groq"); got != "env-key" {
		t.Fatalf("env must win: %q", got)
	}
	if src, _ := cfg.KeySource("groq"); src != "env" {
		t.Fatalf("source %q", src)
	}
	if got := cfg.ProviderKey("nope"); got != "" {
		t.Fatalf("unknown provider: %q", got)
	}
}

func TestProviderKeyLooseModeIsEmptyNotFatal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".be-code")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), []byte(`{"version":1,"providers":{"groq":{"kind":"api_key","key":"k","created":"2026-11-02T00:00:00Z"}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	cfg.Providers["groq"] = ProviderConfig{Type: "openai", BaseURL: "https://x/v1", APIKeyEnv: "BE_TEST_GROQ_KEY"}
	t.Setenv("BE_TEST_GROQ_KEY", "")
	if got := cfg.ProviderKey("groq"); got != "" {
		t.Fatalf("a loose file must yield nothing: %q", got)
	}
}
```

```go
// appended to internal/provider/registry_test.go (create the file with
// `package provider` and the imports if it does not exist)
func TestFromConfigUsesStoredKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s, err := credentials.Open(filepath.Join(home, ".be-code"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Put("groq", credentials.Credential{Kind: credentials.KindAPIKey, Key: "stored-key", Created: time.Now()}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Providers["groq"] = config.ProviderConfig{Type: "openai", BaseURL: "https://x/v1", APIKeyEnv: "BE_TEST_GROQ_KEY"}
	t.Setenv("BE_TEST_GROQ_KEY", "")
	p, err := FromConfig(cfg, "groq")
	if err != nil {
		t.Fatal(err)
	}
	if got := p.(*OpenAICompat).APIKey; got != "stored-key" {
		t.Fatalf("provider key %q", got)
	}
}
```
(Check the field name on `OpenAICompat` that holds the key — `grep -n "APIKey" internal/provider/openai.go` — and use it.)

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ ./internal/provider/ 2>&1 | grep -E "undefined|FAIL" | head`
Expected: `undefined: (*Config).ProviderKey`, `KeySource`.

- [ ] **Step 3: Implement**

In `internal/config/config.go`, add the import `"github.com/brown-enterprises/be-code/internal/credentials"` and, after `APIKey()`:

```go
// ProviderKey resolves the key for the provider named name: the env var
// named by its api_key_env when set and non-empty, else the credential
// stored for name in ~/.be-code/credentials.json, else "". A credential
// file that cannot be read (loose mode, unreadable) counts as absent:
// the key is simply missing, and doctor says why.
func (c *Config) ProviderKey(name string) string {
	_, stored := c.KeySource(name)
	if pc, ok := c.Providers[name]; ok && pc.APIKeyEnv != "" {
		if v := os.Getenv(pc.APIKeyEnv); v != "" {
			return v
		}
	}
	return stored.Key
}

// KeySource says where ProviderKey's answer comes from — "env", "stored"
// or "" — and returns the stored credential's metadata for display. It
// never returns the env var's value.
func (c *Config) KeySource(name string) (string, credentials.Credential) {
	pc, ok := c.Providers[name]
	if !ok {
		return "", credentials.Credential{}
	}
	var stored credentials.Credential
	if dir, err := Dir(); err == nil {
		if s, err := credentials.Open(dir); err == nil {
			stored, _ = s.Key(name)
		}
	}
	if pc.APIKeyEnv != "" && os.Getenv(pc.APIKeyEnv) != "" {
		return "env", stored
	}
	if stored.Key != "" {
		return "stored", stored
	}
	return "", stored
}
```

In `internal/provider/registry.go`, lines 20 and 22: replace `pc.APIKey()` with `cfg.ProviderKey(name)`.

In `internal/setup/probe.go`, lines 50 and 52: `Probe` iterates candidates built from `DefaultCandidates()`, which are local and have no key; keep `c.Cfg.APIKey()` there (there is no `*config.Config` in scope and local candidates never store a key). Leave it unchanged.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config/ ./internal/provider/ 2>&1 | tail -3`
Expected: both `ok`.

- [ ] **Step 5: Run the whole suite for regressions and commit**

Run: `go vet ./... && go test ./... 2>&1 | grep -v "^ok" | grep -v "no test files"`
Expected: no output (only `ok` lines).

```bash
git add internal/config/config.go internal/config/credentials_test.go internal/provider/registry.go internal/provider/registry_test.go
git commit --no-gpg-sign -m "feat(config): ProviderKey resolves the env var, then the stored credential"
```

---

### Task 3: `setup.Validate` and `setup.AddOnline`

**Files:**
- Modify: `internal/provider/openai.go:319-321` (`ListModels` returns a typed `*HTTPError` on a non-200)
- Create: `internal/setup/online.go`
- Create: `internal/setup/online_test.go`

**Interfaces:**
- Consumes: `credentials.Store` (Task 1); `cfg.ProviderKey`/`KeySource` (Task 2); `provider.Presets()`, `provider.PresetByName`, `(Preset).ProviderConfig()`, `provider.NewOpenAICompat`, `provider.HTTPError`, `config.LocalEndpoint`, `cfg.Save()`.
- Produces:
  ```go
  package setup
  type Choice struct {
      Name    string
      BaseURL string
      KeyEnv  string
      Preset  bool
  }
  // ChoiceFor resolves a name to a Choice: a preset by name (URL from the
  // preset even if cfg holds another; Noted says so), or a custom entry
  // with baseURL. Errors: custom name equal to a preset; empty custom URL.
  func ChoiceFor(cfg *config.Config, name, baseURL string) (Choice, string /*note*/, error)
  var ErrKeyRejected = errors.New("key rejected")
  var ErrUnreachable = errors.New("endpoint unreachable or not OpenAI-compatible")
  // Validate lists models with key (trimmed). A wrapped ErrKeyRejected on
  // 401/403; a wrapped ErrUnreachable on transport, non-JSON or other
  // non-200; an empty slice on a listing the server refuses with 404.
  func Validate(ctx context.Context, ch Choice, key string) ([]provider.ModelInfo, error)
  type Added struct {
      Model   string
      Default bool
      Note    string // "note: $X is set and takes precedence", or ""
  }
  // AddOnline writes the credential (source "pasted"|"oauth"), then the
  // provider block, then default_provider when makeDefault.
  func AddOnline(cfg *config.Config, store *credentials.Store, ch Choice, key, source, model string, makeDefault bool) (Added, error)
  ```

- [ ] **Step 1: Make `ListModels` return a typed status error**

In `internal/provider/openai.go` replace lines 319–321:

```go
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, &HTTPError{Provider: p.ProviderName, Code: resp.StatusCode, Body: "listing models: " + strings.TrimSpace(string(b)), RetryAfter: ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
	}
```
(`io` is already imported there; check and add if not.) `Error()` renders `name: HTTP 401: listing models: …`, which the agent's string classifier still reads as before.

Run: `go test ./internal/provider/ ./internal/agent/ 2>&1 | tail -2` — Expected: both `ok`.

- [ ] **Step 2: Write the failing tests**

```go
// internal/setup/online_test.go
package setup

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/credentials"
)

// listingServer answers /models: 401 unless the bearer is "good"; with
// html set, 200 with an HTML body; with refuse set, 404.
func listingServer(t *testing.T, html, refuse bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer good" {
			w.WriteHeader(401)
			return
		}
		if refuse {
			w.WriteHeader(404)
			return
		}
		if html {
			w.Header().Set("Content-Type", "text/html")
			w.Write([]byte("<html>login</html>"))
			return
		}
		w.Write([]byte(`{"data":[{"id":"m1"},{"id":"m2"}]}`))
	}))
}

func customChoice(t *testing.T, url string) Choice {
	t.Helper()
	ch, _, err := ChoiceFor(config.Default(), "mine", url+"/v1")
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

func TestValidateOutcomes(t *testing.T) {
	srv := listingServer(t, false, false)
	defer srv.Close()
	ch := customChoice(t, srv.URL)
	if _, err := Validate(context.Background(), ch, "bad"); !errors.Is(err, ErrKeyRejected) {
		t.Fatalf("bad key: %v", err)
	}
	models, err := Validate(context.Background(), ch, "good")
	if err != nil || len(models) != 2 {
		t.Fatalf("good key: %v %v", models, err)
	}
	down := customChoice(t, "http://127.0.0.1:1")
	if _, err := Validate(context.Background(), down, "good"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("down: %v", err)
	}
}

func TestValidateHTMLIsUnreachable(t *testing.T) {
	srv := listingServer(t, true, false)
	defer srv.Close()
	_, err := Validate(context.Background(), customChoice(t, srv.URL), "good")
	if !errors.Is(err, ErrUnreachable) {
		t.Fatalf("html body: %v", err)
	}
}

func TestValidateRefusedListingIsEmptyNotError(t *testing.T) {
	srv := listingServer(t, false, true)
	defer srv.Close()
	models, err := Validate(context.Background(), customChoice(t, srv.URL), "good")
	if err != nil || len(models) != 0 {
		t.Fatalf("404 listing: %v %v", models, err)
	}
}

func TestValidateTrimsKey(t *testing.T) {
	srv := listingServer(t, false, false)
	defer srv.Close()
	if _, err := Validate(context.Background(), customChoice(t, srv.URL), "  good\n"); err != nil {
		t.Fatalf("trimmed key: %v", err)
	}
}

func TestChoiceForRules(t *testing.T) {
	cfg := config.Default()
	cfg.Providers["openrouter"] = config.ProviderConfig{Type: "openai", BaseURL: "https://elsewhere/v1", APIKeyEnv: "OPENROUTER_API_KEY"}
	ch, note, err := ChoiceFor(cfg, "openrouter", "")
	if err != nil || !ch.Preset || ch.BaseURL != "https://openrouter.ai/api/v1" || !strings.Contains(note, "openrouter is a preset; its URL is https://openrouter.ai/api/v1") {
		t.Fatalf("preset: %+v note=%q err=%v", ch, note, err)
	}
	if _, _, err := ChoiceFor(cfg, "groq", "https://custom/v1"); err == nil {
		t.Fatal("a custom entry named like a preset must be refused")
	}
	if _, _, err := ChoiceFor(cfg, "mine", ""); err == nil {
		t.Fatal("a custom entry needs a URL")
	}
	ch, _, err = ChoiceFor(cfg, "mine", "http://10.0.0.5:8000/v1")
	if err != nil || ch.Preset || ch.KeyEnv != "" {
		t.Fatalf("custom: %+v %v", ch, err)
	}
}

func TestAddOnlineWritesCredentialThenConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.Default()
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	store, err := credentials.Open(filepath.Join(home, ".be-code"))
	if err != nil {
		t.Fatal(err)
	}
	ch, _, _ := ChoiceFor(cfg, "groq", "")
	t.Setenv("GROQ_API_KEY", "")
	added, err := AddOnline(cfg, store, ch, "gsk_1\n", "pasted", "llama", false)
	if err != nil || added.Model != "llama" || added.Default || added.Note != "" {
		t.Fatalf("%+v %v", added, err)
	}
	reloaded, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	pc := reloaded.Providers["groq"]
	if pc.BaseURL != "https://api.groq.com/openai/v1" || pc.APIKeyEnv != "GROQ_API_KEY" || !pc.Online || pc.DefaultModel != "llama" || pc.Type != "openai" {
		t.Fatalf("block: %+v", pc)
	}
	if reloaded.DefaultProvider == "groq" {
		t.Fatal("default_provider changed without being asked")
	}
	b, _ := os.ReadFile(filepath.Join(home, ".be-code", "config.json"))
	if strings.Contains(string(b), "gsk_1") {
		t.Fatal("config.json holds the key")
	}
	if got := reloaded.ProviderKey("groq"); got != "gsk_1" {
		t.Fatalf("stored key %q (must be trimmed)", got)
	}
	// Replace the key, make it the default.
	added, err = AddOnline(cfg, store, ch, "gsk_2", "oauth", "llama2", true)
	if err != nil || !added.Default {
		t.Fatalf("%+v %v", added, err)
	}
	reloaded, _ = config.Load()
	if reloaded.DefaultProvider != "groq" || reloaded.ProviderKey("groq") != "gsk_2" || reloaded.Providers["groq"].DefaultModel != "llama2" {
		t.Fatalf("replace: %+v key=%q", reloaded.Providers["groq"], reloaded.ProviderKey("groq"))
	}
	c, _ := store.Key("groq")
	if c.Source != "oauth" {
		t.Fatalf("source %q", c.Source)
	}
}

func TestAddOnlineNotesEnvPrecedence(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.Default()
	store, _ := credentials.Open(filepath.Join(home, ".be-code"))
	ch, _, _ := ChoiceFor(cfg, "groq", "")
	t.Setenv("GROQ_API_KEY", "from-env")
	added, err := AddOnline(cfg, store, ch, "stored", "pasted", "m", false)
	if err != nil || added.Note != "note: $GROQ_API_KEY is set and takes precedence" {
		t.Fatalf("%+v %v", added, err)
	}
}

func TestAddOnlineCustomOnlineByEndpoint(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.Default()
	store, _ := credentials.Open(filepath.Join(home, ".be-code"))
	local, _, _ := ChoiceFor(cfg, "lan", "http://192.168.1.50:8000/v1")
	if _, err := AddOnline(cfg, store, local, "k", "pasted", "m", false); err != nil {
		t.Fatal(err)
	}
	remote, _, _ := ChoiceFor(cfg, "far", "https://api.together.xyz/v1")
	if _, err := AddOnline(cfg, store, remote, "k", "pasted", "m", false); err != nil {
		t.Fatal(err)
	}
	re, _ := config.Load()
	if re.Providers["lan"].Online || !re.Providers["far"].Online || re.Providers["lan"].APIKeyEnv != "" {
		t.Fatalf("lan=%+v far=%+v", re.Providers["lan"], re.Providers["far"])
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/setup/ 2>&1 | grep -E "undefined" | head -5`
Expected: `undefined: ChoiceFor`, `Validate`, `AddOnline`, `ErrKeyRejected`.

- [ ] **Step 4: Implement**

```go
// internal/setup/online.go
package setup

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/credentials"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// Choice is the provider a person is adding: a preset (name, URL and env
// var from provider.Presets) or a custom OpenAI-compatible endpoint.
type Choice struct {
	Name    string
	BaseURL string
	KeyEnv  string
	Preset  bool
}

// ErrKeyRejected (401/403) sends a UI back to the key step; ErrUnreachable
// (transport, non-JSON, any other non-200) offers retry or cancel.
var (
	ErrKeyRejected = errors.New("key rejected")
	ErrUnreachable = errors.New("endpoint unreachable or not OpenAI-compatible")
)

// ValidateTimeout bounds the one listing Validate makes.
const ValidateTimeout = 15 * time.Second

// ChoiceFor resolves what a person named. A preset's name is a promise
// about where it points: the preset's URL is used even when cfg's block
// of that name was hand-edited to another, and the note says so. A custom
// name must not be a preset's, and needs a URL.
func ChoiceFor(cfg *config.Config, name, baseURL string) (Choice, string, error) {
	name = strings.TrimSpace(name)
	baseURL = strings.TrimSpace(baseURL)
	if name == "" {
		return Choice{}, "", errors.New("a provider needs a name")
	}
	if pre, ok := provider.PresetByName(name); ok {
		if baseURL != "" && baseURL != pre.BaseURL {
			return Choice{}, "", fmt.Errorf("%s is a preset; a custom endpoint needs another name", name)
		}
		note := ""
		if pc, ok := cfg.Providers[name]; ok && pc.BaseURL != "" && pc.BaseURL != pre.BaseURL {
			note = fmt.Sprintf("%s is a preset; its URL is %s", name, pre.BaseURL)
		}
		return Choice{Name: name, BaseURL: pre.BaseURL, KeyEnv: pre.KeyEnv, Preset: true}, note, nil
	}
	if baseURL == "" {
		return Choice{}, "", fmt.Errorf("custom provider %s needs a base URL", name)
	}
	u, err := url.Parse(baseURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Choice{}, "", fmt.Errorf("base URL %q is not an http(s) URL", baseURL)
	}
	return Choice{Name: name, BaseURL: strings.TrimRight(baseURL, "/")}, "", nil
}

// Validate lists models at the endpoint with key. The key is trimmed (a
// copy from a web page carries a newline).
func Validate(ctx context.Context, ch Choice, key string) ([]provider.ModelInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, ValidateTimeout)
	defer cancel()
	p := provider.NewOpenAICompat(ch.Name, ch.BaseURL, strings.TrimSpace(key))
	models, err := p.ListModels(ctx)
	if err == nil {
		return models, nil
	}
	var he *provider.HTTPError
	if errors.As(err, &he) {
		switch he.Code {
		case 401, 403:
			return nil, fmt.Errorf("%w by %s", ErrKeyRejected, ch.Name)
		case 404:
			return nil, nil // no listing route: a typed model name instead
		}
		return nil, fmt.Errorf("%w: %s answered HTTP %d", ErrUnreachable, ch.BaseURL, he.Code)
	}
	var ne net.Error
	if errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) {
		return nil, fmt.Errorf("%w: %s: %v", ErrUnreachable, ch.BaseURL, err)
	}
	// A 200 whose body is not the listing's JSON (an HTML login page).
	return nil, fmt.Errorf("%w: %s did not answer with a models listing", ErrUnreachable, ch.BaseURL)
}

// Added is what AddOnline did.
type Added struct {
	Model   string
	Default bool
	Note    string
}

// AddOnline stores the credential, then writes the provider block, then
// the default. Nothing here validates: callers run Validate first, so a
// cancel before this point leaves both files untouched.
func AddOnline(cfg *config.Config, store *credentials.Store, ch Choice, key, source, model string, makeDefault bool) (Added, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return Added{}, errors.New("no key to store")
	}
	if err := store.Put(ch.Name, credentials.Credential{Kind: credentials.KindAPIKey, Key: key, Source: source, Created: time.Now().UTC()}); err != nil {
		return Added{}, err
	}
	var pc config.ProviderConfig
	if ch.Preset {
		pre, _ := provider.PresetByName(ch.Name)
		pc = pre.ProviderConfig()
	} else {
		pc = config.ProviderConfig{Type: "openai", BaseURL: ch.BaseURL, Online: !config.LocalEndpoint(ch.BaseURL)}
	}
	if old, ok := cfg.Providers[ch.Name]; ok {
		// Keep a person's tuning; only identity fields are ours.
		pc.ContextWindow, pc.KeepAlive, pc.Options = old.ContextWindow, old.KeepAlive, old.Options
	}
	pc.DefaultModel = strings.TrimSpace(model)
	if cfg.Providers == nil {
		cfg.Providers = map[string]config.ProviderConfig{}
	}
	cfg.Providers[ch.Name] = pc
	if makeDefault {
		cfg.DefaultProvider = ch.Name
	}
	if err := cfg.Save(); err != nil {
		return Added{}, err
	}
	added := Added{Model: pc.DefaultModel, Default: makeDefault}
	if src, _ := cfg.KeySource(ch.Name); src == "env" {
		added.Note = fmt.Sprintf("note: $%s is set and takes precedence", pc.APIKeyEnv)
	}
	return added, nil
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/setup/ -v -run 'TestValidate|TestChoiceFor|TestAddOnline' 2>&1 | grep -E "^(--- |ok|FAIL)"`
Expected: all PASS. If `TestValidateOutcomes`'s "down" case is not a `net.Error`, it is wrapped by `url.Error`, which implements `net.Error`; `errors.As` finds it. If `TestValidateHTMLIsUnreachable` fails, the JSON decoder's error is a `*json.SyntaxError`, which the final return already covers.

- [ ] **Step 6: Commit**

```bash
git add internal/provider/openai.go internal/setup/online.go internal/setup/online_test.go
git commit --no-gpg-sign -m "feat(setup): Validate and AddOnline, one implementation of adding a provider"
```

---

### Task 4: The OpenRouter sign-in (`internal/signin`)

**Files:**
- Create: `internal/signin/signin.go`
- Create: `internal/signin/browser.go`
- Create: `internal/signin/signin_test.go`

**Interfaces:**
- Consumes: `procattr.Hide(cmd *exec.Cmd)`.
- Produces:
  ```go
  package signin
  type Options struct {
      AuthorizeURL string              // default "https://openrouter.ai/auth"
      ExchangeURL  string              // default "https://openrouter.ai/api/v1/auth/keys"
      Open         func(url string) error // default OpenBrowser; nil means never launch
      Show         func(url string)    // called with the URL when Open is nil or fails, and always on Linux without a display
      Pasted       <-chan string       // what the person typed: full redirect URL or bare code
      Timeout      time.Duration       // default 5 * time.Minute
      Client       *http.Client        // default http.DefaultClient with 15 s timeout for the exchange
  }
  var ErrCancelled = errors.New("sign-in cancelled; nothing was stored")
  func SignIn(ctx context.Context, o Options) (key string, err error)
  func CodeFromPaste(s string) string // exported for the UIs' tests
  func OpenBrowser(url string) error  // xdg-open | open | rundll32, via procattr.Hide
  ```

- [ ] **Step 1: Write the failing tests**

```go
// internal/signin/signin_test.go
package signin

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// fakeOpenRouter records the authorize request and answers the exchange
// only when the verifier matches the challenge it saw.
type fakeOpenRouter struct {
	srv       *httptest.Server
	challenge string
	callback  string
	refuse    string // when set, the exchange answers 400 with this error.message ("" for a bodyless 500)
	refuseRaw bool
}

func newFake(t *testing.T) *fakeOpenRouter {
	t.Helper()
	f := &fakeOpenRouter{}
	mux := http.NewServeMux()
	mux.HandleFunc("/auth", func(w http.ResponseWriter, r *http.Request) {
		f.challenge = r.URL.Query().Get("code_challenge")
		f.callback = r.URL.Query().Get("callback_url")
		if r.URL.Query().Get("code_challenge_method") != "S256" {
			t.Errorf("method %q", r.URL.Query().Get("code_challenge_method"))
		}
	})
	mux.HandleFunc("/api/v1/auth/keys", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Code, CodeVerifier, Method string }
		var raw map[string]string
		json.NewDecoder(r.Body).Decode(&raw)
		body.Code, body.CodeVerifier, body.Method = raw["code"], raw["code_verifier"], raw["code_challenge_method"]
		if f.refuseRaw {
			w.WriteHeader(500)
			return
		}
		if f.refuse != "" {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": f.refuse}})
			return
		}
		sum := sha256.Sum256([]byte(body.CodeVerifier))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge || body.Code != "the-code" || body.Method != "S256" {
			w.WriteHeader(403)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"key": "sk-or-test"})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOpenRouter) options(t *testing.T, pasted <-chan string) Options {
	t.Helper()
	return Options{
		AuthorizeURL: f.srv.URL + "/auth",
		ExchangeURL:  f.srv.URL + "/api/v1/auth/keys",
		Open: func(u string) error {
			// Stand in for the browser: hit /auth so the fake sees the challenge, then redirect to the callback.
			resp, err := http.Get(u)
			if err != nil {
				return err
			}
			resp.Body.Close()
			go func() {
				time.Sleep(20 * time.Millisecond)
				r, err := http.Get(f.callback + "?code=the-code")
				if err == nil {
					r.Body.Close()
				}
			}()
			return nil
		},
		Show:    func(string) {},
		Pasted:  pasted,
		Timeout: 5 * time.Second,
	}
}

func TestBrowserPath(t *testing.T) {
	f := newFake(t)
	key, err := SignIn(context.Background(), f.options(t, nil))
	if err != nil || key != "sk-or-test" {
		t.Fatalf("%q %v", key, err)
	}
	if !strings.HasPrefix(f.callback, "http://127.0.0.1:") || !strings.Contains(f.callback, "/cb/") {
		t.Fatalf("callback %q", f.callback)
	}
}

func TestPastedURLDecodesCode(t *testing.T) {
	f := newFake(t)
	pasted := make(chan string, 1)
	o := f.options(t, pasted)
	o.Open = func(u string) error {
		resp, err := http.Get(u) // the fake must still see the challenge
		if err == nil {
			resp.Body.Close()
		}
		pasted <- "  " + f.callback + "?code=" + url.QueryEscape("the-code") + "&x=1\n"
		return nil
	}
	key, err := SignIn(context.Background(), o)
	if err != nil || key != "sk-or-test" {
		t.Fatalf("%q %v", key, err)
	}
}

func TestPastedBareCode(t *testing.T) {
	f := newFake(t)
	pasted := make(chan string, 1)
	o := f.options(t, pasted)
	o.Open = func(u string) error {
		resp, err := http.Get(u)
		if err == nil {
			resp.Body.Close()
		}
		pasted <- " the-code \n"
		return nil
	}
	if key, err := SignIn(context.Background(), o); err != nil || key != "sk-or-test" {
		t.Fatalf("%q %v", key, err)
	}
}

func TestWrongPathCallbackIgnored(t *testing.T) {
	f := newFake(t)
	o := f.options(t, nil)
	o.Timeout = 300 * time.Millisecond
	o.Open = func(u string) error {
		resp, err := http.Get(u)
		if err == nil {
			resp.Body.Close()
		}
		base := f.callback[:strings.LastIndex(f.callback, "/cb/")]
		r, err := http.Get(base + "/cb/0000000000000000?code=the-code")
		if err == nil {
			r.Body.Close()
		}
		return nil
	}
	if _, err := SignIn(context.Background(), o); !errors.Is(err, ErrCancelled) {
		t.Fatalf("a callback on another path must not count: %v", err)
	}
}

func TestExchangeRefusals(t *testing.T) {
	f := newFake(t)
	f.refuse = "bad code"
	_, err := SignIn(context.Background(), f.options(t, nil))
	if err == nil || !strings.Contains(err.Error(), "bad code") || strings.Contains(err.Error(), "the-code") {
		t.Fatalf("refusal must carry error.message and never the code: %v", err)
	}
	f.refuse, f.refuseRaw = "", true
	_, err = SignIn(context.Background(), f.options(t, nil))
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("bodyless refusal: %v", err)
	}
}

func TestTimeoutAndCancel(t *testing.T) {
	f := newFake(t)
	o := f.options(t, nil)
	o.Open = func(string) error { return nil } // nobody comes back
	o.Timeout = 200 * time.Millisecond
	if _, err := SignIn(context.Background(), o); !errors.Is(err, ErrCancelled) {
		t.Fatalf("timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	o.Timeout = 5 * time.Second
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if _, err := SignIn(ctx, o); !errors.Is(err, ErrCancelled) {
		t.Fatalf("cancel: %v", err)
	}
}

func TestLaunchFailureShowsURL(t *testing.T) {
	f := newFake(t)
	pasted := make(chan string, 1)
	o := f.options(t, pasted)
	shown := ""
	o.Open = func(string) error { return errors.New("no browser") }
	o.Show = func(u string) {
		shown = u
		resp, err := http.Get(u)
		if err == nil {
			resp.Body.Close()
		}
		pasted <- "the-code"
	}
	if key, err := SignIn(context.Background(), o); err != nil || key != "sk-or-test" || !strings.Contains(shown, "code_challenge=") {
		t.Fatalf("%q %v shown=%q", key, err, shown)
	}
}

func TestCodeFromPaste(t *testing.T) {
	for in, want := range map[string]string{
		"abc":                                     "abc",
		"  abc\n":                                 "abc",
		"http://127.0.0.1:1/cb/x?code=a%20b&y=1":  "a b",
		"http://127.0.0.1:1/cb/x?y=1":             "",
		"":                                        "",
	} {
		if got := CodeFromPaste(in); got != want {
			t.Errorf("%q: got %q want %q", in, got, want)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/signin/ 2>&1 | grep undefined | head -3`
Expected: `undefined: SignIn`, `Options`, `CodeFromPaste`.

- [ ] **Step 3: Implement the flow**

```go
// internal/signin/signin.go
// Package signin is OpenRouter's PKCE sign-in, which hands a third-party
// app a user-scoped API key with no client registration (spec §4). A
// leaf: stdlib plus procattr for the browser launch. The endpoints are
// Options fields so tests run the whole flow against an in-process fake.
package signin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Options configures one SignIn. Zero values take the OpenRouter defaults.
type Options struct {
	AuthorizeURL string
	ExchangeURL  string
	Open         func(url string) error
	Show         func(url string)
	Pasted       <-chan string
	Timeout      time.Duration
	Client       *http.Client
}

// ErrCancelled is the answer to a timeout, a context cancel, or a wait
// that ended with nothing usable: the one message the UIs print.
var ErrCancelled = errors.New("sign-in cancelled; nothing was stored")

const (
	defaultAuthorize = "https://openrouter.ai/auth"
	defaultExchange  = "https://openrouter.ai/api/v1/auth/keys"
	defaultTimeout   = 5 * time.Minute
	exchangeTimeout  = 15 * time.Second
)

// SignIn runs the flow and returns the key OpenRouter issued.
func SignIn(ctx context.Context, o Options) (string, error) {
	if o.AuthorizeURL == "" {
		o.AuthorizeURL = defaultAuthorize
	}
	if o.ExchangeURL == "" {
		o.ExchangeURL = defaultExchange
	}
	if o.Timeout <= 0 {
		o.Timeout = defaultTimeout
	}
	if o.Client == nil {
		o.Client = &http.Client{Timeout: exchangeTimeout}
	}
	if o.Show == nil {
		o.Show = func(string) {}
	}
	verifier, challenge, err := pkce()
	if err != nil {
		return "", err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("sign-in: %w", err)
	}
	defer ln.Close()
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	path := "/cb/" + hex.EncodeToString(id[:])
	codes := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if code == "" {
			http.Error(w, "no code in the callback", http.StatusBadRequest)
			return
		}
		fmt.Fprintln(w, "You can return to BE-Code.")
		select {
		case codes <- code:
		default:
		}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)
	defer srv.Close()

	callback := fmt.Sprintf("http://%s%s", ln.Addr().String(), path)
	q := url.Values{}
	q.Set("callback_url", callback)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	authURL := o.AuthorizeURL + "?" + q.Encode()
	if o.Open == nil || !displayAvailable() {
		o.Show(authURL)
	} else if err := o.Open(authURL); err != nil {
		o.Show(authURL)
	}

	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	var code string
	for code == "" {
		select {
		case code = <-codes:
		case s, ok := <-o.Pasted:
			if !ok {
				return "", ErrCancelled
			}
			code = CodeFromPaste(s)
		case <-ctx.Done():
			return "", ErrCancelled
		}
	}
	return exchange(ctx, o, code, verifier)
}

// CodeFromPaste reads the code out of what a person pasted: the whole
// redirect URL (the browser's address bar after a failed localhost
// redirect) or the bare code.
func CodeFromPaste(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil {
			return ""
		}
		return u.Query().Get("code")
	}
	return s
}

func pkce() (verifier, challenge string, err error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b[:])
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

func exchange(ctx context.Context, o Options, code, verifier string) (string, error) {
	body, _ := json.Marshal(map[string]string{"code": code, "code_verifier": verifier, "code_challenge_method": "S256"})
	req, err := http.NewRequestWithContext(ctx, "POST", o.ExchangeURL, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := o.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("sign-in: exchanging the code: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct{ Message string } `json:"error"`
		}
		if json.Unmarshal(raw, &e) == nil && e.Error.Message != "" {
			return "", fmt.Errorf("sign-in refused: %s", e.Error.Message)
		}
		return "", fmt.Errorf("sign-in refused: HTTP %d", resp.StatusCode)
	}
	var out struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Key == "" {
		return "", errors.New("sign-in: the exchange returned no key")
	}
	return out.Key, nil
}
```

```go
// internal/signin/browser.go
package signin

import (
	"os"
	"os/exec"
	"runtime"

	"github.com/brown-enterprises/be-code/internal/procattr"
)

// OpenBrowser opens url in the person's default browser. The command is
// started, not waited for: xdg-open returns at once, and a browser that
// was not running keeps the process otherwise.
func OpenBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	procattr.Hide(cmd)
	return cmd.Start()
}

// displayAvailable is false on a Linux box with no display: the URL is
// shown instead of launched, and the pasted redirect finishes the flow.
func displayAvailable() bool {
	if runtime.GOOS != "linux" {
		return true
	}
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}
```

Note for the tests: `displayAvailable` consults the environment, so the test file's `TestMain` (add one) sets `DISPLAY=:0` with `os.Setenv` before `m.Run()` so `o.Open` is exercised on a headless CI box; `TestLaunchFailureShowsURL` works either way.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/signin/ -race -v 2>&1 | grep -E "^(--- |ok|FAIL)"`
Expected: all PASS. `internal/procattr` has a test that greps the package source for `exec.Command` without `Hide`: it scans only its own package, but confirm with `go test ./internal/procattr/`.

- [ ] **Step 5: Commit**

```bash
git add internal/signin/signin.go internal/signin/browser.go internal/signin/signin_test.go
git commit --no-gpg-sign -m "feat(signin): OpenRouter PKCE sign-in with a loopback callback and a pasted fallback"
```

---

### Task 5: Shared UI pieces: `/online` report, revoke, the key-rejected hint, command tables

**Files:**
- Modify: `internal/ui/common.go:158-170` (`OnlineCommand`), `:80` (`SlashCommandTable` row for `/online`)
- Modify: `internal/agent/resilience.go:111-117` (`rejectedKeyError`)
- Modify: `internal/tui/view.go:1803-1804`, `internal/ui/repl.go:1127-1128` (callers of `OnlineCommand`)
- Modify: `internal/tui/palette.go:253` (menu row after "Switch provider")
- Test: `internal/ui/online_test.go` (append), `internal/agent/resilience_test.go` (append; create if absent)

**Interfaces:**
- Consumes: `cfg.KeySource(name)` (Task 2), `credentials.Open`/`Revoke` (Task 1), `provider.PresetByName`.
- Produces:
  ```go
  // OnlineCommand handles /online [forget|revoke <name>]; "add" is each
  // UI's own flow and returns usage here. cfg is for the credential line.
  func OnlineCommand(ag *agent.Agent, cfg *config.Config, args []string) string
  // CredentialLine is "credential: stored (oauth, 2026-11-02)", "credential:
  // from $X", or "credential: none (run /online add <name>)".
  func CredentialLine(cfg *config.Config, name string) string
  // RevokeCredential deletes name's stored credential and returns the
  // line both UIs and `be-code online revoke` print.
  func RevokeCredential(cfg *config.Config, name string) string
  ```

- [ ] **Step 1: Write the failing tests**

```go
// appended to internal/ui/online_test.go
func TestCredentialLineAndRevoke(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.Default()
	cfg.Providers["openrouter"] = config.ProviderConfig{Type: "openai", BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "BE_TEST_OR_KEY", Online: true}
	t.Setenv("BE_TEST_OR_KEY", "")
	if got := CredentialLine(cfg, "openrouter"); got != "credential: none (run /online add openrouter)" {
		t.Fatalf("none: %q", got)
	}
	s, _ := credentials.Open(filepath.Join(home, ".be-code"))
	s.Put("openrouter", credentials.Credential{Kind: credentials.KindAPIKey, Key: "sk-or-secret", Source: "oauth", Created: time.Date(2026, 11, 2, 1, 0, 0, 0, time.UTC)})
	if got := CredentialLine(cfg, "openrouter"); got != "credential: stored (oauth, 2026-11-02)" || strings.Contains(got, "secret") {
		t.Fatalf("stored: %q", got)
	}
	t.Setenv("BE_TEST_OR_KEY", "env-secret")
	if got := CredentialLine(cfg, "openrouter"); got != "credential: from $BE_TEST_OR_KEY" {
		t.Fatalf("env: %q", got)
	}
	line := RevokeCredential(cfg, "openrouter")
	if !strings.Contains(line, "revoked locally; revoke it at https://openrouter.ai/docs too") {
		t.Fatalf("revoke: %q", line)
	}
	if _, ok := s.Key("openrouter"); ok {
		t.Fatal("still stored")
	}
	if got := RevokeCredential(cfg, "openrouter"); got != "no stored credential for openrouter" {
		t.Fatalf("second revoke: %q", got)
	}
}

func TestOnlineCommandAddAndRevokeRouting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r := newTestREPL(t)
	if got := OnlineCommand(r.Agent, r.Cfg, []string{"revoke"}); got != "usage: /online [forget | add [name] | revoke <name>]" {
		t.Fatalf("%q", got)
	}
	if got := OnlineCommand(r.Agent, r.Cfg, []string{"revoke", "nobody"}); got != "no stored credential for nobody" {
		t.Fatalf("%q", got)
	}
	r.Agent.SetOnline("openrouter", "K", agent.Pricing{})
	r.Cfg.Providers["openrouter"] = config.ProviderConfig{Type: "openai", BaseURL: "https://openrouter.ai/api/v1", APIKeyEnv: "BE_TEST_OR_KEY", Online: true}
	if got := OnlineCommand(r.Agent, r.Cfg, nil); !strings.Contains(got, "credential: ") {
		t.Fatalf("report lacks the credential line: %q", got)
	}
}
```
Update the three existing calls in that file to the new signature (`OnlineCommand(r.Agent, r.Cfg, …)`).

```go
// appended to internal/agent/resilience_test.go
func TestRejectedKeyErrorNamesTheAddCommand(t *testing.T) {
	he := &provider.HTTPError{Provider: "groq", Code: 401, Body: "x"}
	got := rejectedKeyError("GROQ_API_KEY", he).Error()
	if !strings.Contains(got, "GROQ_API_KEY") || !strings.Contains(got, "or run /online add groq") {
		t.Fatalf("%q", got)
	}
	if got := rejectedKeyError("", he).Error(); !strings.Contains(got, "API key rejected by groq") || !strings.Contains(got, "or run /online add groq") {
		t.Fatalf("%q", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/ui/ ./internal/agent/ -run 'TestCredentialLine|TestOnlineCommandAdd|TestRejectedKeyError' 2>&1 | grep -E "undefined|FAIL|too many" | head`
Expected: `undefined: CredentialLine`, `RevokeCredential`, a signature mismatch on `OnlineCommand`, and the agent test failing on the missing hint.

- [ ] **Step 3: Implement**

`internal/ui/common.go` (add imports for `credentials`, `config`, `provider`, `time` as needed):

```go
// OnlineCommand is /online in both UIs: the report, /online forget,
// /online revoke <name>. /online add is each UI's own flow (it prompts),
// so here it is usage.
func OnlineCommand(ag *agent.Agent, cfg *config.Config, args []string) string {
	const usage = "usage: /online [forget | add [name] | revoke <name>]"
	if len(args) > 0 {
		switch args[0] {
		case "forget":
			if err := ag.ForgetOnline(); err != nil {
				return "error: " + err.Error()
			}
			name, _ := ag.Online()
			return "forgot the approval for " + name + "; the next session asks again"
		case "revoke":
			if len(args) != 2 {
				return usage
			}
			return RevokeCredential(cfg, args[1])
		}
		return usage
	}
	report := ag.OnlineReport()
	if name, online := ag.Online(); online && cfg != nil {
		report += "\n" + CredentialLine(cfg, name)
	}
	return report
}

// CredentialLine says where name's key comes from, never what it is.
func CredentialLine(cfg *config.Config, name string) string {
	src, stored := cfg.KeySource(name)
	switch src {
	case "env":
		return "credential: from $" + cfg.Providers[name].APIKeyEnv
	case "stored":
		return fmt.Sprintf("credential: stored (%s, %s)", stored.Source, stored.Created.UTC().Format("2006-01-02"))
	}
	return fmt.Sprintf("credential: none (run /online add %s)", name)
}

// RevokeCredential deletes the stored credential for name. The harness
// cannot revoke the key at the provider, so the line says to.
func RevokeCredential(cfg *config.Config, name string) string {
	dir, err := config.Dir()
	if err != nil {
		return "error: " + err.Error()
	}
	s, err := credentials.Open(dir)
	if err != nil {
		return "error: " + err.Error()
	}
	if _, ok := s.Key(name); !ok {
		return "no stored credential for " + name
	}
	if err := s.Revoke(name); err != nil {
		return "error: " + err.Error()
	}
	where := "the provider's site"
	if pre, ok := provider.PresetByName(name); ok {
		where = pre.Docs
	}
	return fmt.Sprintf("revoked locally; revoke it at %s too", where)
}
```

`SlashCommandTable` row: `{"/online", "the online main model: provider, approval, spend, credential; /online add [name] · revoke <name> · forget", true}`.

`internal/agent/resilience.go`, `rejectedKeyError`: append `; or run /online add <provider>` to both branches, where `<provider>` is `he.Provider`:

```go
func rejectedKeyError(keyEnv string, he *provider.HTTPError) error {
	who := "API key"
	if keyEnv != "" {
		who = "key in $" + keyEnv
	}
	return fmt.Errorf("%s rejected by %s; or run /online add %s (%w)", who, he.Provider, he.Provider, he)
}
```
(Read the current body first and keep its exact wording for `who`; only the `; or run /online add %s` part is new. If an existing test asserts the old text verbatim, update that assertion.)

Callers: `internal/tui/view.go:1804` → `ui.OnlineCommand(m.ag, m.cfg, fields[1:])`; `internal/ui/repl.go:1128` → `OnlineCommand(r.Agent, r.Cfg, fields[1:])`. The `add` case is dispatched before `OnlineCommand` in each UI (Tasks 6 and 7); for now both UIs fall through to usage.

Menu row in `internal/tui/palette.go` after "Switch provider": `{"Models", "Add an online provider", "OpenRouter sign-in, or a pasted key for any OpenAI-compatible endpoint", cmd("/online add")},`.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/ui/ ./internal/agent/ ./internal/tui/ 2>&1 | tail -3`
Expected: all `ok`. `internal/tui` has a test that keeps the menu and `SlashCommandTable` consistent (`palette_test.go` or `menu_test.go`); if it fails, it names the mismatch.

- [ ] **Step 5: Commit**

```bash
git add internal/ui/common.go internal/ui/online_test.go internal/agent/resilience.go internal/agent/resilience_test.go internal/tui/view.go internal/ui/repl.go internal/tui/palette.go
git commit --no-gpg-sign -m "feat(ui): /online revoke, the credential line, and the add hint on a rejected key"
```

---

### Task 6: The TUI flow (`/online add`)

**Files:**
- Create: `internal/tui/onlineadd.go`
- Create: `internal/tui/onlineadd_test.go`
- Modify: `internal/tui/view.go`: the `/online` case (~1803), the `modeInput` Enter handler (~700), `inputView()` (~1595), the `View` struct (field `onlineAdd *onlineAddFlow`), `handleInputKey` Esc case
- Modify: `internal/tui/session.go`: field `onlineAdding bool` under `mu`

**Interfaces:**
- Consumes: `setup.ChoiceFor`, `setup.Validate`, `setup.AddOnline`, `setup.ErrKeyRejected`, `setup.ErrUnreachable` (Task 3); `signin.SignIn`, `signin.Options`, `signin.OpenBrowser`, `signin.ErrCancelled` (Task 4); `credentials.Open` (Task 1); `ui.BusySafeCommand` (unchanged: `/online` is listed busy-safe for the report, so the `add` sub-command checks `m.running` itself); `m.openPicker`, `m.setProvider(name)`, `m.renderLocalNote`, `m.appendEntryLocked`, `m.histFile.add`, `m.Session.baseCtx()`.
- Produces: `func (m *View) startOnlineAdd(name string) (tea.Model, tea.Cmd)`; the message types below; `View.onlineAdd` consulted by the Enter handler, Esc handler and `inputView`.

**Design of the step state** (one file, so a reviewer sees the whole machine):

```go
// internal/tui/onlineadd.go
package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/credentials"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/setup"
	"github.com/brown-enterprises/be-code/internal/signin"
)

type onlineStep int

const (
	stepPreset   onlineStep = iota // picker: presets + custom
	stepName                       // input line: custom name
	stepURL                        // input line: custom base URL
	stepMethod                     // picker: browser | paste (openrouter only)
	stepKey                        // masked field
	stepWait                       // validating, or waiting for the browser (pasted redirect accepted)
	stepModel                      // picker over the listing, or typed when empty
	stepDefault                    // picker: yes | no
)

// onlineAddFlow is one terminal's /online add in progress. Everything a
// goroutine produces comes back as a message; the flow's own context is
// what Esc cancels.
type onlineAddFlow struct {
	step    onlineStep
	choice  setup.Choice
	note    string
	key     string
	source  string
	models  []provider.ModelInfo
	model   string
	secret  textinput.Model
	pasted  chan string
	ctx     context.Context
	cancel  context.CancelFunc
	browser bool
}

type onlineValidatedMsg struct {
	models []provider.ModelInfo
	err    error
}
type onlineSignedInMsg struct {
	key string
	err error
}
```

`onlineSignIn` and `onlineValidate` are package-level `var`s wrapping `signin.SignIn` and `setup.Validate`, so the tests swap them; `onlineOpenBrowser = signin.OpenBrowser` likewise.

- [ ] **Step 1: Write the failing tests**

```go
// internal/tui/onlineadd_test.go
package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/setup"
)

// stubOnline makes the network seams deterministic: validation accepts
// "good", sign-in returns "sk-or-test".
func stubOnline(t *testing.T) {
	t.Helper()
	ov, os_, ob := onlineValidate, onlineSignIn, onlineOpenBrowser
	onlineValidate = func(ctx context.Context, ch setup.Choice, key string) ([]provider.ModelInfo, error) {
		if strings.TrimSpace(key) != "good" && key != "sk-or-test" {
			return nil, setup.ErrKeyRejected
		}
		return []provider.ModelInfo{{ID: "m1"}, {ID: "m2"}}, nil
	}
	onlineSignIn = func(ctx context.Context, o signinOptions) (string, error) { return "sk-or-test", nil }
	onlineOpenBrowser = func(string) error { t.Fatal("the browser must never be launched in a test"); return nil }
	t.Cleanup(func() { onlineValidate, onlineSignIn, onlineOpenBrowser = ov, os_, ob })
}

func pickRow(t *testing.T, v *View, id string) {
	t.Helper()
	if v.mode != modePicker || v.picker == nil {
		t.Fatalf("not in a picker (mode %v)", v.mode)
	}
	for _, it := range v.picker.items {
		if it.id == id {
			_, cmd := v.picker.onPick(v, it)
			if cmd != nil {
				runCmd(t, v, cmd)
			}
			return
		}
	}
	t.Fatalf("picker has no row %q", id)
}

// runCmd executes a Cmd and feeds its message back through Update, once.
func runCmd(t *testing.T, v *View, cmd tea.Cmd) {
	t.Helper()
	if msg := cmd(); msg != nil {
		v.Update(msg)
	}
}

func typeLine(v *View, s string) {
	v.input.SetValue(s)
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})
}

func TestOnlineAddPasteFlowWritesBothFilesAndSwitches(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubOnline(t)
	v := newTestModel(t)
	v.cfg.Providers["ollama"] = v.cfg.Providers["ollama"]
	if err := v.cfg.Save(); err != nil {
		t.Fatal(err)
	}
	v.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/online add")})
	typeLine(v, "/online add")
	// Step: preset picker loads synchronously for the preset list.
	runCmd(t, v, v.lastCmd) // the picker's load Cmd (see note below on lastCmd)
	pickRow(t, v, "groq")
	if v.onlineAdd == nil || v.onlineAdd.step != stepKey {
		t.Fatalf("after preset: %+v", v.onlineAdd)
	}
	// The masked field replaces the textarea.
	if !strings.Contains(v.inputView(), "•") && !strings.Contains(v.inputView(), "key for groq") {
		t.Fatalf("input row is not the masked field: %q", v.inputView())
	}
	v.onlineAdd.secret.SetValue("good")
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	// Validation runs on a Cmd; drive it.
	runCmd(t, v, v.lastCmd)
	if v.onlineAdd.step != stepModel {
		t.Fatalf("after validate: step %v", v.onlineAdd.step)
	}
	pickRow(t, v, "m2")
	if v.onlineAdd.step != stepDefault {
		t.Fatalf("after model: step %v", v.onlineAdd.step)
	}
	pickRow(t, v, "no")
	if v.onlineAdd != nil {
		t.Fatal("flow still open")
	}
	tr := v.transcriptText()
	if strings.Contains(tr, "good") {
		t.Fatal("the key reached the transcript")
	}
	for _, want := range []string{"key entered (hidden)", "groq added (model m2); switching to it"} {
		if !strings.Contains(tr, want) {
			t.Fatalf("transcript lacks %q:\n%s", want, tr)
		}
	}
	re, err := config.Load()
	if err != nil || re.Providers["groq"].DefaultModel != "m2" || re.DefaultProvider == "groq" {
		t.Fatalf("config: %+v %v", re.Providers["groq"], err)
	}
	if re.ProviderKey("groq") != "good" {
		t.Fatal("credential not stored")
	}
	if v.ag.CurrentProvider() != "groq" {
		t.Fatalf("session not switched: %s", v.ag.CurrentProvider())
	}
	hist, _ := os.ReadFile(filepath.Join(home, ".be-code", "history"))
	if strings.Contains(string(hist), "good") {
		t.Fatal("the key reached the history file")
	}
}

func TestOnlineAddRejectedKeyReturnsToKeyStep(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubOnline(t)
	v := newTestModel(t)
	_, _ = v.startOnlineAdd("groq")
	v.onlineAdd.secret.SetValue("bad")
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	runCmd(t, v, v.lastCmd)
	if v.onlineAdd == nil || v.onlineAdd.step != stepKey {
		t.Fatalf("want the key step again, got %+v", v.onlineAdd)
	}
	if !strings.Contains(v.transcriptText(), "key rejected by groq") {
		t.Fatalf("no refusal line: %s", v.transcriptText())
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".be-code", "credentials.json")); !os.IsNotExist(err) {
		t.Fatal("a rejected key was stored")
	}
}

func TestOnlineAddEscLeavesNoFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubOnline(t)
	v := newTestModel(t)
	_, _ = v.startOnlineAdd("groq")
	v.onlineAdd.secret.SetValue("good")
	v.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if v.onlineAdd != nil || v.mode != modeInput {
		t.Fatalf("flow not cancelled: %+v mode %v", v.onlineAdd, v.mode)
	}
	if _, err := os.Stat(filepath.Join(home, ".be-code", "credentials.json")); !os.IsNotExist(err) {
		t.Fatal("esc left a credential file")
	}
	if !strings.Contains(v.transcriptText(), "cancelled; nothing was stored") {
		t.Fatalf("no cancel line: %s", v.transcriptText())
	}
}

func TestOnlineAddRefusedWhileBusyAndWhenAnotherTerminalIsAdding(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubOnline(t)
	s, a, b := twoViews(t)
	a.mu.Lock()
	a.running = true
	a.mu.Unlock()
	typeLine(a, "/online add groq")
	if a.onlineAdd != nil || !strings.Contains(a.transcriptText(), "commands wait until the agent is done") {
		t.Fatalf("busy: %+v %s", a.onlineAdd, a.transcriptText())
	}
	a.mu.Lock()
	a.running = false
	a.mu.Unlock()
	_, _ = a.startOnlineAdd("groq")
	_, _ = b.startOnlineAdd("groq")
	if b.onlineAdd != nil || !strings.Contains(b.transcriptText(), "another terminal is adding a provider") {
		t.Fatalf("second terminal: %+v %s", b.onlineAdd, b.transcriptText())
	}
	_ = s
}

func TestOnlineAddBrowserPathUsesSignIn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubOnline(t)
	v := newTestModel(t)
	_, _ = v.startOnlineAdd("openrouter")
	if v.onlineAdd.step != stepMethod {
		t.Fatalf("openrouter must offer the method picker, got %v", v.onlineAdd.step)
	}
	pickRow(t, v, "browser")
	runCmd(t, v, v.lastCmd) // the sign-in Cmd
	runCmd(t, v, v.lastCmd) // the validation Cmd it chains into
	if v.onlineAdd == nil || v.onlineAdd.step != stepModel || v.onlineAdd.source != "oauth" {
		t.Fatalf("after sign-in: %+v", v.onlineAdd)
	}
}

func TestOnlineAddCustomEndpointAsksNameAndURL(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	stubOnline(t)
	v := newTestModel(t)
	_, _ = v.startOnlineAdd("")
	runCmd(t, v, v.lastCmd)
	pickRow(t, v, "custom")
	if v.onlineAdd.step != stepName {
		t.Fatalf("step %v", v.onlineAdd.step)
	}
	typeLine(v, "groq") // a preset's name is refused; stays on the name step
	if v.onlineAdd.step != stepName || !strings.Contains(v.transcriptText(), "is a preset") {
		t.Fatalf("preset name accepted: %+v", v.onlineAdd)
	}
	typeLine(v, "mine")
	if v.onlineAdd.step != stepURL {
		t.Fatalf("step %v", v.onlineAdd.step)
	}
	typeLine(v, "https://api.together.xyz/v1")
	if v.onlineAdd.step != stepKey || v.onlineAdd.choice.Name != "mine" {
		t.Fatalf("%+v", v.onlineAdd)
	}
	_ = errors.New
}
```

Two helpers this test relies on that the implementer adds: `View.lastCmd tea.Cmd`, which `Update` sets to the Cmd it returns (wrap the existing body: `func (m *View) Update(msg tea.Msg) (tea.Model, tea.Cmd) { mm, cmd := m.update(msg); m.lastCmd = cmd; return mm, cmd }`, renaming the current `Update` to `update`); and `type signinOptions = signin.Options` declared in `onlineadd.go` so the stub's signature reads without the import. `helpers_test.go`'s `bindEnter` is the existing workaround for driving a Cmd; `lastCmd` replaces the need for it in these tests only.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/tui/ -run TestOnlineAdd 2>&1 | grep -E "undefined" | head -5`
Expected: `undefined: onlineValidate`, `startOnlineAdd`, `stepKey`, `lastCmd`.

- [ ] **Step 3: Implement the flow**

Add to `onlineadd.go`:

```go
var (
	onlineValidate    = setup.Validate
	onlineSignIn      = signin.SignIn
	onlineOpenBrowser = signin.OpenBrowser
)

type signinOptions = signin.Options

// startOnlineAdd is /online add [name] and the menu row. Not while a run
// is in flight (it ends in a provider switch), and one terminal at a time.
func (m *View) startOnlineAdd(name string) (tea.Model, tea.Cmd) {
	if m.running {
		m.appendEntryLocked(entry{Kind: entryDim, Text: "commands wait until the agent is done (Esc cancels); plain text is queued"})
		return m, nil
	}
	s := m.Session
	if s.onlineAdding {
		m.appendEntryLocked(entry{Kind: entryErr, Text: "another terminal is adding a provider"})
		return m, nil
	}
	s.onlineAdding = true
	ctx, cancel := context.WithCancel(s.baseCtx())
	f := &onlineAddFlow{ctx: ctx, cancel: cancel, pasted: make(chan string, 1)}
	m.onlineAdd = f
	if name == "" {
		f.step = stepPreset
		return m.openPicker("Add an online provider", m.presetRows, func(v *View, it pickItem) (tea.Model, tea.Cmd) {
			return v.onlineChose(it.id)
		})
	}
	return m.onlineChose(name)
}

func (m *View) presetRows() ([]pickItem, error) {
	var items []pickItem
	for _, p := range provider.Presets() {
		items = append(items, pickItem{id: p.Name, label: p.Name, desc: p.Docs})
	}
	items = append(items, pickItem{id: "custom", label: "custom OpenAI-compatible endpoint", desc: "a name and a base URL"})
	return items, nil
}

func (m *View) onlineChose(id string) (tea.Model, tea.Cmd) {
	f := m.onlineAdd
	m.mode = modeInput
	if id == "custom" {
		f.step = stepName
		m.appendEntryLocked(entry{Kind: entryDim, Text: "name for the new provider:"})
		return m, nil
	}
	ch, note, err := setup.ChoiceFor(m.cfg, id, "")
	if err != nil {
		return m.endOnlineAdd(err.Error())
	}
	f.choice, f.note = ch, note
	if note != "" {
		m.appendEntryLocked(entry{Kind: entryDim, Text: note})
	}
	if id == "openrouter" {
		f.step = stepMethod
		return m.openPicker("openrouter: how to sign in", func() ([]pickItem, error) {
			return []pickItem{
				{id: "browser", label: "Sign in with browser", desc: "OpenRouter issues a key for this machine"},
				{id: "paste", label: "Paste a key", desc: "from openrouter.ai/keys"},
			}, nil
		}, func(v *View, it pickItem) (tea.Model, tea.Cmd) {
			v.mode = modeInput
			if it.id == "browser" {
				return v.onlineBrowser()
			}
			return v.onlineAskKey()
		})
	}
	return m.onlineAskKey()
}

func (m *View) onlineAskKey() (tea.Model, tea.Cmd) {
	f := m.onlineAdd
	f.step = stepKey
	ti := textinput.New()
	ti.EchoMode = textinput.EchoPassword
	ti.EchoCharacter = '•'
	ti.Prompt = fmt.Sprintf("key for %s: ", f.choice.Name)
	ti.Focus()
	f.secret = ti
	return m, nil
}

// onlineLine is the Enter handler's hook: true when the line belonged to
// the flow. The key step never reaches here (its field is separate); the
// name, URL, typed-model and pasted-redirect steps do.
func (m *View) onlineLine(text string) (bool, tea.Cmd) {
	f := m.onlineAdd
	if f == nil {
		return false, nil
	}
	switch f.step {
	case stepName:
		if _, ok := provider.PresetByName(text); ok {
			m.appendEntryLocked(entry{Kind: entryErr, Text: text + " is a preset; a custom endpoint needs another name"})
			return true, nil
		}
		f.choice.Name = text
		f.step = stepURL
		m.appendEntryLocked(entry{Kind: entryDim, Text: "base URL (ending in /v1):"})
		return true, nil
	case stepURL:
		ch, note, err := setup.ChoiceFor(m.cfg, f.choice.Name, text)
		if err != nil {
			m.appendEntryLocked(entry{Kind: entryErr, Text: err.Error()})
			return true, nil
		}
		f.choice, f.note = ch, note
		_, _ = m.onlineAskKey()
		return true, nil
	case stepWait:
		if f.browser {
			select {
			case f.pasted <- text:
			default:
			}
		}
		return true, nil
	case stepModel:
		f.model = text
		return true, m.onlineAskDefault()
	}
	return false, nil
}

func (m *View) onlineKeyEntered() tea.Cmd {
	f := m.onlineAdd
	f.key = strings.TrimSpace(f.secret.Value())
	f.secret.SetValue("")
	if f.key == "" {
		return nil
	}
	f.source = "pasted"
	m.appendEntryLocked(entry{Kind: entryDim, Text: "key entered (hidden)"})
	return m.onlineValidateCmd()
}

func (m *View) onlineValidateCmd() tea.Cmd {
	f := m.onlineAdd
	f.step = stepWait
	m.statusNote = "checking " + f.choice.Name + "…"
	ctx, ch, key := f.ctx, f.choice, f.key
	return func() tea.Msg {
		models, err := onlineValidate(ctx, ch, key)
		return onlineValidatedMsg{models: models, err: err}
	}
}

func (m *View) onlineBrowser() (tea.Model, tea.Cmd) {
	f := m.onlineAdd
	f.step, f.browser, f.source = stepWait, true, "oauth"
	m.statusNote = "waiting for the browser; or paste the redirect URL"
	ctx, pasted := f.ctx, f.pasted
	show := func(u string) { m.Session.notice("open this URL to sign in: " + u) }
	return m, func() tea.Msg {
		key, err := onlineSignIn(ctx, signinOptions{Open: onlineOpenBrowser, Show: show, Pasted: pasted})
		return onlineSignedInMsg{key: key, err: err}
	}
}

// onlineMsg handles the flow's own messages; Update routes them here.
func (m *View) onlineMsg(msg tea.Msg) (tea.Model, tea.Cmd, bool) {
	f := m.onlineAdd
	if f == nil {
		return m, nil, false
	}
	switch msg := msg.(type) {
	case onlineSignedInMsg:
		m.statusNote = ""
		if msg.err != nil {
			if errors.Is(msg.err, signin.ErrCancelled) {
				return m.endOnlineAddPair("sign-in cancelled; nothing was stored")
			}
			m.appendEntryLocked(entry{Kind: entryErr, Text: msg.err.Error()})
			f.browser = false
			mm, cmd := m.onlineAskKey() // paste-a-key offered instead
			return mm, cmd, true
		}
		f.key = msg.key
		return m, m.onlineValidateCmd(), true
	case onlineValidatedMsg:
		m.statusNote = ""
		switch {
		case errors.Is(msg.err, setup.ErrKeyRejected):
			m.appendEntryLocked(entry{Kind: entryErr, Text: msg.err.Error()})
			mm, cmd := m.onlineAskKey()
			return mm, cmd, true
		case msg.err != nil:
			m.appendEntryLocked(entry{Kind: entryErr, Text: msg.err.Error() + " (Enter the key again to retry, Esc to cancel)"})
			mm, cmd := m.onlineAskKey()
			return mm, cmd, true
		}
		f.models = msg.models
		if len(f.models) == 0 {
			f.step = stepModel
			m.appendEntryLocked(entry{Kind: entryDim, Text: f.choice.Name + " lists no models; type a model name:"})
			return m, nil, true
		}
		f.step = stepModel
		mm, cmd := m.openPicker("Select model on "+f.choice.Name, func() ([]pickItem, error) {
			items := make([]pickItem, 0, len(f.models))
			for _, mo := range f.models {
				items = append(items, pickItem{id: mo.ID, label: mo.ID, desc: mo.Describe()})
			}
			return items, nil
		}, func(v *View, it pickItem) (tea.Model, tea.Cmd) {
			v.mode = modeInput
			v.onlineAdd.model = it.id
			return v, v.onlineAskDefault()
		})
		return mm, cmd, true
	}
	return m, nil, false
}

func (m *View) onlineAskDefault() tea.Cmd {
	f := m.onlineAdd
	f.step = stepDefault
	_, cmd := m.openPicker(fmt.Sprintf("make %s the default provider?", f.choice.Name), func() ([]pickItem, error) {
		return []pickItem{{id: "no", label: "no", desc: "this session only; the next launch keeps its default"}, {id: "yes", label: "yes", desc: "write default_provider"}}, nil
	}, func(v *View, it pickItem) (tea.Model, tea.Cmd) {
		v.mode = modeInput
		return v.onlineFinish(it.id == "yes")
	})
	return cmd
}

func (m *View) onlineFinish(makeDefault bool) (tea.Model, tea.Cmd) {
	f := m.onlineAdd
	dir, err := config.Dir()
	if err != nil {
		return m.endOnlineAdd(err.Error())
	}
	store, err := credentials.Open(dir)
	if err != nil {
		return m.endOnlineAdd(err.Error())
	}
	added, err := setup.AddOnline(m.cfg, store, f.choice, f.key, f.source, f.model, makeDefault)
	if err != nil {
		return m.endOnlineAdd(err.Error())
	}
	if added.Note != "" {
		m.appendEntryLocked(entry{Kind: entryDim, Text: added.Note})
	}
	m.appendEntryLocked(entry{Kind: entryOK, Text: fmt.Sprintf("%s added (model %s); switching to it", f.choice.Name, added.Model)})
	name := f.choice.Name
	m.clearOnlineAdd()
	return m.setProvider(name)
}

func (m *View) endOnlineAdd(line string) (tea.Model, tea.Cmd) {
	m.appendEntryLocked(entry{Kind: entryErr, Text: line})
	m.clearOnlineAdd()
	return m, nil
}

func (m *View) endOnlineAddPair(line string) (tea.Model, tea.Cmd, bool) {
	mm, cmd := m.endOnlineAdd(line)
	return mm, cmd, true
}

func (m *View) clearOnlineAdd() {
	if f := m.onlineAdd; f != nil {
		f.cancel()
		f.key = ""
	}
	m.onlineAdd = nil
	m.statusNote = ""
	m.Session.onlineAdding = false
	if m.mode == modePicker {
		m.mode = modeInput
	}
}

// cancelOnlineAdd is Esc: nothing has been written before onlineFinish.
func (m *View) cancelOnlineAdd() {
	if m.onlineAdd == nil {
		return
	}
	m.appendEntryLocked(entry{Kind: entryDim, Text: "cancelled; nothing was stored"})
	m.clearOnlineAdd()
}
```

Wiring in `view.go`:
- `View` gains `onlineAdd *onlineAddFlow` and `lastCmd tea.Cmd`; at the end of `Update`, before each `return m, cmd`, there is one exit point or a few; set `m.lastCmd = cmd` at the single wrapper if `Update` has one (`defer`-free: assign in a small wrapper `update()` the exported `Update` calls).
- `Update`, before its mode switch: `if mm, cmd, ok := m.onlineMsg(msg); ok { return mm, cmd }`.
- The `/online` case: `if len(fields) > 1 && fields[1] == "add" { return m.startOnlineAdd(strings.Join(fields[2:], " ")) }` before the `OnlineCommand` call.
- `handleInputKey` Enter: before `m.histFile.add(text, m.id)`, `if m.onlineAdd != nil && m.onlineAdd.step == stepKey { return m, m.onlineKeyEntered() }` (the masked field holds the text, not the textarea), and after `in.Reset()`: `if ok, cmd := m.onlineLine(text); ok { return m, cmd }` placed **before** `m.histFile.add` as well, so a URL or name typed into the flow is not history either (keep it simple: both checks before `histFile.add`).
- `handleInputKey` key routing: when `m.onlineAdd != nil && m.onlineAdd.step == stepKey`, every key except Enter and Esc goes to `m.onlineAdd.secret.Update(k)` instead of the textarea.
- `handleInputKey` Esc: `if m.onlineAdd != nil { m.cancelOnlineAdd(); return m, nil }` first.
- `handlePickerKey` Esc (closing a picker) while `m.onlineAdd != nil`: call `m.cancelOnlineAdd()` too, so Esc on the preset or model picker cancels the whole flow.
- `inputView()`: `if f := m.onlineAdd; f != nil && f.step == stepKey { return f.secret.View() }`.
- `session.go`: `onlineAdding bool` in the `Session` struct, documented "one terminal at a time in /online add".

Also: `ui.BusySafeCommand("/online")` is true for the report, so `startOnlineAdd`'s own `m.running` check is what refuses `add` mid-run; the test above pins that.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/tui/ -run 'TestOnlineAdd' -v 2>&1 | grep -E "^(--- |ok|FAIL|\s+onlineadd_test)"`
Expected: six PASS. Then the whole package: `go test ./internal/tui/ 2>&1 | tail -1` → `ok` (the served and shared tests must not notice the new field).

- [ ] **Step 5: Commit**

```bash
git add internal/tui/onlineadd.go internal/tui/onlineadd_test.go internal/tui/view.go internal/tui/session.go
git commit --no-gpg-sign -m "feat(tui): /online add — presets, masked key, browser sign-in, model and default pickers"
```

---

### Task 7: Plain mode (`/online add` in the REPL)

**Files:**
- Create: `internal/ui/onlineadd.go`
- Create: `internal/ui/onlineadd_test.go`
- Modify: `internal/ui/repl.go:1127-1128` (the `/online` case)

**Interfaces:**
- Consumes: `setup.ChoiceFor/Validate/AddOnline` (Task 3), `signin.SignIn/OpenBrowser/ErrCancelled` (Task 4), `credentials.Open` (Task 1), `r.promptAnswer(ctx, q)`, `r.rl.SetMaskRune(rune)` (chzyer/readline: a non-zero mask rune makes the read loop echo that rune; 0 restores), `r.Agent.SetProvider`, `r.Agent.SetModelNow`, `r.underPrompt`.
- Produces:
  ```go
  // Prompter is what the flow needs from a terminal: one question, one
  // answer, false when withdrawn. The REPL and `be-code online add` both
  // implement it; tests script it.
  type Prompter interface {
      Ask(ctx context.Context, q string) (string, bool)
      AskSecret(ctx context.Context, q string) (string, bool)
      Say(line string)
  }
  // OnlineAdd runs the numbered-prompt flow. It returns the provider's name
  // and model on success ("" when cancelled) so the REPL can switch; the
  // standalone command ignores them.
  func OnlineAdd(ctx context.Context, cfg *config.Config, p Prompter, name string) (provider, model string, err error)
  ```
  The seams `onlineValidate`, `onlineSignIn`, `onlineOpenBrowser` are package vars here too (same names as the TUI's, in package `ui`).

- [ ] **Step 1: Write the failing tests**

```go
// internal/ui/onlineadd_test.go
package ui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/setup"
	"github.com/brown-enterprises/be-code/internal/signin"
)

type scriptedPrompter struct {
	answers []string
	secrets []string
	said    []string
}

func (s *scriptedPrompter) Ask(_ context.Context, q string) (string, bool) {
	s.said = append(s.said, q)
	if len(s.answers) == 0 {
		return "", false
	}
	a := s.answers[0]
	s.answers = s.answers[1:]
	return a, true
}
func (s *scriptedPrompter) AskSecret(_ context.Context, q string) (string, bool) {
	s.said = append(s.said, q)
	if len(s.secrets) == 0 {
		return "", false
	}
	a := s.secrets[0]
	s.secrets = s.secrets[1:]
	return a, true
}
func (s *scriptedPrompter) Say(line string) { s.said = append(s.said, line) }
func (s *scriptedPrompter) all() string   { return strings.Join(s.said, "\n") }

func stubOnlineUI(t *testing.T) {
	t.Helper()
	ov, os_, ob := onlineValidate, onlineSignIn, onlineOpenBrowser
	onlineValidate = func(ctx context.Context, ch setup.Choice, key string) ([]provider.ModelInfo, error) {
		if strings.TrimSpace(key) != "good" && key != "sk-or-test" {
			return nil, setup.ErrKeyRejected
		}
		return []provider.ModelInfo{{ID: "m1"}, {ID: "m2"}}, nil
	}
	onlineSignIn = func(ctx context.Context, o signin.Options) (string, error) { return "sk-or-test", nil }
	onlineOpenBrowser = func(string) error { t.Fatal("no browser in tests"); return nil }
	t.Cleanup(func() { onlineValidate, onlineSignIn, onlineOpenBrowser = ov, os_, ob })
}

func TestOnlineAddPlainFlow(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubOnlineUI(t)
	cfg := config.Default()
	// answers: preset number for groq (3rd preset), model 2, default no
	p := &scriptedPrompter{answers: []string{"3", "2", "n"}, secrets: []string{"bad", "good\n"}}
	name, model, err := OnlineAdd(context.Background(), cfg, p, "")
	if err != nil || name != "groq" || model != "m2" {
		t.Fatalf("%q %q %v\n%s", name, model, err, p.all())
	}
	out := p.all()
	if strings.Contains(out, "good") || strings.Contains(out, "bad") {
		t.Fatalf("a key was printed:\n%s", out)
	}
	for _, want := range []string{"key rejected by groq", "key entered (hidden)", "[1] m1", "groq added (model m2)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	re, _ := config.Load()
	if re.Providers["groq"].DefaultModel != "m2" || re.ProviderKey("groq") != "good" || re.DefaultProvider == "groq" {
		t.Fatalf("%+v key=%q default=%s", re.Providers["groq"], re.ProviderKey("groq"), re.DefaultProvider)
	}
}

func TestOnlineAddPlainBrowserAndCancel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stubOnlineUI(t)
	cfg := config.Default()
	p := &scriptedPrompter{answers: []string{"1", "1", "y"}} // openrouter, browser, default yes
	name, _, err := OnlineAdd(context.Background(), cfg, p, "openrouter")
	if err != nil || name != "openrouter" {
		t.Fatalf("%q %v\n%s", name, err, p.all())
	}
	re, _ := config.Load()
	if re.DefaultProvider != "openrouter" || re.ProviderKey("openrouter") != "sk-or-test" {
		t.Fatalf("%+v", re)
	}
	// Withdrawn at the key step: nothing written.
	home2 := t.TempDir()
	t.Setenv("HOME", home2)
	p = &scriptedPrompter{answers: []string{"3"}}
	if name, _, err := OnlineAdd(context.Background(), config.Default(), p, ""); name != "" || err != nil {
		t.Fatalf("cancel: %q %v", name, err)
	}
	if _, err := os.Stat(filepath.Join(home2, ".be-code", "credentials.json")); !os.IsNotExist(err) {
		t.Fatal("cancel wrote a credential")
	}
	if !strings.Contains(p.all(), "cancelled; nothing was stored") {
		t.Fatalf("%s", p.all())
	}
}

func TestOnlineAddPlainNonTTYRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	r := newTestREPL(t)
	// os.Stdin under `go test` is not a terminal.
	out := capture(t, func() { r.command(context.Background(), "/online add groq") })
	if !strings.Contains(out, "needs a terminal") {
		t.Fatalf("%q", out)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/ui/ -run 'TestOnlineAddPlain' 2>&1 | grep -E "undefined" | head -4`
Expected: `undefined: OnlineAdd`, `Prompter`, `onlineValidate`.

- [ ] **Step 3: Implement**

```go
// internal/ui/onlineadd.go
package ui

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/credentials"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/setup"
	"github.com/brown-enterprises/be-code/internal/signin"
)

var (
	onlineValidate    = setup.Validate
	onlineSignIn      = signin.SignIn
	onlineOpenBrowser = signin.OpenBrowser
)

// Prompter is one terminal's questions and lines for OnlineAdd.
type Prompter interface {
	Ask(ctx context.Context, q string) (string, bool)
	AskSecret(ctx context.Context, q string) (string, bool)
	Say(line string)
}

// OnlineAdd is /online add [name] on numbered prompts (spec §6).
func OnlineAdd(ctx context.Context, cfg *config.Config, p Prompter, name string) (string, string, error) {
	cancelled := func() (string, string, error) { p.Say("cancelled; nothing was stored"); return "", "", nil }
	presets := provider.Presets()
	var ch setup.Choice
	if name == "" {
		p.Say("Online providers:")
		for i, pre := range presets {
			p.Say(fmt.Sprintf("  [%d] %-12s %s", i+1, pre.Name, pre.Docs))
		}
		p.Say(fmt.Sprintf("  [%d] custom OpenAI-compatible endpoint", len(presets)+1))
		n, ok := askNumber(ctx, p, "Choose", 1, len(presets)+1)
		if !ok {
			return cancelled()
		}
		if n <= len(presets) {
			name = presets[n-1].Name
		}
	}
	if name == "" {
		for {
			n, ok := p.Ask(ctx, "name for the new provider: ")
			if !ok {
				return cancelled()
			}
			if _, isPreset := provider.PresetByName(strings.TrimSpace(n)); isPreset {
				p.Say(strings.TrimSpace(n) + " is a preset; a custom endpoint needs another name")
				continue
			}
			u, ok := p.Ask(ctx, "base URL (ending in /v1): ")
			if !ok {
				return cancelled()
			}
			c, note, err := setup.ChoiceFor(cfg, n, u)
			if err != nil {
				p.Say(err.Error())
				continue
			}
			ch = c
			if note != "" {
				p.Say(note)
			}
			break
		}
	} else {
		c, note, err := setup.ChoiceFor(cfg, name, "")
		if err != nil {
			return "", "", err
		}
		ch = c
		if note != "" {
			p.Say(note)
		}
	}

	key, source := "", "pasted"
	var models []provider.ModelInfo
	browser := false
	if ch.Name == "openrouter" {
		p.Say("  [1] Sign in with browser\n  [2] Paste a key")
		n, ok := askNumber(ctx, p, "Choose", 1, 2)
		if !ok {
			return cancelled()
		}
		browser = n == 1
	}
	for {
		if browser {
			pasted := make(chan string, 1)
			go func() {
				// The person may paste the redirect URL while the browser is awaited.
				if s, ok := p.Ask(ctx, "waiting for the browser; or paste the redirect URL here: "); ok {
					pasted <- s
				}
			}()
			k, err := onlineSignIn(ctx, signin.Options{Open: onlineOpenBrowser, Show: func(u string) { p.Say("open this URL to sign in: " + u) }, Pasted: pasted})
			if err != nil {
				if errors.Is(err, signin.ErrCancelled) {
					p.Say(err.Error())
					return "", "", nil
				}
				p.Say(err.Error())
				browser = false // paste-a-key offered instead
				continue
			}
			key, source = k, "oauth"
		} else {
			k, ok := p.AskSecret(ctx, fmt.Sprintf("key for %s: ", ch.Name))
			if !ok {
				return cancelled()
			}
			key = strings.TrimSpace(k)
			if key == "" {
				continue
			}
			p.Say("key entered (hidden)")
		}
		p.Say("checking " + ch.Name + "…")
		ms, err := onlineValidate(ctx, ch, key)
		if errors.Is(err, setup.ErrKeyRejected) {
			p.Say(err.Error())
			browser = false
			continue
		}
		if err != nil {
			p.Say(err.Error())
			a, ok := p.Ask(ctx, "retry? [Y/n] ")
			if !ok || strings.HasPrefix(strings.ToLower(strings.TrimSpace(a)), "n") {
				return cancelled()
			}
			continue
		}
		models = ms
		break
	}

	model := ""
	if len(models) == 0 {
		p.Say(ch.Name + " lists no models")
		m, ok := p.Ask(ctx, "model name: ")
		if !ok {
			return cancelled()
		}
		model = strings.TrimSpace(m)
	} else {
		show := models
		if len(show) > 20 {
			show = show[:20]
			p.Say(fmt.Sprintf("  (showing first 20 of %d)", len(models)))
		}
		for i, m := range show {
			p.Say(fmt.Sprintf("  [%d] %s", i+1, m.ID))
		}
		n, ok := askNumber(ctx, p, "Choose a model", 1, len(show))
		if !ok {
			return cancelled()
		}
		model = show[n-1].ID
	}
	a, ok := p.Ask(ctx, fmt.Sprintf("make %s the default provider? [y/N] ", ch.Name))
	if !ok {
		return cancelled()
	}
	makeDefault := strings.HasPrefix(strings.ToLower(strings.TrimSpace(a)), "y")

	dir, err := config.Dir()
	if err != nil {
		return "", "", err
	}
	store, err := credentials.Open(dir)
	if err != nil {
		return "", "", err
	}
	added, err := setup.AddOnline(cfg, store, ch, key, source, model, makeDefault)
	if err != nil {
		return "", "", err
	}
	if added.Note != "" {
		p.Say(added.Note)
	}
	p.Say(fmt.Sprintf("%s added (model %s)", ch.Name, added.Model))
	return ch.Name, added.Model, nil
}

func askNumber(ctx context.Context, p Prompter, q string, lo, hi int) (int, bool) {
	for {
		a, ok := p.Ask(ctx, fmt.Sprintf("%s [%d-%d]: ", q, lo, hi))
		if !ok {
			return 0, false
		}
		n, err := strconv.Atoi(strings.TrimSpace(a))
		if err == nil && n >= lo && n <= hi {
			return n, true
		}
		p.Say(fmt.Sprintf("enter a number from %d to %d", lo, hi))
	}
}

// stdinIsTerminal is the /online add gate in plain mode.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}
```

The REPL's `Prompter` (in `repl.go`, next to the `/online` case):

```go
type replPrompter struct{ r *REPL }

func (p replPrompter) Ask(ctx context.Context, q string) (string, bool) { return p.r.promptAnswer(ctx, q) }
func (p replPrompter) AskSecret(ctx context.Context, q string) (string, bool) {
	p.r.rl.SetMaskRune('*')
	defer p.r.rl.SetMaskRune(0)
	return p.r.promptAnswer(ctx, q)
}
func (p replPrompter) Say(line string) { fmt.Println(line) }
```

and the case:

```go
	case "/online":
		if len(fields) > 1 && fields[1] == "add" {
			if !stdinIsTerminal() {
				fmt.Println("needs a terminal")
				break
			}
			name := strings.Join(fields[2:], " ")
			var provName, model string
			var err error
			r.underPrompt(ctx, func(c context.Context) { provName, model, err = OnlineAdd(c, r.Cfg, replPrompter{r}, name) })
			if err != nil {
				fmt.Printf("%s %v\n", red("error>"), err)
				break
			}
			if provName != "" {
				p, err := provider.FromConfig(r.Cfg, provName)
				if err != nil {
					fmt.Printf("%s %v\n", red("error>"), err)
					break
				}
				r.Agent.SetProvider(p)
				r.underPrompt(ctx, func(c context.Context) { r.Agent.SetModelNow(c, model) })
				fmt.Printf("provider set to %s (model %s)\n", r.Agent.CurrentProvider(), r.Agent.CurrentModel())
			}
			break
		}
		fmt.Println(OnlineCommand(r.Agent, r.Cfg, fields[1:]))
```

`underPrompt` bounds its context with `agent.ModelResolveTimeout`; check its value (`grep -n ModelResolveTimeout internal/agent/*.go`). If it is under the sign-in's 5 minutes, call `OnlineAdd` under `r.SetPromptContext(ctx)` with a plain `context.WithCancel` instead of `underPrompt` so the browser wait is not cut short; keep `underPrompt` for the `SetModelNow` that follows.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/ui/ 2>&1 | tail -2`
Expected: `ok`. `TestOnlineAddPlainNonTTYRefused` depends on `r.command` reaching the `/online` case without a readline instance: `newTestREPL` has `rl == nil`, and the non-TTY branch returns before `replPrompter` touches it; keep that order.

- [ ] **Step 5: Commit**

```bash
git add internal/ui/onlineadd.go internal/ui/onlineadd_test.go internal/ui/repl.go
git commit --no-gpg-sign -m "feat(ui): /online add in plain mode on numbered prompts with a masked key"
```

---

### Task 8: `be-code online add|revoke`, the wizard, `doctor`, docs

**Files:**
- Create: `cmd/online.go`, `cmd/online_test.go`
- Modify: `cmd/root.go:153` (register `onlineCmd`)
- Modify: `internal/setup/wizard.go:124-135` (`wizardOnline`'s key step)
- Modify: `internal/setup/wizard_test.go` (append)
- Modify: `cmd/commands.go:764-804` (`onlineDoctorLine`) and the print site at `:549` (a new `credentialDoctorLines(cfg)`)
- Modify: `cmd/doctor_test.go` (append)
- Modify: `README.md` (`## Online providers` 663–673, config reference 1451–1455, `## Command reference`), `CHANGELOG.md` (v1.3.1 entry)

**Interfaces:**
- Consumes: `ui.OnlineAdd`, `ui.Prompter`, `ui.RevokeCredential`, `ui.CredentialLine` (Tasks 5, 7); `setup.Validate/AddOnline` (Task 3); `signin` (Task 4); `cfg.KeySource`, `credentials.Open` (Tasks 1, 2).
- Produces: `onlineCmd` with `add [name]` and `revoke <name>`; `credentialDoctorLines(cfg *config.Config) []string`.

- [ ] **Step 1: Write the failing tests**

```go
// cmd/online_test.go
package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestOnlineAddRefusesNonTTY(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	rootCmd.SetArgs([]string{"online", "add", "groq"})
	err := rootCmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Fatalf("err=%v out=%s", err, out.String())
	}
}

func TestOnlineRevokeNeedsNoTerminal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetArgs([]string{"online", "revoke", "groq"})
	if err := rootCmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no stored credential for groq") {
		t.Fatalf("%s", out.String())
	}
}

func TestOnlineHasNoKeyFlag(t *testing.T) {
	for _, c := range onlineCmd.Commands() {
		if c.Flags().Lookup("key") != nil {
			t.Fatalf("%s has a --key flag", c.Name())
		}
	}
}
```
(Look at `cmd/apps_test.go` for how this package drives `rootCmd` in tests and copy its setup, including any `testmain_test.go` HOME guard.)

```go
// appended to cmd/doctor_test.go
func TestCredentialDoctorLines(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := config.Default()
	cfg.Providers["groq"] = config.ProviderConfig{Type: "openai", BaseURL: "https://api.groq.com/openai/v1", APIKeyEnv: "BE_TEST_GROQ_KEY", Online: true}
	t.Setenv("BE_TEST_GROQ_KEY", "")
	lines := strings.Join(credentialDoctorLines(cfg), "\n")
	for _, want := range []string{"credentials: none (~/.be-code/credentials.json absent)", "groq: key none: run /online add groq"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("missing %q in:\n%s", want, lines)
		}
	}
	dir := filepath.Join(home, ".be-code")
	s, _ := credentials.Open(dir)
	s.Put("groq", credentials.Credential{Kind: credentials.KindAPIKey, Key: "gsk_secret", Source: "pasted", Created: time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)})
	lines = strings.Join(credentialDoctorLines(cfg), "\n")
	if strings.Contains(lines, "gsk_secret") {
		t.Fatal("doctor printed a key")
	}
	for _, want := range []string{"credentials: ~/.be-code/credentials.json (mode 0600)", "groq: key stored (pasted 2026-11-02)"} {
		if !strings.Contains(lines, want) {
			t.Fatalf("missing %q in:\n%s", want, lines)
		}
	}
	os.Chmod(filepath.Join(dir, "credentials.json"), 0o644)
	lines = strings.Join(credentialDoctorLines(cfg), "\n")
	if !strings.Contains(lines, "readable by others; run chmod 600") {
		t.Fatalf("loose mode not flagged:\n%s", lines)
	}
	t.Setenv("BE_TEST_GROQ_KEY", "x")
	if lines = strings.Join(credentialDoctorLines(cfg), "\n"); !strings.Contains(lines, "groq: key $BE_TEST_GROQ_KEY (set)") {
		t.Fatalf("%s", lines)
	}
}
```

```go
// appended to internal/setup/wizard_test.go — the wizard's online path with
// a pasted key. Look at the existing online wizard test in this file for
// how probeBackends and listOnline are stubbed and how input is scripted
// (a bufio.Reader over a string); reuse those helpers.
func TestWizardOnlineAcceptsAPastedKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GROQ_API_KEY", "")
	oldP, oldL := probeBackends, listOnline
	t.Cleanup(func() { probeBackends, listOnline = oldP, oldL })
	probeBackends = func(context.Context, []Candidate, time.Duration) []Found { return nil }
	listOnline = func(context.Context, provider.Preset) ([]provider.ModelInfo, error) { return nil, errors.New("listOnline must not be used on the pasted-key path") }
	ov := onlineValidate
	onlineValidate = func(ctx context.Context, ch Choice, key string) ([]provider.ModelInfo, error) {
		if key != "good" {
			return nil, ErrKeyRejected
		}
		return []provider.ModelInfo{{ID: "m1"}, {ID: "m2"}}, nil
	}
	t.Cleanup(func() { onlineValidate = ov })
	// choices: online (last numbered), preset 3 = groq, key method 2 = paste, key, model 1
	in := bufio.NewReader(strings.NewReader("2\n3\n2\ngood\n1\n"))
	var out bytes.Buffer
	cfg, err := Wizard(context.Background(), in, &out)
	if err != nil || cfg == nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if strings.Contains(out.String(), "good") {
		t.Fatal("the key was echoed")
	}
	if cfg.DefaultProvider != "groq" || cfg.ProviderKey("groq") != "good" {
		t.Fatalf("%+v key=%q", cfg, cfg.ProviderKey("groq"))
	}
}
```
The exact numbered answers depend on how `Wizard` numbers the online choice when no backend is found; `TestWizardOnlineWithNoLocalBackend` in the same file scripts that case, so copy its first answers and append the new ones. The wizard reads the key from its `bufio.Reader` (it runs before any readline exists), so it cannot mask; it says `(the key is not echoed on a terminal)` and, when `out` is a terminal, reads it with `term.ReadPassword(os.Stdin.Fd())` from `charmbracelet/x/term`; with a non-terminal `in` (tests) it reads a line. Add `onlineValidate = Validate` as a package var in `setup` so the wizard test can stub it.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./cmd/ ./internal/setup/ -run 'TestOnline|TestCredentialDoctor|TestWizardOnlineAccepts' 2>&1 | grep -E "undefined|FAIL" | head -5`
Expected: `undefined: onlineCmd`, `credentialDoctorLines`; the wizard test fails on `MissingKeyError`.

- [ ] **Step 3: Implement**

`cmd/online.go`:

```go
package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/charmbracelet/x/term"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/ui"
)

var onlineCmd = &cobra.Command{
	Use:   "online",
	Short: "Add or revoke an online provider's stored credential",
	Long: `be-code online add [name] adds an online provider (a preset or a custom
OpenAI-compatible endpoint) with a pasted key or, for OpenRouter, a browser sign-in,
storing the key in ~/.be-code/credentials.json (mode 0600; an environment variable
named by api_key_env always wins). be-code online revoke <name> deletes the stored
credential. Inside a session the same flow is /online add.`,
}

var onlineAddCmd = &cobra.Command{
	Use:   "add [name]",
	Short: "Add an online provider (asks on the terminal; never a --key flag)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if !stdinIsTTY() {
			return errors.New("needs a terminal")
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		name := ""
		if len(args) == 1 {
			name = args[0]
		}
		p := stdioPrompter{in: bufio.NewReader(os.Stdin), out: cmd.OutOrStdout()}
		_, _, err = ui.OnlineAdd(cmd.Context(), cfg, p, name)
		return err
	},
}

var onlineRevokeCmd = &cobra.Command{
	Use:   "revoke <name>",
	Short: "Delete the stored credential for a provider",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), ui.RevokeCredential(cfg, args[0]))
		return nil
	},
}

// stdioPrompter asks on the process's own stdio: the standalone command
// and the setup wizard, which run before any readline exists.
type stdioPrompter struct {
	in  *bufio.Reader
	out interface{ Write([]byte) (int, error) }
}

func (p stdioPrompter) Ask(_ context.Context, q string) (string, bool) {
	fmt.Fprint(p.out, q)
	line, err := p.in.ReadString('\n')
	if err != nil && line == "" {
		return "", false
	}
	return strings.TrimRight(line, "\r\n"), true
}

func (p stdioPrompter) AskSecret(ctx context.Context, q string) (string, bool) {
	fmt.Fprint(p.out, q)
	if term.IsTerminal(os.Stdin.Fd()) {
		b, err := term.ReadPassword(os.Stdin.Fd())
		fmt.Fprintln(p.out)
		if err != nil {
			return "", false
		}
		return string(b), true
	}
	return p.Ask(ctx, "")
}

func (p stdioPrompter) Say(line string) { fmt.Fprintln(p.out, line) }

func init() {
	onlineCmd.AddCommand(onlineAddCmd, onlineRevokeCmd)
}
```
(`config.Load` on a fresh HOME writes defaults; if the `add` path should not create a config when none exists, use `config.Exists()` first and `config.Default()`; follow what `appsCmd` does.) Register in `cmd/root.go:153`: append `onlineCmd` to the `AddCommand` list.

`wizardOnline` in `internal/setup/wizard.go`: replace the `MissingKeyError` return with the three ways in. The wizard keeps its own numbered prompts (it predates `ui.Prompter`, and `setup` cannot import `ui`), so: if `os.Getenv(pre.KeyEnv) != ""`, use it as today; otherwise print `  [1] Sign in with browser` (OpenRouter only) / `  [N] Paste a key`, read the key (`term.ReadPassword` when `in` is `os.Stdin` on a terminal, else a line), run `onlineValidate(ctx, Choice{Name: pre.Name, BaseURL: pre.BaseURL, KeyEnv: pre.KeyEnv, Preset: true}, key)` for the listing (replacing `listOnline` for that branch), and after the model pick call `AddOnline` with `makeDefault: true` instead of `BuildOnlineConfig`+`Save` so the credential is stored. `MissingKeyError` stays defined (cmd/root.go names it) but is no longer returned; its comment says so. The wizard's browser path calls `signin.SignIn` with `Show` printing to `out` and `Pasted` fed from a goroutine reading `in` (same shape as `ui.OnlineAdd`).

`credentialDoctorLines` in `cmd/commands.go`, called right after `onlineDoctorLine` at the print site:

```go
// credentialDoctorLines: the credential file's state and, per online
// provider, where its key would come from. Never a key.
func credentialDoctorLines(cfg *config.Config) []string {
	var out []string
	dir, err := config.Dir()
	if err != nil {
		return nil
	}
	path := filepath.Join(dir, credentials.FileName)
	shown := "~/.be-code/" + credentials.FileName
	if fi, err := os.Stat(path); os.IsNotExist(err) {
		out = append(out, fmt.Sprintf("credentials: none (%s absent)", shown))
	} else if err == nil {
		if fi.Mode().Perm()&0o077 != 0 {
			out = append(out, fmt.Sprintf("credentials: %s is readable by others; run chmod 600 %s", shown, path))
		} else {
			out = append(out, fmt.Sprintf("credentials: %s (mode %04o)", shown, fi.Mode().Perm()))
		}
	}
	if s, err := credentials.Open(dir); err == nil {
		for _, name := range s.Unsupported() {
			out = append(out, fmt.Sprintf("credentials: %s: unsupported credential kind; update be-code", name))
		}
	}
	names := make([]string, 0, len(cfg.Providers))
	for name, pc := range cfg.Providers {
		if config.ProviderIsOnline(pc) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		src, stored := cfg.KeySource(name)
		switch src {
		case "env":
			out = append(out, fmt.Sprintf("%s: key $%s (set)", name, cfg.Providers[name].APIKeyEnv))
		case "stored":
			out = append(out, fmt.Sprintf("%s: key stored (%s %s)", name, stored.Source, stored.Created.UTC().Format("2006-01-02")))
		default:
			out = append(out, fmt.Sprintf("%s: key none: run /online add %s", name, name))
		}
	}
	return out
}
```
`onlineDoctorLine` (line 775) tests `os.Getenv(pc.APIKeyEnv) == ""` to say `key … missing`; change that test to `cfg.ProviderKey(name) == ""` so a stored key is probed, and render the key part through the same three cases (`$X set` / `stored` / `missing`).

Docs:
- README `## Online providers`: replace the sentence from "Setup saves nothing and says" through "never from the config file." with: "`be-code setup`, `be-code online add [name]` and `/online add` in a session take the key three ways: the environment variable the preset names, a key pasted into a masked field, or, for OpenRouter, a sign-in in the browser that issues a key for this machine (no account registration; on a box without a browser the URL is printed and the redirect can be pasted back). A pasted or signed-in key is stored in `~/.be-code/credentials.json` at mode 0600; `config.json` never holds a secret, and the environment variable always wins when set. `/online` shows where the key comes from, `/online revoke <name>` (or `be-code online revoke`) deletes the stored one, and `doctor` reports the file and each provider's key source."
- README config reference (1451–1455): "whose key comes from its environment variable (…) or, when that is unset, from `~/.be-code/credentials.json` (written by `/online add`; see Online providers)."
- README `## Command reference`: add `/online add [name]` and `/online revoke <name>`, and `be-code online add|revoke` next to `be-code apps`.
- CHANGELOG v1.3.1: a bullet "**Add an online provider from inside the harness.** `/online add` (and `be-code online add`, and `be-code setup`) takes a pasted key or, for OpenRouter, a browser sign-in, validates it against the provider's listing, and stores it in `~/.be-code/credentials.json` (0600). An environment variable still wins. `/online revoke`, `doctor` lines, and a `/online add` hint on a rejected key."
- `cmd/readme_test.go` guards parts of the README; run it.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./cmd/ ./internal/setup/ 2>&1 | tail -2`
Expected: both `ok`.

- [ ] **Step 5: Verify and commit**

Run: `make -f build.mk verify 2>&1 | grep -v "^ok" | grep -v "no test files" | tail -5`
Expected: `verify_download PASS` and nothing failing.

```bash
git add cmd/online.go cmd/online_test.go cmd/root.go cmd/commands.go cmd/doctor_test.go internal/setup/wizard.go internal/setup/wizard_test.go internal/setup/online.go README.md CHANGELOG.md
git commit --no-gpg-sign -m "feat(cmd): be-code online add|revoke, wizard key entry, doctor credential lines, docs"
```

---

### Task 9: End-to-end scenario and the live OpenRouter check

**Files:**
- Create: `test/e2e/online_e2e.sh`
- Modify: `test/e2e/run_e2e.sh` (call it before `api_e2e.sh`)
- Modify: `test/e2e/mock_server.py` (the `/v1/models` listing checks the bearer when `BE_E2E_KEY` is set)

**Interfaces:**
- Consumes: the built `./be-code`, `be-code online add` (Task 8) on a scripted stdin, `be-code doctor`, `be-code run -y`.

- [ ] **Step 1: Teach the mock to require a key**

In `mock_server.py`'s `do_GET` listing branch (the final `self._json({"data": …})`), before answering: if `os.environ.get("BE_E2E_KEY")` is set and the request's `Authorization` header is not `Bearer <that key>`, answer 401 (`self.send_response(401); self.end_headers()`). The chat handler (`do_POST`) gets the same check so the stored key is proven on a real request. Add `import os` at the top if absent. Without the variable set the server behaves as before, so every existing scenario is unchanged.

- [ ] **Step 2: Write the scenario**

```sh
#!/bin/sh
# Online setup scenario: `be-code online add` on a scripted stdin stores a
# key for a custom endpoint (the mock, which now demands it), config.json
# never holds it, doctor reports the source, and a real run uses it.
set -e
HERE=$(cd "$(dirname "$0")" && pwd)
BIN="$HERE/../../be-code"
WS=$(mktemp -d); HOME=$(mktemp -d); export HOME
trap 'rm -rf "$WS" "$HOME"; kill $SRV 2>/dev/null || :' EXIT
mkdir -p "$HOME/.be-code"
printf 'module example.com/online\n\ngo 1.22\n' > "$WS/go.mod"
printf 'package main\n\nfunc main() {}\n' > "$WS/main.go"
cat > "$HOME/.be-code/config.json" <<CFG
{"default_provider":"mock","model":"mock-model",
 "providers":{"mock":{"type":"openai","base_url":"http://127.0.0.1:18112/v1"}},
 "max_turns":4,"verify_on_done":false,"compat_tool_calls":"auto"}
CFG
BE_E2E_KEY=e2e-secret BE_E2E_PORT=18112 python3 "$HERE/mock_server.py" & SRV=$!
sleep 1

# Custom endpoint "far" at a non-local-looking URL is not possible against
# the mock, so the endpoint is local and online stays false; the key path
# is what this scenario proves. Answers: custom (last row), name, URL,
# key (wrong once, then right), model 1, default yes.
"$BIN" online add <<'IN' > "$WS/add.log" 2>&1
8
mockkey
http://127.0.0.1:18112/v1
wrong
e2e-secret
1
y
IN
grep -q "key rejected by mockkey" "$WS/add.log"
grep -q "mockkey added (model mock-model)" "$WS/add.log"
! grep -q "e2e-secret" "$WS/add.log"
! grep -q "e2e-secret" "$HOME/.be-code/config.json"
grep -q '"e2e-secret"' "$HOME/.be-code/credentials.json"
MODE=$(stat -c %a "$HOME/.be-code/credentials.json")
[ "$MODE" = "600" ]
"$BIN" doctor > "$WS/doctor.log" 2>&1 || :
grep -q "mockkey: key stored (pasted" "$WS/doctor.log"
! grep -q "e2e-secret" "$WS/doctor.log"
# A real run through the stored key: the mock refuses without it.
"$BIN" run -y -C "$WS" "add an Add function in add.go" > "$WS/run.log" 2>&1
grep -q '^}' "$WS/add.go"
"$BIN" online revoke mockkey | grep -q "revoked locally"
! grep -q mockkey "$HOME/.be-code/credentials.json"
echo "[PASS] online"
```
`mock_server.py` binds a fixed port 18111; give it `BE_E2E_PORT` (default 18111) so this scenario's server, which demands a key, does not collide with the suite's. The `online add` stdin above is not a terminal, so the `needs a terminal` gate from Task 8 refuses it. Run that one command under `script(1)` to give it a pty: `script -qec "$BIN online add" /dev/null <<'IN' > "$WS/add.log" 2>&1` (the heredoc feeds the pty's input). When `script` is not on PATH, print `[SKIP] online (no script(1))` and exit 0; the gate is tested for real in `cmd/online_test.go`.

- [ ] **Step 3: Run it**

Run: `make -f build.mk build && sh test/e2e/online_e2e.sh`
Expected: `[PASS] online`. Then `sh test/e2e/run_e2e.sh` end to end → `E2E PASS`.

- [ ] **Step 4: Commit**

```bash
git add test/e2e/online_e2e.sh test/e2e/run_e2e.sh test/e2e/mock_server.py
git commit --no-gpg-sign -m "test(e2e): online add stores a key the mock demands; doctor and run use it"
```

- [ ] **Step 5: Manual check against the live service (not a subagent task; the controller does it last)**

From this machine, in a throwaway HOME with the real config copied in: `be-code online add openrouter`, choose the browser, approve in the browser, confirm `credential: stored (oauth, <today>)` in `be-code doctor`, then `be-code online revoke openrouter` and revoke the key at openrouter.ai/settings/keys. If the live service rejects the documented parameter names, record the difference in the spec's amendments and fix `signin` to match; the fake in Task 4 then changes with it.
