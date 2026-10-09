# Pack Loader Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Install a skill pack from a git remote or a local path into
`<workspace>/.becode/packs/`, reachable from the TUI menu, a slash command and
the CLI.

**Architecture:** One new leaf package, `internal/packsrc`, fetches a pack
(git clone for a remote, a mode-preserving copy for a path) into a temp
directory, validates it with the same code `packs validate` runs, and only
then moves it into place. A committed `.pack-source.json` marker records
provenance and becomes the project's declaration that it wants the pack;
consent stays in `~/.be-code/engine/<key>/packs.json`, keyed by content hash,
and does not travel with the repository.

**Tech Stack:** Go (stdlib only, plus `os/exec` for `git` and
`internal/procattr` to hide the child on Windows). Cobra for the CLI. No new
module dependencies.

**Spec:** `docs/superpowers/specs/2026-10-09-pack-loader-design.md`

## Global Constraints

Copied from the spec; every task's requirements include these.

- Packs install **into the project only**: `<workspace>/.becode/packs/<name>/`.
  Nothing in this feature writes to `~/.be-code/packs`.
- Note the two dot-directories: `.be-code/` is the task engine's, `.becode/`
  is packs and custom commands. A pack in the wrong one is never found.
- **Every child process goes through `internal/procattr.Hide`.** The session
  host is detached with no console; an unhidden `exec.Command` opens a visible
  console window on Windows.
- **No test reaches the network.** `packsrc.DefaultSource` is a `var`; the git
  path is tested against a real local repository in a temp directory.
- **The model cannot install a pack.** No tool is registered. Menu, slash and
  CLI only; a fired turn refuses.
- `pack_install` has **no "always"**, and `-y` never answers it. It must be
  named in all three approvers: `internal/tui/view.go:904`,
  `internal/ui/repl.go:332`, `cmd/commands.go:277`.
- **Atomicity:** validate in a temp directory, then move. There is no state in
  which `.becode/packs/<name>/` holds half a pack.
- The marker carries **no checksum** of the pack. It is inside `ContentHash`,
  so editing it voids consent on its own.
- `internal/packs` must not import `internal/engine` (it stays a leaf for
  that): the trust path is passed in as a parameter.
- Go version and style follow the repo: `go vet` is the only linter, and
  `make -f build.mk verify` must pass before any task is called done.

## Review Focus

Input classes the spec implies that no task's deliverable would naturally
exercise. Each has its test added to the task that owns the code.

1. **The executable bit through a path copy.** Pack `tools/` wrappers are
   executables. `cmd/packs.go`'s validate already warns that "a copy does not
   always keep the executable bit"; an installer that loses it breaks every
   check at the first build. (Task 3)
2. **A pack name that escapes the directory.** `packs add ../../etc`, a name
   with a separator, or an absolute name must be refused *before* any path is
   joined. (Task 2)
3. **A symlink inside a fetched pack pointing outside it.** A fetched artifact
   is untrusted content; a `tools/` symlink to something outside the pack
   would be run by a check. (Task 3)
4. **A ref that is a commit SHA.** `git clone --depth 1 --branch <sha>` is
   rejected by most servers, so a SHA needs a different path or a clear error
   — and the spec invites SHAs by saying a commit is the real pin. (Task 3)
5. **An install interrupted partway.** A half-written pack directory that the
   loader then reads and puts in the system prompt. (Task 4)

---

### Task 1: The marker format, and forgetting a trust

**Files:**
- Create: `internal/packs/marker.go`
- Create: `internal/packs/marker_test.go`
- Modify: `internal/packs/trust.go` (add `Untrust` beside `ClearForced`, which is at line 177)

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `packs.MarkerName = ".pack-source.json"`
  - `type Marker struct { Pack, Version, Source, Ref, Commit, Installed string }`
  - `func ReadMarker(packRoot string) (Marker, bool)` — the bool is "a marker is there and parsed"
  - `func WriteMarker(packRoot string, m Marker) error`
  - `func (m Marker) DeclaresWanted(pack string) bool`
  - `func Untrust(path, pack string) error`

- [ ] **Step 1: Write the failing test**

Create `internal/packs/marker_test.go`:

```go
package packs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMarkerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Marker{
		Pack: "assist", Version: "0.1.0",
		Source:    "https://github.com/BE-AI-Research/be-skill-packs",
		Ref:       "main",
		Commit:    "45f1be8c9e2a1d4f6b8c0a3e5d7f9b1c2e4a6d80",
		Installed: "2026-10-09T16:12:04Z",
	}
	if err := WriteMarker(dir, want); err != nil {
		t.Fatal(err)
	}
	got, ok := ReadMarker(dir)
	if !ok {
		t.Fatal("ReadMarker said there was no marker")
	}
	if got != want {
		t.Errorf("round trip changed it:\n got %+v\nwant %+v", got, want)
	}
}

func TestNoMarkerIsNotAnError(t *testing.T) {
	if _, ok := ReadMarker(t.TempDir()); ok {
		t.Error("an empty directory reported a marker")
	}
}

func TestUnparsableMarkerIsNotAMarker(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, MarkerName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadMarker(dir); ok {
		t.Error("a corrupt marker was accepted; a pack must not activate on one")
	}
}

// The marker is a declaration of intent, so it must say which pack it is
// about. One naming a different pack is somebody else's marker, copied in.
func TestDeclaresWantedRequiresTheSameName(t *testing.T) {
	m := Marker{Pack: "assist"}
	if !m.DeclaresWanted("assist") {
		t.Error("a marker naming this pack should declare it wanted")
	}
	if m.DeclaresWanted("ue5") {
		t.Error("a marker naming another pack must not declare this one wanted")
	}
	if (Marker{}).DeclaresWanted("assist") {
		t.Error("a marker with no pack name declares nothing")
	}
}

func TestUntrustForgetsOnlyThatPack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "packs.json")
	if err := Trust(path, "assist", "hash-a"); err != nil {
		t.Fatal(err)
	}
	if err := Trust(path, "ue5", "hash-b"); err != nil {
		t.Fatal(err)
	}
	if err := Untrust(path, "assist"); err != nil {
		t.Fatal(err)
	}
	if got := TrustedHash(path, "assist"); got != "" {
		t.Errorf("assist is still trusted: %q", got)
	}
	if got := TrustedHash(path, "ue5"); got != "hash-b" {
		t.Errorf("ue5's approval was collateral damage: %q", got)
	}
}

func TestUntrustOnAMissingFileIsNotAnError(t *testing.T) {
	if err := Untrust(filepath.Join(t.TempDir(), "none.json"), "assist"); err != nil {
		t.Errorf("forgetting what was never recorded should succeed: %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/packs -run 'TestMarker|TestNoMarker|TestUnparsable|TestDeclares|TestUntrust' -v`
Expected: FAIL to build — `undefined: Marker`, `undefined: WriteMarker`, `undefined: ReadMarker`, `undefined: MarkerName`, `undefined: Untrust`.

- [ ] **Step 3: Write the marker**

Create `internal/packs/marker.go`:

```go
package packs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// MarkerName is the provenance file an installed pack carries, committed with
// the pack. It records where the pack came from, and it is the project's
// declaration that it wants the pack (design §7a).
//
// It deliberately holds no checksum of the pack. ContentHash covers every
// non-prose file at the pack root, this file included, so a checksum here
// would be circular — and leaving it out is stronger: editing the marker to
// forge a provenance claim changes the hash and voids the approval.
const MarkerName = ".pack-source.json"

// Marker is MarkerName's contents. Ref is what a person asked for and Commit
// is what they got: `packs update` needs the first, a listing shows the second.
type Marker struct {
	Pack      string `json:"pack"`
	Version   string `json:"version,omitempty"`
	Source    string `json:"source"`
	Ref       string `json:"ref,omitempty"`
	Commit    string `json:"commit,omitempty"`
	Installed string `json:"installed,omitempty"`
}

// Now is the clock the installer stamps a marker with. A test replaces it.
var Now = func() time.Time { return time.Now().UTC() }

// ReadMarker reads a pack's marker. The bool is "there is one and it parsed";
// a missing or corrupt marker is simply not a marker, never an error, because
// every caller's next move is the same either way.
func ReadMarker(packRoot string) (Marker, bool) {
	b, err := os.ReadFile(filepath.Join(packRoot, MarkerName))
	if err != nil {
		return Marker{}, false
	}
	var m Marker
	if err := json.Unmarshal(b, &m); err != nil {
		return Marker{}, false
	}
	return m, true
}

// WriteMarker writes a pack's marker, indented so a person reading the
// committed file can see what it says.
func WriteMarker(packRoot string, m Marker) error {
	if m.Installed == "" {
		m.Installed = Now().Format(time.RFC3339)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(packRoot, MarkerName), append(b, '\n'), 0o644)
}

// DeclaresWanted reports whether this marker is a declaration that the named
// pack is wanted in this project. It must name the pack it sits beside: a
// marker naming something else is somebody else's, copied in by hand.
//
// Wanting is not trust. A pack this returns true for still raises pack_trust
// before any of its rules reach the system prompt, so a repository can move a
// pack from "inactive" to "asks you" and never to "active".
func (m Marker) DeclaresWanted(pack string) bool {
	return pack != "" && m.Pack == pack
}
```

- [ ] **Step 4: Add `Untrust`**

In `internal/packs/trust.go`, directly after `ClearForced` (line 177):

```go
// Untrust forgets a pack's approval for this workspace. Removing a pack
// without it leaves an approval recorded for content that is no longer there,
// which a later reinstall — or a different pack that happens to share the
// name — would inherit.
func Untrust(path, pack string) error {
	return mutate(path, func(st *state) { delete(st.Trusted, pack) })
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/packs -run 'TestMarker|TestNoMarker|TestUnparsable|TestDeclares|TestUntrust' -v`
Expected: PASS, 6 tests.

- [ ] **Step 6: Confirm the marker is inside ContentHash**

This is the property the whole trust story rests on, so prove it rather than
assume it. Append to `internal/packs/marker_test.go`:

```go
// ContentHash must cover the marker: that is what makes forging a provenance
// claim void the approval instead of inheriting it.
func TestContentHashCoversTheMarker(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "core.md"), []byte("rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteMarker(dir, Marker{Pack: "p", Source: "a"}); err != nil {
		t.Fatal(err)
	}
	before, err := ContentHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteMarker(dir, Marker{Pack: "p", Source: "b"}); err != nil {
		t.Fatal(err)
	}
	after, err := ContentHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Error("editing the marker did not change the content hash, so a forged provenance claim would keep its approval")
	}
}
```

Run: `go test ./internal/packs -run TestContentHashCoversTheMarker -v`
Expected: PASS. If it fails, `ContentHash`'s exclusion rules have changed and
the marker must be brought back inside them — do not weaken the test.

- [ ] **Step 7: Commit**

```bash
git add internal/packs/marker.go internal/packs/marker_test.go internal/packs/trust.go
git commit -m "packs: the provenance marker, and forgetting a trust

The marker records where an installed pack came from and is the project's
declaration that it wants it. It carries no checksum deliberately: ContentHash
covers it, so editing it to forge provenance voids the approval instead of
inheriting it, and a test pins exactly that.

Untrust is the one function trust.go was missing — removing a pack otherwise
leaves an approval for content that is gone."
```

---

### Task 2: Source classification

**Files:**
- Create: `internal/packsrc/source.go`
- Create: `internal/packsrc/source_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `packsrc.DefaultSource` (a `var string`)
  - `type Kind int`, with `KindPath` and `KindGit`
  - `type Source struct { Spec string; Kind Kind; Ref string }`
  - `func Resolve(spec, ref string) (Source, error)`
  - `func ValidPackName(name string) error`

- [ ] **Step 1: Write the failing test**

Create `internal/packsrc/source_test.go`:

```go
package packsrc

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveClassifies(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name, spec string
		wantKind   Kind
		wantSpec   string
	}{
		{"empty takes the default", "", KindGit, DefaultSource},
		{"an existing directory is a path", dir, KindPath, dir},
		{"an https url is git", "https://github.com/o/r", KindGit, "https://github.com/o/r"},
		{"an ssh url is git", "ssh://git@host/o/r.git", KindGit, "ssh://git@host/o/r.git"},
		{"scp-style is git", "git@github.com:o/r.git", KindGit, "git@github.com:o/r.git"},
		{"a file url is git", "file:///srv/mirror.git", KindGit, "file:///srv/mirror.git"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Resolve(c.spec, "")
			if err != nil {
				t.Fatalf("Resolve(%q): %v", c.spec, err)
			}
			if got.Kind != c.wantKind {
				t.Errorf("kind = %v, want %v", got.Kind, c.wantKind)
			}
			if got.Spec != c.wantSpec {
				t.Errorf("spec = %q, want %q", got.Spec, c.wantSpec)
			}
		})
	}
}

// A bare word is a mistake, not a repository. Guessing it into a clone of
// something would contact a host the person never named.
func TestResolveRefusesABareWord(t *testing.T) {
	_, err := Resolve("be-skill-packs", "")
	if err == nil {
		t.Fatal("a bare word was accepted as a source")
	}
	if !strings.Contains(err.Error(), "be-skill-packs") {
		t.Errorf("the error should quote what was given: %v", err)
	}
}

func TestResolveKeepsTheRef(t *testing.T) {
	got, err := Resolve("https://github.com/o/r", "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if got.Ref != "v1.2.3" {
		t.Errorf("ref = %q, want v1.2.3", got.Ref)
	}
}

// Review Focus 2: the name is joined onto a directory, so it must never be
// able to leave it.
func TestValidPackNameRefusesEscapes(t *testing.T) {
	for _, bad := range []string{
		"", "..", "../etc", "a/b", `a\b`, "/abs", "C:/abs", ".", ".hidden",
		"a..b/../..", strings.Repeat("x", 200),
	} {
		if err := ValidPackName(bad); err == nil {
			t.Errorf("ValidPackName(%q) was accepted", bad)
		}
	}
	for _, ok := range []string{"assist", "ui-web", "ue5", "my_pack", "a1"} {
		if err := ValidPackName(ok); err != nil {
			t.Errorf("ValidPackName(%q) was refused: %v", ok, err)
		}
	}
}

// Belt and braces: whatever ValidPackName allows must still be a single
// element when joined.
func TestValidNamesStayOneElement(t *testing.T) {
	for _, n := range []string{"assist", "ui-web", "ue5"} {
		joined := filepath.Join("/base", n)
		if filepath.Dir(joined) != "/base" {
			t.Errorf("%q escaped its directory: %s", n, joined)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/packsrc -v`
Expected: FAIL — the package does not exist (`no Go files` / `undefined: Resolve`).

- [ ] **Step 3: Write the implementation**

Create `internal/packsrc/source.go`:

```go
// Package packsrc fetches a skill pack from somewhere and installs it into a
// project. It is the only code in BE-Code that reaches a pack registry, and
// the only one that runs git for a reason other than reading the workspace.
//
// Direction of dependency: packsrc imports internal/packs, never the reverse.
// The agent needs the marker and the trust file, not the fetcher, and keeping
// it that way is what stops os/exec reaching the agent's import graph.
package packsrc

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultSource is where a pack comes from when nobody names a source. It is
// a var so a test can point it at a local repository, exactly as
// update.APIBase is.
var DefaultSource = "https://github.com/BE-AI-Research/be-skill-packs"

// Kind is how a source has to be fetched.
type Kind int

const (
	// KindPath is a directory on this machine: copied, no git, no network.
	KindPath Kind = iota
	// KindGit is anything git can clone.
	KindGit
)

func (k Kind) String() string {
	if k == KindPath {
		return "path"
	}
	return "git"
}

// Source is a resolved place to fetch from.
type Source struct {
	Spec string // the directory or the clone URL
	Kind Kind
	Ref  string // branch, tag or commit; "" means the remote's default
}

// scpLike matches git's scp-style remote, user@host:path, which has no scheme
// and so would otherwise look like a bare word.
var scpLike = regexp.MustCompile(`^[^/@:]+@[^/@:]+:.+$`)

// Resolve classifies a source by inspection. It never guesses a bare word
// into a URL: that would contact a host the person did not name.
func Resolve(spec, ref string) (Source, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Source{Spec: DefaultSource, Kind: KindGit, Ref: ref}, nil
	}
	if st, err := os.Stat(spec); err == nil && st.IsDir() {
		abs, err := filepath.Abs(spec)
		if err != nil {
			abs = spec
		}
		return Source{Spec: abs, Kind: KindPath, Ref: ref}, nil
	}
	if strings.Contains(spec, "://") || scpLike.MatchString(spec) {
		return Source{Spec: spec, Kind: KindGit, Ref: ref}, nil
	}
	return Source{}, fmt.Errorf("source %q is neither a directory on this machine nor a URL git can clone; "+
		"pass a path, a git URL, or nothing at all to use %s", spec, DefaultSource)
}

// packName is what a pack directory may be called: one path element of
// ordinary characters. The name is joined onto a directory, so anything that
// could leave that directory is refused here, before any path is built.
var packName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidPackName refuses a name that is not a single safe path element.
func ValidPackName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("no pack name given")
	case !packName.MatchString(name):
		return fmt.Errorf("pack name %q is not usable: a pack name is 1-64 characters of letters, "+
			"digits, dot, dash or underscore, starting with a letter or digit", name)
	case name == "." || name == ".." || strings.HasPrefix(name, "."):
		return fmt.Errorf("pack name %q is not usable: it must not start with a dot", name)
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/packsrc -v`
Expected: PASS, 5 tests (`TestResolveClassifies` with 6 subtests).

- [ ] **Step 5: Commit**

```bash
git add internal/packsrc/source.go internal/packsrc/source_test.go
git commit -m "packsrc: classify a pack source by inspection, not by guessing

Empty means the default repository, an existing directory is a copy, and
anything with a scheme or git's scp-style form is a clone. A bare word is
refused: guessing it into a URL would contact a host nobody named.

ValidPackName refuses anything that is not one safe path element, because the
name is joined onto a directory."
```

---

### Task 3: Fetching — a mode-preserving copy, and git

**Files:**
- Create: `internal/packsrc/fetch.go`
- Create: `internal/packsrc/fetch_test.go`
- Create: `internal/packsrc/gitfixture_test.go`

**Interfaces:**
- Consumes: `Source`, `Kind`, `KindPath`, `KindGit`, `ValidPackName` (Task 2).
- Produces:
  - `type Fetched struct { Dir, Pack, Version, Commit string }`
  - `func Fetch(ctx context.Context, s Source, pack string) (Fetched, error)`
  - `type Available struct { Name, Version, Description string }`
  - `func List(ctx context.Context, s Source) ([]Available, error)`

- [ ] **Step 1: Write the fixture helper**

Create `internal/packsrc/gitfixture_test.go`. Real git repositories: the code
shells out to git, so a mock would prove nothing.

```go
package packsrc

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func git(t *testing.T, dir string, argv ...string) string {
	t.Helper()
	cmd := exec.Command("git", argv...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", argv, dir, err, out)
	}
	return string(out)
}

// packFiles writes a minimal but valid pack: a manifest, a core, and an
// executable wrapper, which is what Review Focus 1 is about.
func packFiles(t *testing.T, root, name, version string) {
	t.Helper()
	mustMkdir(t, filepath.Join(root, "tools"))
	mustWrite(t, filepath.Join(root, "pack.json"), `{
  "schema": 1,
  "name": "`+name+`",
  "version": "`+version+`",
  "description": "a pack for tests",
  "core": { "file": "core.md", "budget_tokens": 1500 }
}
`, 0o644)
	mustWrite(t, filepath.Join(root, "core.md"), "Rules for "+name+".\n", 0o644)
	mustWrite(t, filepath.Join(root, "tools", "check"), "#!/bin/sh\nexit 0\n", 0o755)
}

// packsRepo is a checkout shaped like be-skill-packs: packs/<name>/...
func packsRepo(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		packFiles(t, filepath.Join(dir, "packs", n), n, "0.1.0")
	}
	return dir
}

// gitPacksRepo is packsRepo with a git history, so it can be cloned.
func gitPacksRepo(t *testing.T, names ...string) (dir, head string) {
	t.Helper()
	dir = packsRepo(t, names...)
	git(t, dir, "init", "--quiet", "--initial-branch=main")
	git(t, dir, "config", "user.email", "fixture@example.com")
	git(t, dir, "config", "user.name", "Fixture")
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "--quiet", "-m", "packs")
	head = trim(git(t, dir, "rev-parse", "HEAD"))
	return dir, head
}

func mustMkdir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	mustMkdir(t, filepath.Dir(p))
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r') {
		s = s[:len(s)-1]
	}
	return s
}
```

- [ ] **Step 2: Write the failing test**

Create `internal/packsrc/fetch_test.go`:

```go
package packsrc

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFetchFromAPacksCheckout(t *testing.T) {
	src := packsRepo(t, "assist", "ue5")
	s, err := Resolve(src, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Fetch(context.Background(), s, "assist")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(got.Dir)
	if got.Pack != "assist" || got.Version != "0.1.0" {
		t.Errorf("got %q %q, want assist 0.1.0", got.Pack, got.Version)
	}
	if _, err := os.Stat(filepath.Join(got.Dir, "core.md")); err != nil {
		t.Errorf("core.md did not come across: %v", err)
	}
	if got.Commit != "" {
		t.Errorf("a path source has no commit, got %q", got.Commit)
	}
}

// A directory that is itself a pack, rather than a packs checkout.
func TestFetchFromABarePackDirectory(t *testing.T) {
	dir := t.TempDir()
	packFiles(t, dir, "assist", "0.2.0")
	s, err := Resolve(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Fetch(context.Background(), s, "")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(got.Dir)
	if got.Pack != "assist" || got.Version != "0.2.0" {
		t.Errorf("got %q %q, want assist 0.2.0", got.Pack, got.Version)
	}
}

// Review Focus 1: pack tools are executables, and a copy that drops the mode
// breaks every check at the first build.
func TestFetchKeepsTheExecutableBit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no executable bit on Windows")
	}
	src := packsRepo(t, "assist")
	s, _ := Resolve(src, "")
	got, err := Fetch(context.Background(), s, "assist")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(got.Dir)
	st, err := os.Stat(filepath.Join(got.Dir, "tools", "check"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0o111 == 0 {
		t.Errorf("tools/check came across as %v: a pack's wrappers must stay executable", st.Mode().Perm())
	}
}

// Review Focus 3: a fetched pack is untrusted content. A symlink pointing out
// of the pack would be read, or run, by a check.
func TestFetchRefusesASymlinkOutOfThePack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privilege on Windows")
	}
	src := packsRepo(t, "assist")
	if err := os.Symlink("/etc/passwd", filepath.Join(src, "packs", "assist", "tools", "escape")); err != nil {
		t.Fatal(err)
	}
	s, _ := Resolve(src, "")
	_, err := Fetch(context.Background(), s, "assist")
	if err == nil {
		t.Fatal("a pack containing a symlink out of itself was fetched")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("the error should say what was wrong: %v", err)
	}
}

func TestFetchFromAGitRepo(t *testing.T) {
	src, head := gitPacksRepo(t, "assist")
	s, err := Resolve("file://"+src, "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := Fetch(context.Background(), s, "assist")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(got.Dir)
	if got.Pack != "assist" {
		t.Errorf("pack = %q", got.Pack)
	}
	if got.Commit != head {
		t.Errorf("commit = %q, want %q", got.Commit, head)
	}
}

func TestFetchAtATag(t *testing.T) {
	src, _ := gitPacksRepo(t, "assist")
	git(t, src, "tag", "v1.0.0")
	// Move main on, so "at the tag" and "at the default branch" differ.
	mustWrite(t, filepath.Join(src, "packs", "assist", "core.md"), "Changed after the tag.\n", 0o644)
	git(t, src, "add", "-A")
	git(t, src, "commit", "--quiet", "-m", "after the tag")

	s, _ := Resolve("file://"+src, "v1.0.0")
	got, err := Fetch(context.Background(), s, "assist")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(got.Dir)
	body, err := os.ReadFile(filepath.Join(got.Dir, "core.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "Changed after the tag") {
		t.Error("fetched the default branch rather than the tag")
	}
}

// Review Focus 4: `clone --depth 1 --branch <sha>` is refused by most
// servers, so a commit SHA needs its own path.
func TestFetchAtACommitSHA(t *testing.T) {
	src, head := gitPacksRepo(t, "assist")
	mustWrite(t, filepath.Join(src, "packs", "assist", "core.md"), "Later.\n", 0o644)
	git(t, src, "add", "-A")
	git(t, src, "commit", "--quiet", "-m", "later")

	s, _ := Resolve("file://"+src, head)
	got, err := Fetch(context.Background(), s, "assist")
	if err != nil {
		t.Fatalf("a commit SHA must be fetchable: %v", err)
	}
	defer os.RemoveAll(got.Dir)
	if got.Commit != head {
		t.Errorf("commit = %q, want the SHA asked for %q", got.Commit, head)
	}
	body, _ := os.ReadFile(filepath.Join(got.Dir, "core.md"))
	if strings.Contains(string(body), "Later") {
		t.Error("fetched a later commit than the SHA asked for")
	}
}

func TestFetchNamesWhatTheSourceHasWhenThePackIsMissing(t *testing.T) {
	src := packsRepo(t, "assist", "ue5")
	s, _ := Resolve(src, "")
	_, err := Fetch(context.Background(), s, "nope")
	if err == nil {
		t.Fatal("fetching a pack the source does not have succeeded")
	}
	for _, want := range []string{"nope", "assist", "ue5"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should name %q: %v", want, err)
		}
	}
}

func TestFetchRefusesABadPackName(t *testing.T) {
	src := packsRepo(t, "assist")
	s, _ := Resolve(src, "")
	if _, err := Fetch(context.Background(), s, "../../etc"); err == nil {
		t.Fatal("a pack name that escapes the directory was accepted")
	}
}

func TestListReportsEveryPack(t *testing.T) {
	src := packsRepo(t, "assist", "ue5", "ui-web")
	s, _ := Resolve(src, "")
	got, err := List(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("got %d packs, want 3: %+v", len(got), got)
	}
	seen := map[string]string{}
	for _, a := range got {
		seen[a.Name] = a.Version
	}
	for _, n := range []string{"assist", "ue5", "ui-web"} {
		if seen[n] != "0.1.0" {
			t.Errorf("%s: version %q, want 0.1.0", n, seen[n])
		}
	}
}

func TestGitMissingSaysSo(t *testing.T) {
	src, _ := gitPacksRepo(t, "assist")
	s, _ := Resolve("file://"+src, "")
	t.Setenv("PATH", t.TempDir()) // a PATH with no git on it
	_, err := Fetch(context.Background(), s, "assist")
	if err == nil {
		t.Fatal("fetching with no git on PATH succeeded")
	}
	if !strings.Contains(err.Error(), "git") {
		t.Errorf("the error should name git: %v", err)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/packsrc -run 'TestFetch|TestList|TestGitMissing' -v`
Expected: FAIL to build — `undefined: Fetch`, `undefined: List`, `undefined: Fetched`, `undefined: Available`.

- [ ] **Step 4: Write the implementation**

Create `internal/packsrc/fetch.go`:

```go
package packsrc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/brown-enterprises/be-code/internal/procattr"
)

// Fetched is one pack in a temp directory. The caller removes Dir.
type Fetched struct {
	Dir     string
	Pack    string
	Version string
	Commit  string // "" for a path source: there is no commit to record
}

// Available is one pack a source offers, for the picker.
type Available struct {
	Name, Version, Description string
}

// manifest is the part of pack.json this package reads. The loader validates
// the rest; here we only need to know what a directory calls itself.
type manifest struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

func readManifest(dir string) (manifest, error) {
	b, err := os.ReadFile(filepath.Join(dir, "pack.json"))
	if err != nil {
		return manifest{}, err
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return manifest{}, fmt.Errorf("%s: %w", filepath.Join(dir, "pack.json"), err)
	}
	return m, nil
}

// Fetch puts one pack in a temp directory.
func Fetch(ctx context.Context, s Source, pack string) (Fetched, error) {
	if pack != "" {
		if err := ValidPackName(pack); err != nil {
			return Fetched{}, err
		}
	}
	root, commit, cleanup, err := checkout(ctx, s)
	if err != nil {
		return Fetched{}, err
	}
	defer cleanup()

	packDir, err := locate(root, pack)
	if err != nil {
		return Fetched{}, err
	}
	m, err := readManifest(packDir)
	if err != nil {
		return Fetched{}, err
	}
	if m.Name == "" {
		return Fetched{}, fmt.Errorf("%s has no name in its pack.json", packDir)
	}
	if pack != "" && m.Name != pack {
		return Fetched{}, fmt.Errorf("the directory for %q holds a pack called %q", pack, m.Name)
	}
	if err := ValidPackName(m.Name); err != nil {
		return Fetched{}, err
	}

	out, err := os.MkdirTemp("", "be-code-pack-")
	if err != nil {
		return Fetched{}, err
	}
	dest := filepath.Join(out, m.Name)
	if err := copyTree(packDir, dest); err != nil {
		os.RemoveAll(out)
		return Fetched{}, err
	}
	return Fetched{Dir: dest, Pack: m.Name, Version: m.Version, Commit: commit}, nil
}

// List is every pack a source offers.
func List(ctx context.Context, s Source) ([]Available, error) {
	root, _, cleanup, err := checkout(ctx, s)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	var out []Available
	for _, dir := range packDirs(root) {
		m, err := readManifest(dir)
		if err != nil || m.Name == "" {
			continue // not a pack; a source may hold other things
		}
		out = append(out, Available{Name: m.Name, Version: m.Version, Description: m.Description})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s offers no packs (looked for pack.json in %s and in its packs/ subdirectories)", s.Spec, s.Spec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// checkout gives a directory to read the source from, the commit it is at
// (empty for a path), and a cleanup the caller must run.
func checkout(ctx context.Context, s Source) (root, commit string, cleanup func(), err error) {
	if s.Kind == KindPath {
		return s.Spec, "", func() {}, nil
	}
	tmp, err := os.MkdirTemp("", "be-code-clone-")
	if err != nil {
		return "", "", func() {}, err
	}
	cleanup = func() { os.RemoveAll(tmp) }
	if err := clone(ctx, s, tmp); err != nil {
		cleanup()
		return "", "", func() {}, err
	}
	out, err := runGit(ctx, tmp, "rev-parse", "HEAD")
	if err != nil {
		cleanup()
		return "", "", func() {}, err
	}
	return tmp, strings.TrimSpace(out), cleanup, nil
}

var sha = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

// clone fetches s into dir. A ref that looks like a commit cannot go to
// `clone --branch`, which only takes a branch or tag and which most servers
// refuse a SHA for; that case clones and then checks the commit out.
func clone(ctx context.Context, s Source, dir string) error {
	if s.Ref != "" && sha.MatchString(s.Ref) {
		if _, err := runGit(ctx, "", "clone", "--quiet", s.Spec, dir); err != nil {
			return err
		}
		if _, err := runGit(ctx, dir, "checkout", "--quiet", s.Ref); err != nil {
			return fmt.Errorf("%s has no commit %s: %w", s.Spec, s.Ref, err)
		}
		return nil
	}
	argv := []string{"clone", "--quiet", "--depth", "1"}
	if s.Ref != "" {
		argv = append(argv, "--branch", s.Ref)
	}
	argv = append(argv, s.Spec, dir)
	if _, err := runGit(ctx, "", argv...); err != nil {
		if s.Ref != "" {
			return fmt.Errorf("could not fetch %s at %q: %w", s.Spec, s.Ref, err)
		}
		return fmt.Errorf("could not fetch %s: %w", s.Spec, err)
	}
	return nil
}

func runGit(ctx context.Context, dir string, argv ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", argv...)
	cmd.Dir = dir
	// The session host is detached with no console: an unhidden child opens a
	// visible console window on Windows.
	procattr.Hide(cmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", fmt.Errorf("git is not on PATH; a pack can still be installed from a local directory with --source <dir>")
		}
		msg := strings.TrimSpace(string(out))
		if len(msg) > 400 {
			msg = msg[:400] + "…"
		}
		if msg == "" {
			return "", err
		}
		return "", fmt.Errorf("%s", msg)
	}
	return string(out), nil
}

// packDirs is every directory in a source that might be a pack: the source
// itself, and each child of its packs/ directory.
func packDirs(root string) []string {
	var out []string
	if _, err := os.Stat(filepath.Join(root, "pack.json")); err == nil {
		out = append(out, root)
	}
	entries, err := os.ReadDir(filepath.Join(root, "packs"))
	if err != nil {
		return out
	}
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(root, "packs", e.Name()))
		}
	}
	return out
}

// locate finds the named pack in a checkout, or the only pack when no name
// was given. Ambiguity is an error, never a guess: installing the wrong pack
// writes somebody else's rules into the model's system prompt.
func locate(root, pack string) (string, error) {
	type candidate struct{ dir, name string }
	var found []candidate
	var names []string
	for _, d := range packDirs(root) {
		m, err := readManifest(d)
		if err != nil || m.Name == "" {
			continue // not a pack; a source may hold other things
		}
		found = append(found, candidate{d, m.Name})
		names = append(names, m.Name)
	}
	sort.Strings(names)

	if pack != "" {
		for _, c := range found {
			if c.name == pack {
				return c.dir, nil
			}
		}
		if len(names) == 0 {
			return "", fmt.Errorf("no pack %q here, and this source offers none", pack)
		}
		return "", fmt.Errorf("no pack %q here; this source offers %s", pack, strings.Join(names, ", "))
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("%s offers no packs", root)
	case 1:
		return found[0].dir, nil
	default:
		return "", fmt.Errorf("name which pack to install; this source offers %s", strings.Join(names, ", "))
	}
}

// copyTree copies src to dst, keeping each file's permission bits, and
// refuses a symlink. A fetched pack is untrusted content: a symlink out of it
// would be read — or, in tools/, run — by a check.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		if skipFetched(d.Name()) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			return fmt.Errorf("%s is a symlink; a pack may not contain one, because it could point out of the pack", rel)
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm()|0o700)
		case !info.Mode().IsRegular():
			return fmt.Errorf("%s is not a regular file (%v); a pack holds only files and directories", rel, info.Mode().Type())
		}
		return copyFile(p, target, info.Mode().Perm())
	})
}

// skipFetched is what never comes across from a source: version-control
// metadata, and anything a source's own tooling left behind.
func skipFetched(name string) bool {
	switch name {
	case ".git", ".hg", ".svn", "node_modules":
		return true
	}
	return false
}

func copyFile(src, dst string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// OpenFile's mode is masked by umask, so set it explicitly: a pack's
	// tools/ wrappers have to stay executable or every check fails at the
	// first build with a confusing error.
	return os.Chmod(dst, mode)
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/packsrc -v`
Expected: PASS, all tests. On a machine without `git`, the git tests fail —
that is correct; `git` is a hard dependency of BE-Code.

- [ ] **Step 6: Add the hidden-child source check**

Append to `internal/packsrc/fetch_test.go`. The same guard
`internal/tools` has: a child that is not hidden opens a console window on
Windows, and the session host has no console.

```go
func TestEveryExecCommandIsHidden(t *testing.T) {
	srcs, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range srcs {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		body := string(b)
		if !strings.Contains(body, "exec.Command") {
			continue
		}
		if !strings.Contains(body, "procattr.Hide") {
			t.Errorf("%s runs a child process without procattr.Hide: on Windows the detached "+
				"session host would open a visible console window for it", f)
		}
	}
}
```

Run: `go test ./internal/packsrc -run TestEveryExecCommandIsHidden -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/packsrc/fetch.go internal/packsrc/fetch_test.go internal/packsrc/gitfixture_test.go
git commit -m "packsrc: fetch a pack by clone or by mode-preserving copy

A path source is copied with each file's permission bits kept, because a
pack's tools/ wrappers are executables and packs validate already warns that a
copy does not always keep the bit. A symlink is refused outright: a fetched
pack is untrusted content and a link in tools/ would be run by a check.

A ref that looks like a commit cannot go to clone --branch, which most servers
refuse a SHA for, so that case clones and checks the commit out. Tests run
against real local git repositories; nothing reaches the network."
```

---

### Task 4: Install, update, remove

**Files:**
- Create: `internal/packsrc/install.go`
- Create: `internal/packsrc/install_test.go`

**Interfaces:**
- Consumes: `Fetch`, `Fetched`, `Source`, `Resolve` (Tasks 2–3); `packs.Load`,
  `packs.ContentHash`, `packs.Trust`, `packs.Untrust`, `packs.ClearForced`,
  `packs.WriteMarker`, `packs.ReadMarker`, `packs.Marker`, `packs.MarkerName`,
  `packs.CoreOverBudget` (Task 1 and existing).
- Produces:
  - `type Options struct { Workspace, TrustPath string; Approve func(action, detail string) bool; Force bool }`
  - `func Install(ctx context.Context, s Source, pack string, o Options) (Result, error)`
  - `func Update(ctx context.Context, pack string, o Options) (Result, error)`
  - `func Remove(pack string, o Options) error`
  - `type Result struct { Pack, Version, Commit, Dir string; Previous string }`
  - `func PacksDir(workspace string) string`

- [ ] **Step 1: Write the failing test**

Create `internal/packsrc/install_test.go`:

```go
package packsrc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/packs"
)

func opts(t *testing.T, yes bool) (Options, string) {
	t.Helper()
	ws := t.TempDir()
	return Options{
		Workspace: ws,
		TrustPath: filepath.Join(t.TempDir(), "packs.json"),
		Approve:   func(string, string) bool { return yes },
	}, ws
}

func TestInstallLandsInTheProjectAndRecordsTrust(t *testing.T) {
	src := packsRepo(t, "assist")
	s, _ := Resolve(src, "")
	o, ws := opts(t, true)

	res, err := Install(context.Background(), s, "assist", o)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(ws, ".becode", "packs", "assist")
	if res.Dir != want {
		t.Errorf("installed at %q, want %q", res.Dir, want)
	}
	if _, err := os.Stat(filepath.Join(want, "core.md")); err != nil {
		t.Errorf("the pack is not there: %v", err)
	}
	// Trust is recorded, so no prompt follows the person's own install.
	hash, err := packs.ContentHash(want)
	if err != nil {
		t.Fatal(err)
	}
	if got := packs.TrustedHash(o.TrustPath, "assist"); got != hash {
		t.Errorf("trusted hash = %q, want the installed content's %q", got, hash)
	}
	// Enablement comes from the marker, so nothing per-workspace is written.
	if _, set := packs.Forced(o.TrustPath, "assist"); set {
		t.Error("install wrote a Forced entry; the marker is what declares the pack wanted")
	}
}

func TestInstallWritesTheMarker(t *testing.T) {
	src, head := gitPacksRepo(t, "assist")
	s, _ := Resolve("file://"+src, "main")
	o, ws := opts(t, true)

	if _, err := Install(context.Background(), s, "assist", o); err != nil {
		t.Fatal(err)
	}
	m, ok := packs.ReadMarker(filepath.Join(ws, ".becode", "packs", "assist"))
	if !ok {
		t.Fatal("no marker was written")
	}
	if m.Pack != "assist" || m.Ref != "main" || m.Commit != head {
		t.Errorf("marker = %+v, want pack=assist ref=main commit=%s", m, head)
	}
	if m.Source != "file://"+src {
		t.Errorf("marker source = %q, want the source as given", m.Source)
	}
}

func TestInstallRefusedWritesNothing(t *testing.T) {
	src := packsRepo(t, "assist")
	s, _ := Resolve(src, "")
	o, ws := opts(t, false)

	if _, err := Install(context.Background(), s, "assist", o); err == nil {
		t.Fatal("a refused install succeeded")
	}
	if _, err := os.Stat(filepath.Join(ws, ".becode", "packs", "assist")); !os.IsNotExist(err) {
		t.Error("a refused install left something behind")
	}
	if got := packs.TrustedHash(o.TrustPath, "assist"); got != "" {
		t.Error("a refused install recorded a trust")
	}
}

// Review Focus 5: nothing half-installed, ever.
func TestAnInvalidPackIsNotInstalled(t *testing.T) {
	src := packsRepo(t, "assist")
	// A core far over its declared budget: the loader refuses it.
	big := strings.Repeat("rule after rule, and then some more of them. ", 500)
	mustWrite(t, filepath.Join(src, "packs", "assist", "core.md"), big, 0o644)
	s, _ := Resolve(src, "")
	o, ws := opts(t, true)

	_, err := Install(context.Background(), s, "assist", o)
	if err == nil {
		t.Fatal("a pack the loader would refuse was installed")
	}
	if _, serr := os.Stat(filepath.Join(ws, ".becode", "packs", "assist")); !os.IsNotExist(serr) {
		t.Error("a refused pack left a directory behind; the loader would read it")
	}
}

func TestInstallRefusesToClobberWithoutForce(t *testing.T) {
	src := packsRepo(t, "assist")
	s, _ := Resolve(src, "")
	o, _ := opts(t, true)
	if _, err := Install(context.Background(), s, "assist", o); err != nil {
		t.Fatal(err)
	}
	_, err := Install(context.Background(), s, "assist", o)
	if err == nil {
		t.Fatal("a second install overwrote the first silently")
	}
	if !strings.Contains(err.Error(), "update") {
		t.Errorf("the error should point at packs update: %v", err)
	}
	o.Force = true
	if _, err := Install(context.Background(), s, "assist", o); err != nil {
		t.Errorf("--force should replace it: %v", err)
	}
}

func TestUpdateMovesToTheNewCommitAndRetrusts(t *testing.T) {
	src, first := gitPacksRepo(t, "assist")
	s, _ := Resolve("file://"+src, "main")
	o, ws := opts(t, true)
	if _, err := Install(context.Background(), s, "assist", o); err != nil {
		t.Fatal(err)
	}

	mustWrite(t, filepath.Join(src, "packs", "assist", "core.md"), "Revised rules.\n", 0o644)
	git(t, src, "add", "-A")
	git(t, src, "commit", "--quiet", "-m", "revise")
	second := trim(git(t, src, "rev-parse", "HEAD"))

	res, err := Update(context.Background(), "assist", o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Previous != first || res.Commit != second {
		t.Errorf("moved %q→%q, want %q→%q", res.Previous, res.Commit, first, second)
	}
	body, _ := os.ReadFile(filepath.Join(ws, ".becode", "packs", "assist", "core.md"))
	if !strings.Contains(string(body), "Revised rules") {
		t.Error("the pack was not replaced")
	}
	hash, _ := packs.ContentHash(filepath.Join(ws, ".becode", "packs", "assist"))
	if packs.TrustedHash(o.TrustPath, "assist") != hash {
		t.Error("update did not re-record trust for the new content")
	}
}

func TestUpdateRefusesAPackWithNoMarker(t *testing.T) {
	o, ws := opts(t, true)
	dir := filepath.Join(ws, ".becode", "packs", "arrived")
	packFiles(t, dir, "arrived", "0.1.0")

	_, err := Update(context.Background(), "arrived", o)
	if err == nil {
		t.Fatal("updating a pack with no provenance succeeded")
	}
	if !strings.Contains(err.Error(), packs.MarkerName) {
		t.Errorf("the error should say the marker is missing: %v", err)
	}
}

func TestRemoveDeletesThePackAndItsState(t *testing.T) {
	src := packsRepo(t, "assist")
	s, _ := Resolve(src, "")
	o, ws := opts(t, true)
	if _, err := Install(context.Background(), s, "assist", o); err != nil {
		t.Fatal(err)
	}
	if err := packs.SetForced(o.TrustPath, "assist", false); err != nil {
		t.Fatal(err)
	}

	if err := Remove("assist", o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".becode", "packs", "assist")); !os.IsNotExist(err) {
		t.Error("the pack directory is still there")
	}
	if got := packs.TrustedHash(o.TrustPath, "assist"); got != "" {
		t.Errorf("a trust for content that is gone was left behind: %q", got)
	}
	if _, set := packs.Forced(o.TrustPath, "assist"); set {
		t.Error("a Forced entry for a pack that is gone was left behind")
	}
}

func TestRemoveRefusesANameThatEscapes(t *testing.T) {
	o, _ := opts(t, true)
	if err := Remove("../../etc", o); err == nil {
		t.Fatal("a name that escapes the packs directory was removed")
	}
}

func TestRemoveOfSomethingAbsentSaysSo(t *testing.T) {
	o, _ := opts(t, true)
	if err := Remove("assist", o); err == nil {
		t.Fatal("removing a pack that is not installed succeeded")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/packsrc -run 'TestInstall|TestUpdate|TestRemove|TestAnInvalid' -v`
Expected: FAIL to build — `undefined: Install`, `undefined: Update`,
`undefined: Remove`, `undefined: Options`, `undefined: Result`.

- [ ] **Step 3: Write the implementation**

Create `internal/packsrc/install.go`:

```go
package packsrc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/brown-enterprises/be-code/internal/packs"
)

// Options is everything an install needs from its caller. Approve is the
// person's seam: a nil Approve is a refusal, never a silent yes, because an
// install reaches the network and then writes rules into the model's own
// system prompt.
type Options struct {
	Workspace string // the project root
	TrustPath string // ~/.be-code/engine/<key>/packs.json
	Approve   func(action, detail string) bool
	Force     bool // replace a pack that is already installed
}

// Result is what an install or update did.
type Result struct {
	Pack     string
	Version  string
	Commit   string
	Dir      string
	Previous string // the commit replaced, for an update
}

// PacksDir is where a project's packs live. Note the dot-directory: the task
// engine uses .be-code, packs and custom commands use .becode, and a pack
// written to the wrong one is never found.
func PacksDir(workspace string) string {
	return filepath.Join(workspace, ".becode", "packs")
}

func packPath(workspace, pack string) (string, error) {
	if err := ValidPackName(pack); err != nil {
		return "", err
	}
	return filepath.Join(PacksDir(workspace), pack), nil
}

// Install fetches a pack and puts it in the project.
//
// The order is fixed and load-bearing: approve, fetch to a temp directory,
// validate it there, move it into place, write the marker, hash the result,
// record the trust. Validating before the move is what keeps a pack the
// loader would refuse from ever existing at its final path; hashing after the
// marker is what puts the marker inside the hash.
func Install(ctx context.Context, s Source, pack string, o Options) (Result, error) {
	detail := installDetail(s, pack, o.Workspace)
	if o.Approve == nil || !o.Approve("pack_install", detail) {
		return Result{}, fmt.Errorf("install declined")
	}
	got, err := Fetch(ctx, s, pack)
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(filepath.Dir(got.Dir))

	if err := validate(got.Dir); err != nil {
		return Result{}, err
	}
	dest, err := packPath(o.Workspace, got.Pack)
	if err != nil {
		return Result{}, err
	}
	previous := ""
	if _, err := os.Stat(dest); err == nil {
		if !o.Force {
			return Result{}, fmt.Errorf("%s is already installed at %s; use packs update to move it, or --force to replace it",
				got.Pack, dest)
		}
		if m, ok := packs.ReadMarker(dest); ok {
			previous = m.Commit
		}
		if err := os.RemoveAll(dest); err != nil {
			return Result{}, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return Result{}, err
	}
	if err := move(got.Dir, dest); err != nil {
		return Result{}, err
	}
	m := packs.Marker{
		Pack: got.Pack, Version: got.Version,
		Source: s.Spec, Ref: s.Ref, Commit: got.Commit,
		Installed: packs.Now().Format(time.RFC3339),
	}
	if err := packs.WriteMarker(dest, m); err != nil {
		return Result{}, err
	}
	if err := record(dest, got.Pack, o.TrustPath); err != nil {
		return Result{}, err
	}
	return Result{Pack: got.Pack, Version: got.Version, Commit: got.Commit, Dir: dest, Previous: previous}, nil
}

// Update re-fetches an installed pack at the ref its marker records.
func Update(ctx context.Context, pack string, o Options) (Result, error) {
	dest, err := packPath(o.Workspace, pack)
	if err != nil {
		return Result{}, err
	}
	if _, serr := os.Stat(dest); serr != nil {
		return Result{}, fmt.Errorf("%s is not installed in this project", pack)
	}
	m, ok := packs.ReadMarker(dest)
	if !ok || m.Source == "" {
		return Result{}, fmt.Errorf("%s has no %s, so there is nothing to say where it came from; "+
			"a pack that arrived with the project is updated by whoever maintains it", pack, packs.MarkerName)
	}
	s, err := Resolve(m.Source, m.Ref)
	if err != nil {
		return Result{}, err
	}
	o.Force = true
	return Install(ctx, s, pack, o)
}

// Remove deletes a pack from the project and forgets its per-workspace state.
// os.RemoveAll is rm -rf's semantics without a shell: Windows has no rm, and
// putting a path through a shell adds an injection surface for no gain.
func Remove(pack string, o Options) error {
	dest, err := packPath(o.Workspace, pack)
	if err != nil {
		return err
	}
	if _, serr := os.Stat(dest); serr != nil {
		return fmt.Errorf("%s is not installed in this project", pack)
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if o.TrustPath == "" {
		return nil
	}
	// A trust for content that is gone, or a switch for a pack that is gone,
	// would be inherited by a later reinstall or by a different pack that
	// happens to share the name.
	if err := packs.Untrust(o.TrustPath, pack); err != nil {
		return err
	}
	return packs.ClearForced(o.TrustPath, pack)
}

// validate is what `be-code packs validate` runs: a pack the loader would
// refuse must never reach its final path.
func validate(dir string) error {
	p, _, err := packs.Load(dir)
	if err != nil {
		return fmt.Errorf("not a usable pack: %w", err)
	}
	if over, est, budget := packs.CoreOverBudget(p); over {
		return fmt.Errorf("not installed: %s's core is about %d tokens against a budget of %d", p.Name, est, budget)
	}
	return nil
}

// record stamps the install as consent: the person asked for this pack, so
// the trust gate must not ask again for this exact content.
func record(dest, pack, trustPath string) error {
	if trustPath == "" {
		return nil
	}
	hash, err := packs.ContentHash(dest)
	if err != nil {
		return err
	}
	return packs.Trust(trustPath, pack, hash)
}

// move puts the validated pack at its final path in one step where it can,
// and falls back to copy-then-remove across filesystems, which a temp
// directory on another mount makes ordinary rather than exotic.
func move(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyTree(src, dst); err != nil {
		os.RemoveAll(dst)
		return err
	}
	return os.RemoveAll(src)
}

func installDetail(s Source, pack, workspace string) string {
	name := pack
	if name == "" {
		name = "(the pack this source offers)"
	}
	ref := s.Ref
	if ref == "" {
		ref = "the default branch"
	}
	what := "copy from"
	if s.Kind == KindGit {
		what = "fetch over the network from"
	}
	return fmt.Sprintf("install the skill pack %q\n  %s %s\n  at %s\n  into %s\n\n"+
		"A pack's rules go into the model's system prompt and its checks into the verification loop.",
		name, what, s.Spec, ref, filepath.Join(PacksDir(workspace), name))
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/packsrc -v`
Expected: PASS, all tests.

- [ ] **Step 5: Commit**

```bash
git add internal/packsrc/install.go internal/packsrc/install_test.go
git commit -m "packsrc: install, update and remove a project's packs

The order is load-bearing: approve, fetch to temp, validate there, move into
place, write the marker, hash, record the trust. Validating before the move is
what stops a pack the loader would refuse from ever existing at its final
path, and hashing after the marker is what puts the marker inside the hash.

Install records trust because the install was the consent, and writes no
Forced entry: the marker is what declares the pack wanted, so the setting
survives the project moving. Remove forgets both, since a trust for content
that is gone would be inherited by a reinstall.

A nil Approve is a refusal, never a silent yes."
```

---

### Task 5: The marker as an activation rung

**Files:**
- Modify: `internal/agent/packs.go:111-131` (the `switch` inside `LoadPacks`)
- Create: `internal/agent/packmarker_test.go`

**Interfaces:**
- Consumes: `packs.ReadMarker`, `packs.Marker.DeclaresWanted` (Task 1).
- Produces: no new exported API. The behaviour change is the precedence.

- [ ] **Step 1: Write the failing test**

Create `internal/agent/packmarker_test.go`. Follow the existing helpers in
this package: `newTestAgent(t, provider, mutators...)` returns an agent and
its workspace directory, and this package's `TestMain` pins `HOME`.

```go
package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/brown-enterprises/be-code/internal/packs"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// okProvider is this package's scripted provider with one canned reply. It is
// never called here: LoadPacks does not talk to a model. The helpers are
// newTestAgent (internal/agent/loop_test.go:46) and scriptedProvider
// (internal/agent/agent_test.go:89), both already in this package's tests.
func okProvider() provider.Provider {
	return &scriptedProvider{responses: []provider.ChatResponse{{Content: "ok"}}}
}

// projectPack writes a valid pack with no detect block into the workspace,
// and returns its directory.
func projectPack(t *testing.T, ws, name string) string {
	t.Helper()
	dir := filepath.Join(ws, ".becode", "packs", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pack.json", `{"schema":1,"name":"`+name+`","version":"0.1.0",
  "description":"a pack for tests","core":{"file":"core.md","budget_tokens":1500}}`)
	write("core.md", "Rules for "+name+".\n")
	return dir
}

func rowFor(ag *Agent, name string) (why string, found bool) {
	for _, r := range ag.PackRows() {
		if r.Name == name {
			return r.Why, true
		}
	}
	return "", false
}

// A pack with no detect block and no marker is inactive: today's behaviour,
// and the baseline the next test is measured against.
func TestPackWithoutMarkerIsNotWanted(t *testing.T) {
	ag, ws := newTestAgent(t, okProvider(), nil)
	projectPack(t, ws, "tp")
	ag.LoadPacks()
	why, ok := rowFor(ag, "tp")
	if !ok {
		t.Fatal("the pack was not discovered at all")
	}
	if why != "does not match this workspace" {
		t.Errorf("why = %q, want \"does not match this workspace\"", why)
	}
}

// The marker is the project's declaration that it wants the pack, so it is
// wanted even though nothing detects it.
func TestMarkerDeclaresThePackWanted(t *testing.T) {
	ag, ws := newTestAgent(t, okProvider(), nil)
	dir := projectPack(t, ws, "tp")
	if err := packs.WriteMarker(dir, packs.Marker{Pack: "tp", Source: "x"}); err != nil {
		t.Fatal(err)
	}
	ag.LoadPacks()
	why, _ := rowFor(ag, "tp")
	if why == "does not match this workspace" {
		t.Error("a pack whose marker declares it wanted was treated as unwanted")
	}
	if why != "installed in this project" {
		t.Errorf("why = %q, want \"installed in this project\"", why)
	}
}

// A marker naming another pack is somebody else's, copied in: it declares
// nothing.
func TestMarkerForAnotherPackDeclaresNothing(t *testing.T) {
	ag, ws := newTestAgent(t, okProvider(), nil)
	dir := projectPack(t, ws, "tp")
	if err := packs.WriteMarker(dir, packs.Marker{Pack: "somethingelse", Source: "x"}); err != nil {
		t.Fatal(err)
	}
	ag.LoadPacks()
	why, _ := rowFor(ag, "tp")
	if why != "does not match this workspace" {
		t.Errorf("why = %q: a marker naming another pack must not activate this one", why)
	}
}

// An explicit choice beats the marker, in both directions.
func TestForcedBeatsTheMarker(t *testing.T) {
	ag, ws := newTestAgent(t, okProvider(), nil)
	dir := projectPack(t, ws, "tp")
	if err := packs.WriteMarker(dir, packs.Marker{Pack: "tp", Source: "x"}); err != nil {
		t.Fatal(err)
	}
	path, err := ag.packTrustPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := packs.SetForced(path, "tp", false); err != nil {
		t.Fatal(err)
	}
	ag.LoadPacks()
	if why, _ := rowFor(ag, "tp"); why != "switched off" {
		t.Errorf("why = %q, want \"switched off\": /packs off must beat a marker", why)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/agent -run 'TestPackWithoutMarker|TestMarker|TestForcedBeats' -v`
Expected: `TestPackWithoutMarkerIsNotWanted` and
`TestMarkerForAnotherPackDeclaresNothing` PASS (today's behaviour already
gives them), `TestMarkerDeclaresThePackWanted` FAILS with
`why = "does not match this workspace", want "installed in this project"`,
and `TestForcedBeatsTheMarker` PASSES. **A partial RED is correct here**: two
of the four assert behaviour that must not change, so they are guards rather
than new requirements. Only one test is allowed to be red at this step; if
`TestForcedBeatsTheMarker` is also red, `Forced` is not being read before
detection and that is a pre-existing bug to fix first.

- [ ] **Step 3: Add the rung**

In `internal/agent/packs.go`, replace the `default:` arm of the `switch` at
line 111 (currently going straight to `MatchesDetail`) so the marker sits
between an explicit choice and detection:

```go
		switch {
		case set && !on:
			row.Why = "switched off"
		case set && on:
			row.Why = "switched on"
			candidates = append(candidates, p)
		default:
			// The marker is the project's declaration that it wants this
			// pack (design §7a). It sits above detection and below an
			// explicit /packs on|off, so a pack installed into a project
			// stays wanted even when the project is moved or renamed —
			// ~/.be-code/engine/<key>/ keys on a hash of the absolute
			// workspace path, so a move orphans the per-workspace entry and
			// a pack with no detect block would otherwise go silently
			// inactive while sitting in .becode/packs.
			//
			// Wanting is not trust: an untrusted pack still raises
			// pack_trust below, so a marker can move a pack from inactive to
			// asks-you and never to active.
			if m, ok := packs.ReadMarker(p.Root); ok && m.DeclaresWanted(p.Name) {
				row.Why = "installed in this project"
				candidates = append(candidates, p)
				break
			}
			matched, truncated := p.MatchesDetail(a.Tools.Root)
			switch {
			case matched:
				row.Why = "detected"
				candidates = append(candidates, p)
			case truncated:
				// Not the same as "no": detection stopped before it had seen
				// the whole tree, so saying it does not match would be a
				// statement of fact that may well be false.
				row.Why = "gave up looking on a large workspace; /packs on " + p.Name + " switches it on"
			default:
				row.Why = "does not match this workspace"
			}
		}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/agent -run 'TestPackWithoutMarker|TestMarker|TestForcedBeats' -v`
Expected: PASS, 4 tests.

- [ ] **Step 5: Run the whole agent package, which is where a regression would show**

Run: `go test ./internal/agent`
Expected: ok. This package's tests are the slowest in the repo; if one fails,
it is a real regression in activation, not flake.

- [ ] **Step 6: Commit**

```bash
git add internal/agent/packs.go internal/agent/packmarker_test.go
git commit -m "agent: an installed pack's marker declares it wanted

Activation becomes: an explicit /packs on|off choice, else a valid marker,
else the pack's own detect block.

Without the middle rung, moving or renaming a project orphans its
~/.be-code/engine/<key>/ entry — the key is a hash of the absolute workspace
path — and a pack with no detect block goes silently inactive while sitting
right there in .becode/packs, reported as \"does not match this workspace\".

No hole: wanting is not trust. An untrusted pack still raises pack_trust
before any rule reaches the system prompt."
```

---

### Task 6: An installed pack's checks run without an agent

**Files:**
- Modify: `internal/packs/discover.go:275-294` (`GlobalChecks` → `Checks`)
- Modify: `cmd/commands.go:672` (its one caller)
- Create: `internal/packs/checks_test.go`

**Interfaces:**
- Consumes: `packs.ContentHash`, `packs.TrustedHash` (existing), `Task 1`'s nothing.
- Produces: `func Checks(packsDir, workspace, trustPath string) (checks []verify.Check, notes []string)`.
  `GlobalChecks` is gone; there is one caller and no compatibility shim.

- [ ] **Step 1: Write the failing test**

Create `internal/packs/checks_test.go`:

```go
package packs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkPack writes a pack with one verify command into a workspace's
// .becode/packs, with a detect block so it matches any workspace.
func checkPack(t *testing.T, ws, name string) string {
	t.Helper()
	dir := filepath.Join(ws, ".becode", "packs", name)
	if err := os.MkdirAll(filepath.Join(dir, "tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string, mode os.FileMode) {
		p := filepath.Join(dir, rel)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, mode); err != nil {
			t.Fatal(err)
		}
	}
	write("pack.json", `{"schema":1,"name":"`+name+`","version":"0.1.0",
  "description":"a pack for tests",
  "core":{"file":"core.md","budget_tokens":1500},
  "detect":{"any_file":["**/*"]},
  "verify":[{"name":"lint","cmd":["{pack}/tools/lint","--project","{workspace}"],"parser":"generic"}]}`, 0o644)
	write("core.md", "Rules.\n", 0o644)
	write("tools/lint", "#!/bin/sh\nexit 0\n", 0o755)
	return dir
}

func TestChecksWithheldUntilApproved(t *testing.T) {
	ws := t.TempDir()
	checkPack(t, ws, "cp")
	trust := filepath.Join(t.TempDir(), "packs.json")

	got, notes := Checks(filepath.Join(t.TempDir(), "globalpacks"), ws, trust)
	if len(got) != 0 {
		t.Errorf("an unapproved pack's checks were returned: %+v", got)
	}
	if len(notes) == 0 || !strings.Contains(strings.Join(notes, " "), "cp") {
		t.Errorf("a withheld pack must be named in the notes: %v", notes)
	}
	// The old note told people to copy the pack to the global directory,
	// which this design rules out.
	if strings.Contains(strings.Join(notes, " "), "copy the pack") {
		t.Errorf("the note still suggests copying the pack somewhere: %v", notes)
	}
}

func TestChecksRunWhenTrustIsRecorded(t *testing.T) {
	ws := t.TempDir()
	dir := checkPack(t, ws, "cp")
	trust := filepath.Join(t.TempDir(), "packs.json")
	hash, err := ContentHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := Trust(trust, "cp", hash); err != nil {
		t.Fatal(err)
	}

	got, notes := Checks(filepath.Join(t.TempDir(), "globalpacks"), ws, trust)
	if len(got) != 1 {
		t.Fatalf("an approved project pack's checks were not returned (%d): notes %v", len(got), notes)
	}
	if got[0].Name != "lint" {
		t.Errorf("check name = %q, want lint", got[0].Name)
	}
}

func TestChecksWithheldAfterThePackChanges(t *testing.T) {
	ws := t.TempDir()
	dir := checkPack(t, ws, "cp")
	trust := filepath.Join(t.TempDir(), "packs.json")
	hash, _ := ContentHash(dir)
	if err := Trust(trust, "cp", hash); err != nil {
		t.Fatal(err)
	}
	// Somebody edits the committed pack.
	if err := os.WriteFile(filepath.Join(dir, "tools", "lint"), []byte("#!/bin/sh\nrm -rf /\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, _ := Checks(filepath.Join(t.TempDir(), "globalpacks"), ws, trust)
	if len(got) != 0 {
		t.Error("an edited pack's checks were still returned; the approval is for exact content")
	}
}

func TestChecksWithNoTrustPathWithholds(t *testing.T) {
	ws := t.TempDir()
	checkPack(t, ws, "cp")
	got, _ := Checks(filepath.Join(t.TempDir(), "globalpacks"), ws, "")
	if len(got) != 0 {
		t.Error("with nowhere to read approvals from, checks must be withheld")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/packs -run TestChecks -v`
Expected: FAIL to build — `undefined: Checks`.

- [ ] **Step 3: Rewrite the function**

In `internal/packs/discover.go`, replace `GlobalChecks` (line 275) with:

```go
// Checks is the verify commands of every installed pack that matches this
// workspace, for a caller with no agent: `be-code verify`.
//
// A global pack is the person's own, so its checks run. A project pack's need
// the trust decision — and since a pack can now be installed into a project
// deliberately (design §11), a recorded approval for exactly this content is
// that decision, readable here without an agent. One that has no approval is
// named in the notes rather than run unapproved or silently dropped.
//
// trustPath is a parameter rather than computed here so this package does not
// import internal/engine and stays a leaf; its caller knows the workspace and
// can build it.
func Checks(packsDir, workspace, trustPath string) (checks []verify.Check, notes []string) {
	found, warns := Discover(packsDir, workspace)
	notes = append(notes, warns...)
	for _, p := range found {
		if !p.Matches(workspace) {
			continue
		}
		if p.Source != SourceGlobal && !approvedHere(p, trustPath) {
			notes = append(notes, fmt.Sprintf("pack %q came with this project and has not been approved here, "+
				"so its checks are not run; run be-code interactively once to approve it", p.Name))
			continue
		}
		if over, est, budget := CoreOverBudget(p); over {
			notes = append(notes, fmt.Sprintf("pack %q is not active: its core is about %d tokens against a budget of %d", p.Name, est, budget))
			continue
		}
		checks = append(checks, p.Checks(workspace)...)
	}
	return checks, notes
}

// approvedHere reports whether this exact pack content is approved for this
// workspace. An unreadable pack, or nowhere to read approvals from, is not an
// approval: this decides whether to run somebody else's commands.
func approvedHere(p *Pack, trustPath string) bool {
	if trustPath == "" {
		return false
	}
	hash, err := ContentHash(p.Root)
	if err != nil {
		return false
	}
	return hash != "" && TrustedHash(trustPath, p.Name) == hash
}
```

- [ ] **Step 4: Update the one caller**

In `cmd/commands.go`, inside `verifyCmd`'s `RunE` (the block beginning at line
663), `root` and `dir` are already in scope. Replace the call and correct the
comment above it, which currently says global packs only:

```go
		// The installed packs' checks are part of this workspace's
		// verification too, or `be-code verify` disagrees with what the
		// session actually runs. This command has no agent and nobody to ask,
		// so a project pack's checks run only when an approval for exactly
		// its content is already recorded for this workspace — which is what
		// installing it deliberately writes. One without is named in the
		// notes instead.
		if dir, err := config.Dir(); err == nil {
			trustPath := filepath.Join(dir, "engine", engine.Key(root), "packs.json")
			packChecks, notes := packs.Checks(filepath.Join(dir, "packs"), root, trustPath)
```

Add `"github.com/brown-enterprises/be-code/internal/engine"` to that file's
imports if it is not already there.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/packs -run TestChecks -v && go build ./...`
Expected: PASS, 4 tests, and a clean build (which is what proves the caller
was updated).

- [ ] **Step 6: Commit**

```bash
git add internal/packs/discover.go internal/packs/checks_test.go cmd/commands.go
git commit -m "packs: run an approved project pack's checks without an agent

GlobalChecks refused every project pack and told the person to copy it to the
global directory. A pack installed into a project deliberately now has a
recorded approval for exactly its content, and that is readable without an
agent — so be-code verify and headless runs can honour it.

Withheld in three cases, each of them 'this is somebody else's command': no
approval, content that no longer matches the approval, or nowhere to read
approvals from. The trust path is a parameter so this package does not import
internal/engine."
```

---

### Task 7: The config key and the approval action

**Files:**
- Modify: `internal/config/config.go` (add `PacksConfig`, a field on `Config`, and its default at line 464's neighbourhood)
- Modify: `internal/tui/view.go:904-906` (the no-"always" branch)
- Modify: `internal/ui/repl.go:332` (`noAlwaysAction`)
- Modify: `cmd/commands.go:276-277` (`headlessApprover`)
- Create: `internal/config/packs_test.go`
- Create: `cmd/packinstall_test.go`
- Modify: `README.md` (config reference)

**Interfaces:**
- Consumes: nothing.
- Produces: `config.PacksConfig{Source string}`, `Config.Packs PacksConfig`,
  and the `pack_install` action recognised as having no "always" in all three
  approvers.

- [ ] **Step 1: Write the failing test**

Create `internal/config/packs_test.go`:

```go
package config

import "testing"

func TestPacksSourceDefault(t *testing.T) {
	c := Default()
	if c.Packs.Source != "https://github.com/BE-AI-Research/be-skill-packs" {
		t.Errorf("default packs.source = %q", c.Packs.Source)
	}
}
```

Create `cmd/packinstall_test.go`:

```go
package cmd

import (
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
)

// -y means "run the commands I chose", not "fetch code from the internet and
// put its rules in my system prompt".
func TestHeadlessNeverApprovesPackInstall(t *testing.T) {
	cfg := config.Default()
	cfg.AutoApproveShell = true
	cfg.AutoApproveBrowser = true
	if headlessApprover(cfg)("pack_install", "install assist from https://example/r") {
		t.Error("-y approved pack_install")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config -run TestPacksSourceDefault -v && go test ./cmd -run TestHeadlessNeverApprovesPackInstall -v`
Expected: the first FAILS to build (`c.Packs` undefined); the second FAILS
with "-y approved pack_install", because an unknown action falls through to
`cfg.AutoApproveShell`.

- [ ] **Step 3: Add the config section**

In `internal/config/config.go`, beside the other section types:

```go
// PacksConfig covers installing packs. Discovery and activation need no
// configuration: they follow the workspace.
type PacksConfig struct {
	// Source is where `packs add` fetches from when no --source is given. A
	// team pointing this at their own mirror makes every install in every
	// workspace come from it. Empty means the built-in default, so clearing
	// the key is not a way to break installs.
	Source string `json:"source"`
}
```

Add the field to `Config`, beside `Schedules`:

```go
	Packs PacksConfig `json:"packs"`
```

And to `Default()`, beside the `Schedules:` line:

```go
		Packs: PacksConfig{Source: "https://github.com/BE-AI-Research/be-skill-packs"},
```

- [ ] **Step 4: Name the action in all three approvers**

`internal/ui/repl.go`, in `noAlwaysAction` (line 332), add `"pack_install"` to
the case list, and extend the comment above it to name it.

`internal/tui/view.go` (line 904), add `|| a.Action == "pack_install"` to that
condition.

`cmd/commands.go` (line 277), add `action == "pack_install" ||` to the group,
and extend the comment above it:

```go
		// -y never approves pack_install (pack loader spec §8): it reaches the
		// network and then writes rules into the model's own system prompt.
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/config -run TestPacksSourceDefault -v && go test ./cmd -run TestHeadlessNeverApprovesPackInstall -v`
Expected: PASS, both.

- [ ] **Step 6: Pin the invariant that the model cannot install a pack**

Spec §8 says there is no tool for installing, so the protection is structural
rather than a gate: a package that cannot see the `tools.Tool` interface
cannot implement one. Pin that, in `internal/packsrc/source_test.go`:

```go
// Spec §8: the model cannot install a pack. The protection is that no tool
// exists, and the structural guarantee is that this package cannot build one
// — it does not import the registry. A future import of internal/tools here
// is the moment that stops being true, so it fails this test instead.
func TestPacksrcCannotRegisterATool(t *testing.T) {
	srcs, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range srcs {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "internal/tools") {
			t.Errorf("%s imports internal/tools: installing a pack must stay unreachable from the model, "+
				"because a model that can install a pack can rewrite its own system prompt", f)
		}
	}
}
```

It needs `os` and `strings` in that file's imports (`path/filepath` and
`strings` are already there).

Run: `go test ./internal/packsrc -run TestPacksrcCannotRegisterATool -v`
Expected: PASS.

- [ ] **Step 7: Check the TUI and REPL branches with the repo's own guard**

The TUI has a test that every approval action is classified; run the UI
packages to be sure the new action did not fall into the diff-preview branch.

Run: `go test ./internal/tui ./internal/ui`
Expected: ok. If a test names `pack_install` as unclassified, add it where
that test says.

- [ ] **Step 8: Document the key**

In `README.md`'s config reference, beside the other sections:

```markdown
| `packs.source` | `https://github.com/BE-AI-Research/be-skill-packs` | Where `be-code packs add` fetches from when `--source` is not given. Point it at your own mirror to make every install come from there. |
```

- [ ] **Step 9: Commit**

```bash
git add internal/config/config.go internal/config/packs_test.go internal/tui/view.go internal/ui/repl.go cmd/commands.go cmd/packinstall_test.go README.md
git commit -m "config: packs.source, and pack_install has no always

pack_install is named in all three approvers as an action with no \"always\",
and -y never answers it: it reaches the network and then writes rules into the
model's own system prompt, which is not what \"run the commands I chose\"
means."
```

---

### Task 8: The CLI

**Files:**
- Modify: `cmd/packs.go` (add three subcommands and register them)
- Create: `cmd/packs_add_test.go`

**Interfaces:**
- Consumes: `packsrc.Install`, `packsrc.Update`, `packsrc.Remove`,
  `packsrc.Options`, `packsrc.Resolve`, `packsrc.List`, `packsrc.PacksDir`
  (Tasks 2–4); `config.Load`, `config.Dir`, `engine.Key`.
- Produces: `be-code packs add|update|remove`, and a package-level
  `packsApprover` used by all three.

- [ ] **Step 1: Write the failing test**

Create `cmd/packs_add_test.go`:

```go
package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The CLI's own seam: a non-TTY stdin with no -y must refuse rather than hang
// or silently proceed, the same rule `be-code init` follows.
func TestPacksAddRefusesNonInteractiveWithoutYes(t *testing.T) {
	ws := t.TempDir()
	src := t.TempDir()
	writePackFixture(t, filepath.Join(src, "packs", "assist"))

	out, err := runPacksAdd(t, []string{"assist", "--source", src, "-C", ws})
	if err == nil {
		t.Fatalf("a non-interactive add succeeded without -y:\n%s", out)
	}
	if !strings.Contains(out+err.Error(), "-y") {
		t.Errorf("the refusal should say how to proceed: %s / %v", out, err)
	}
	if _, serr := os.Stat(filepath.Join(ws, ".becode", "packs", "assist")); !os.IsNotExist(serr) {
		t.Error("a refused add installed something")
	}
}

func TestPacksAddInstallsWithYes(t *testing.T) {
	ws := t.TempDir()
	src := t.TempDir()
	writePackFixture(t, filepath.Join(src, "packs", "assist"))

	out, err := runPacksAdd(t, []string{"assist", "--source", src, "-C", ws, "-y"})
	if err != nil {
		t.Fatalf("add failed: %v\n%s", err, out)
	}
	if _, serr := os.Stat(filepath.Join(ws, ".becode", "packs", "assist", "core.md")); serr != nil {
		t.Errorf("the pack is not installed: %v", serr)
	}
	if !strings.Contains(out, "assist") {
		t.Errorf("the output should name the pack: %s", out)
	}
}

func TestPacksAddWithNoNameListsWhatIsOnOffer(t *testing.T) {
	ws := t.TempDir()
	src := t.TempDir()
	writePackFixture(t, filepath.Join(src, "packs", "assist"))
	writePackFixture(t, filepath.Join(src, "packs", "ue5"))

	out, err := runPacksAdd(t, []string{"--source", src, "-C", ws, "-y"})
	if err == nil {
		t.Fatal("add with no pack name succeeded; it must ask which one")
	}
	for _, want := range []string{"assist", "ue5"} {
		if !strings.Contains(out+err.Error(), want) {
			t.Errorf("the output should offer %q: %s / %v", want, out, err)
		}
	}
}
```

Add the two helpers to the same file. `runPacksAdd` must run the cobra command
in-process with its output captured, and must pin `HOME` so no test writes to
the real dotdir — the repo's standing rule.

```go
func writePackFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(dir)
	files := map[string]string{
		"pack.json": `{"schema":1,"name":"` + name + `","version":"0.1.0",
  "description":"a pack for tests","core":{"file":"core.md","budget_tokens":1500}}`,
		"core.md": "Rules.\n",
	}
	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func runPacksAdd(t *testing.T, args []string) (string, error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir()) // never touch the real ~/.be-code
	var buf strings.Builder
	cmd := packsAddCmd
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs(args)
	defer func() {
		cmd.SetArgs(nil)
		flagYes = false
		flagPackSource = ""
	}()
	err := cmd.Execute()
	return buf.String(), err
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd -run TestPacksAdd -v`
Expected: FAIL to build — `undefined: packsAddCmd`, `undefined: flagPackSource`.

- [ ] **Step 3: Write the subcommands**

In `cmd/packs.go`, add the flag, the approver and the three commands, and
register them in that file's `init` beside `packsValidateCmd` and
`packsInfoCmd`:

```go
var flagPackSource string

// packsApprover is the person's seam for an install from the CLI. A TTY is
// asked; a non-interactive run refuses unless -y, the same rule be-code init
// follows — except that -y here approves only this one command the person
// typed, which is not the same as the headless approver's blanket -y.
func packsApprover(out io.Writer) func(string, string) bool {
	return func(action, detail string) bool {
		fmt.Fprintf(out, "%s\n", detail)
		if flagYes {
			fmt.Fprintln(out, "approved by -y")
			return true
		}
		if !stdinIsTTY() {
			fmt.Fprintln(out, "non-interactive; use -y to approve")
			return false
		}
		fmt.Fprint(out, "install it? [y/N] ")
		r := bufio.NewReader(os.Stdin)
		line, _ := r.ReadString('\n')
		return strings.EqualFold(strings.TrimSpace(line), "y")
	}
}

// packOptions is the workspace, the trust file and the approver, which every
// one of the three subcommands needs.
func packOptions(out io.Writer) (packsrc.Options, error) {
	root, err := filepath.Abs(flagDir)
	if err != nil {
		return packsrc.Options{}, err
	}
	base, err := config.Dir()
	if err != nil {
		return packsrc.Options{}, err
	}
	return packsrc.Options{
		Workspace: root,
		TrustPath: filepath.Join(base, "engine", engine.Key(root), "packs.json"),
		Approve:   packsApprover(out),
	}, nil
}

var packsAddCmd = &cobra.Command{
	Use:   "add [name[@ref]]",
	Short: "Install a skill pack into this project",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		name, ref := "", ""
		if len(args) == 1 {
			name, ref, _ = strings.Cut(args[0], "@")
		}
		spec := flagPackSource
		if spec == "" {
			if cfg, err := config.Load(); err == nil && cfg.Packs.Source != "" {
				spec = cfg.Packs.Source
			}
		}
		s, err := packsrc.Resolve(spec, ref)
		if err != nil {
			return err
		}
		o, err := packOptions(out)
		if err != nil {
			return err
		}
		if name == "" {
			avail, lerr := packsrc.List(cmd.Context(), s)
			if lerr != nil {
				return lerr
			}
			var names []string
			for _, a := range avail {
				fmt.Fprintf(out, "  %-12s %-8s %s\n", a.Name, a.Version, a.Description)
				names = append(names, a.Name)
			}
			return fmt.Errorf("name the pack to install: %s", strings.Join(names, ", "))
		}
		res, err := packsrc.Install(cmd.Context(), s, name, o)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "installed %s %s into %s\n", res.Pack, res.Version, res.Dir)
		if res.Commit != "" {
			fmt.Fprintf(out, "  from %s at %s\n", s.Spec, short(res.Commit))
		}
		fmt.Fprintf(out, "  commit it so the project carries its own pack setup\n")
		return nil
	},
}

var packsUpdateCmd = &cobra.Command{
	Use:   "update <name>",
	Short: "Re-fetch an installed pack at the ref it was installed from",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		o, err := packOptions(out)
		if err != nil {
			return err
		}
		res, err := packsrc.Update(cmd.Context(), args[0], o)
		if err != nil {
			return err
		}
		switch {
		case res.Previous == res.Commit:
			fmt.Fprintf(out, "%s is already at %s\n", res.Pack, short(res.Commit))
		default:
			fmt.Fprintf(out, "updated %s %s: %s → %s\n", res.Pack, res.Version, short(res.Previous), short(res.Commit))
		}
		return nil
	},
}

var packsRemoveCmd = &cobra.Command{
	Use:   "remove <name>",
	Short: "Delete a pack from this project and forget its approval",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		out := cmd.OutOrStdout()
		o, err := packOptions(out)
		if err != nil {
			return err
		}
		if err := packsrc.Remove(args[0], o); err != nil {
			return err
		}
		fmt.Fprintf(out, "removed %s from %s\n", args[0], packsrc.PacksDir(o.Workspace))
		return nil
	},
}

func short(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}
```

Register them and the flag:

```go
	packsAddCmd.Flags().StringVar(&flagPackSource, "source", "", "a directory or git URL to fetch from (default: packs.source)")
	packsAddCmd.Flags().Bool("force", false, "replace a pack that is already installed")
	packsCmd.AddCommand(packsAddCmd, packsUpdateCmd, packsRemoveCmd)
```

Wire `--force` into the options in `packsAddCmd`'s `RunE`, after `packOptions`:

```go
		o.Force, _ = cmd.Flags().GetBool("force")
```

Add the imports this needs to `cmd/packs.go`: `bufio`, `io`, `path/filepath`,
`internal/config`, `internal/engine`, `internal/packsrc`.

**No new module dependency.** `golang.org/x/term` is *not* in `go.mod`; the
repo's own TTY test is `stdinIsTTY` (`cmd/root.go:115`), which is a `var` so
tests can stub it, and `packsApprover` above uses it.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./cmd -run TestPacksAdd -v`
Expected: PASS, 3 tests.

- [ ] **Step 5: Install a real pack end to end, from a real clone**

This is the first time the whole path runs. Do it against the public packs
repository, in a scratch workspace, and read the output.

```bash
make -f build.mk build
ws=$(mktemp -d) && git -C "$ws" init -q .
./be-code packs add assist -C "$ws" -y
ls -la "$ws/.becode/packs/assist" && cat "$ws/.becode/packs/assist/.pack-source.json"
./be-code packs validate "$ws/.becode/packs/assist"
./be-code packs update assist -C "$ws" -y
./be-code packs remove assist -C "$ws" -y
ls "$ws/.becode/packs" 2>&1
```

Expected: the pack installs, the marker names `assist` with a 40-character
commit, `validate` prints `ok`, `update` reports it is already at that commit,
and `remove` leaves an empty or missing `packs` directory. If the clone is
slow or fails, the network is the cause, not the code — say so rather than
working around it.

- [ ] **Step 6: Commit**

```bash
git add cmd/packs.go cmd/packs_add_test.go
git commit -m "cmd: be-code packs add, update and remove

add takes a name and an optional @ref, falls back to packs.source when no
--source is given, and with no name lists what the source offers rather than
guessing. A non-interactive run refuses without -y, and -y here approves the
one command the person typed."
```

---

### Task 9: The session surface — slash commands and the menu

**Files:**
- Modify: `internal/ui/packs.go` (`PacksCommand`, `IsPacksBlocking`)
- Modify: `internal/ui/common.go:87` (the `/packs` row)
- Modify: `internal/tui/palette.go:242` (`menuEntries`)
- Create: `internal/ui/packs_install_test.go`

**Interfaces:**
- Consumes: `packsrc.Install`, `packsrc.Update`, `packsrc.Remove`,
  `packsrc.Resolve`, `packsrc.Options` (Tasks 2–4); `agent.Agent`'s
  `Tools.Root`, `Tools.Approve`, `LoadPacks`.
- Produces: `/packs add|update|remove`, handled inside `ui.PacksCommand`.

- [ ] **Step 1: Write the failing test**

Create `internal/ui/packs_install_test.go`:

```go
package ui

import (
	"strings"
	"testing"
)

// Every subcommand that can raise a question must be blocking: Session.Ask
// relocks the mutex Update holds, and an inline question froze every attached
// terminal with the alt screen up.
func TestInstallingSubcommandsAreBlocking(t *testing.T) {
	for _, verb := range []string{"add", "update", "remove"} {
		if !IsPacksBlocking([]string{verb}) {
			t.Errorf("/packs %s must be blocking: it can raise pack_install", verb)
		}
	}
	if IsPacksBlocking(nil) {
		t.Error("a bare /packs listing must stay inline")
	}
}

func TestPacksUsageNamesTheNewVerbs(t *testing.T) {
	// The usage string is what a person sees after a typo, so it has to list
	// everything that exists.
	usage := packsUsage()
	for _, want := range []string{"add", "update", "remove", "on", "off", "default", "reload"} {
		if !strings.Contains(usage, want) {
			t.Errorf("usage does not mention %q: %s", want, usage)
		}
	}
}

func TestSlashTableMentionsTheNewVerbs(t *testing.T) {
	var row string
	for _, c := range SlashCommandTable {
		if c.Name == "/packs" {
			row = c.Desc // SlashCommandInfo{Name, Desc, Args} — internal/ui/common.go:56
		}
	}
	if row == "" {
		t.Fatal("no /packs row in SlashCommandTable")
	}
	for _, want := range []string{"add", "remove"} {
		if !strings.Contains(row, want) {
			t.Errorf("the /packs row does not mention %q: %s", want, row)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/ui -run 'TestInstallingSubcommands|TestPacksUsage|TestSlashTableMentions' -v`
Expected: `TestInstallingSubcommandsAreBlocking` FAILS (the three verbs are
not in `IsPacksBlocking`), `TestPacksUsageNamesTheNewVerbs` FAILS to build
(`undefined: packsUsage`), `TestSlashTableMentionsTheNewVerbs` FAILS (the row
mentions neither).

- [ ] **Step 3: Extend the session command**

In `internal/ui/packs.go`:

Add the verbs to `IsPacksBlocking`'s switch:

```go
	case "reload", "on", "off", "default", "clear", "auto", "add", "update", "remove":
		return true
```

Extract the usage string so one place defines it:

```go
func packsUsage() string {
	return "usage: /packs · /packs add <name>[@<ref>] · /packs update <name> · /packs remove <name> · " +
		"/packs on <name> · /packs off <name> · /packs default <name> · /packs reload"
}
```

Replace both existing `"usage: /packs · …"` literals in `PacksCommand` with
`packsUsage()`, and add the three verbs to its `switch`:

```go
	case "add":
		return packsAdd(ag, fields[2:])
	case "update", "remove":
		return packsManage(ag, verb, fields[2])
```

Then the two helpers, in the same file:

```go
// packOptionsFor is the agent's own workspace, trust file and approval seam.
// The approver is Tools.Approve, so an install asks through whichever UI is
// running — and a nil one is a refusal, which packsrc enforces.
func packOptionsFor(ag *agent.Agent) (packsrc.Options, error) {
	base, err := config.Dir()
	if err != nil {
		return packsrc.Options{}, err
	}
	root := ag.Tools.Root
	return packsrc.Options{
		Workspace: root,
		TrustPath: filepath.Join(base, "engine", engine.Key(root), "packs.json"),
		Approve:   ag.Tools.Approve,
	}, nil
}

func packsAdd(ag *agent.Agent, args []string) string {
	if len(args) == 0 {
		return packsUsage()
	}
	name, ref, _ := strings.Cut(args[0], "@")
	spec := ""
	if len(args) > 1 {
		spec = args[1]
	}
	if spec == "" && ag.Cfg != nil {
		spec = ag.Cfg.Packs.Source
	}
	s, err := packsrc.Resolve(spec, ref)
	if err != nil {
		return err.Error()
	}
	o, err := packOptionsFor(ag)
	if err != nil {
		return err.Error()
	}
	res, err := packsrc.Install(context.Background(), s, name, o)
	if err != nil {
		return err.Error()
	}
	ag.LoadPacks()
	return fmt.Sprintf("installed %s %s into %s\n  commit it so the project carries its own pack setup",
		res.Pack, res.Version, res.Dir)
}

func packsManage(ag *agent.Agent, verb, name string) string {
	o, err := packOptionsFor(ag)
	if err != nil {
		return err.Error()
	}
	if verb == "remove" {
		if err := packsrc.Remove(name, o); err != nil {
			return err.Error()
		}
		ag.LoadPacks()
		return "removed " + name
	}
	res, err := packsrc.Update(context.Background(), name, o)
	if err != nil {
		return err.Error()
	}
	ag.LoadPacks()
	return fmt.Sprintf("updated %s %s", res.Pack, res.Version)
}
```

Add the imports: `context`, `path/filepath`, `internal/config`,
`internal/engine`, `internal/packsrc`.

- [ ] **Step 4: Update the slash table row**

In `internal/ui/common.go` (line 87):

```go
	{"/packs", "list installed packs and which are active; /packs add|update|remove <name> · /packs on|off|default <name> · /packs reload", false},
```

- [ ] **Step 5: Add the menu row**

In `internal/tui/palette.go`, in `menuEntries` beside the other rows:

```go
		{"Packs", "Install a skill pack", "fetch a pack from the packs repository into this project", cmd("/packs add")},
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/ui ./internal/tui -run 'TestInstallingSubcommands|TestPacksUsage|TestSlashTableMentions|TestMenu' -v`
Expected: PASS. The TUI package has a test that every menu entry's command is
a real slash command; if it names this row, the `/packs add` spelling is
wrong.

- [ ] **Step 7: Run the whole suite**

Run: `make -f build.mk verify`
Expected: vet clean, build ok, every package ok.

- [ ] **Step 8: Commit**

```bash
git add internal/ui/packs.go internal/ui/common.go internal/ui/packs_install_test.go internal/tui/palette.go
git commit -m "ui: /packs add, update and remove, and a menu row

All three are blocking subcommands, because each can raise pack_install and
Session.Ask relocks the mutex Update holds. One packsUsage() so the usage
string, the palette and the menu cannot drift apart."
```

---

## Notes for whoever executes this

- **Run `make -f build.mk verify` before calling any task done.** `go vet` is
  the only linter, and the `verify` package's own tests invoke the real Go
  toolchain, so they need `go` on PATH.
- **Never let a test write to `~/.be-code`.** `internal/tui` once wrote the
  real `config.json` from a theme test. Pin `HOME` with `t.Setenv` in any test
  that could reach `config.Dir()`, and `stat -c %Y ~/.be-code/config.json`
  before and after a run to be sure.
- **The `internal/agent` package is the slowest in the repo** and its tests
  are sensitive to CPU contention: a timeout there under load is usually
  contention, not a bug. Re-run it alone before believing a failure.
- Task 5's test file assumes `newTestAgent` and a scripted provider helper
  exist in `internal/agent`'s tests. Read `internal/agent/agent_test.go` for
  the exact names before writing it; the provider is never called.
