# Chat and Messaging Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A per-session chat room (`/chat`, with `@agent` reaching the session's model) and a machine-wide inbox of DMs (`/inbox`, `/dm`), both inside the existing served TUI, with identity resolved from each terminal's config name, IP and PID.

**Architecture:** Chat is a `Room` on `tui.Session`, broadcast to views through the existing mailbox and saved in the session file. DMs are one JSON file each under `~/.be-code/inbox/<user>/`, written atomically by whichever session host sends and noticed by a poller in every session host (`internal/inbox`). Identity is `~/.be-code/users.json`, read-modify-written under a file lock. The `Hello` frame carries IP, login, PID and config name so the host knows who each terminal is.

**Tech Stack:** Go 1.22, Bubble Tea/lipgloss (existing), no new module dependencies.

**Spec:** `docs/superpowers/specs/2026-09-21-chat-and-messaging-design.md`

## Global Constraints

- BE-Code version becomes `0.15.0` (`build.mk` `VERSION`, CHANGELOG entry, README status line) in the final task; every task before it leaves the version alone.
- **No new module dependency.** The spec names fsnotify; ruling: the watcher is a 1 s poll of directory mtimes only (an offline-first tool must build from an existing module cache). Recorded as a deviation; the spec's "2 s poll fallback" becomes the only path at 1 s.
- Every persisted file under `~/.be-code` is written atomically (temp file in the same directory, then rename), files 0600, directories 0700. Tests use a temp HOME, never the real one; run `stat -c '%y' ~/.be-code/config.json` before and after a test run and confirm it is unchanged.
- User IDs match `^[a-z0-9_.-]{1,32}$` after lower-casing; `agent` is reserved.
- The `@agent` match is exactly the regexp `(?i)(^|[^\w.@])@agent\b`.
- `@agent` in a DM is plain text: nothing in `internal/inbox` may import `internal/agent`, and no DM text ever reaches `Agent.Enqueue`/`RunFull`.
- `chat`, `inbox` and `dm` are view modes of one terminal: entering them never changes `Session.running`, never touches another view, and `Esc` on an empty input or `/back` returns to `modeInput`/`modeBusy` as the run state dictates (`m.idleMode()`).
- Every child process (the `arp -a` lookup) goes through `internal/procattr.Hide`.
- Commit messages end with the two attribution trailers used throughout this repository.
- Run `make -f build.mk verify` (from `be-code/`) before every commit; it must pass.

---

## File structure

| Path | Responsibility |
|---|---|
| `internal/inbox/inbox.go` | `Message`, `ThreadSummary`, `Dir`, `Send`, `Thread`, `Threads`, `MarkRead`, `ValidID`; atomic file writes |
| `internal/inbox/watch.go` | `Watch(ctx, dir, onNew)`: 1 s poll of user directories for new message files |
| `internal/inbox/users.go` | `users.json`: `Users`, `Load`, `Bind`, `Resolve`, `Resolution` |
| `internal/inbox/lock_unix.go`, `lock_windows.go` | `withLock(path, fn)` — flock / LockFileEx |
| `internal/inbox/mac.go`, `mac_unix.go`, `mac_windows.go` | `macFor(ip)`: parse `/proc/net/arp` or `arp -a` |
| `internal/live/frame.go`, `client.go`, `host.go`, `record.go` | `Hello`/`ClientInfo` gain `IP`, `Login`, `PID`, `User`; `Record` gains `Users` |
| `internal/store/sessions.go` | `Session.Chat []ChatLine`, `ChatLine` |
| `internal/agent/session.go` (new) | `Agent.UpdateSession(fn)` |
| `internal/config/config.go` | `ChatConfig{Enabled, MentionContext, Name}` |
| `internal/tui/chat.go` | the room on `Session`: `Post`, join/leave, cap, `chatMsg`; `modeChat` keys and view |
| `internal/tui/identity.go` | per-client user IDs on `Session`, the naming prompt, `/whoami`, `/clients` column |
| `internal/tui/dm.go` | `modeInbox`, `modeDM`: views, keys, unread counts, `inboxMsg`, watcher wiring |
| `internal/tui/mention.go` | `@agent`: match, request text, queueing, reply capture |
| `internal/ui/common.go`, `internal/ui/repl.go` | slash table entries; plain-mode refusal |
| `README.md`, `CHANGELOG.md`, `build.mk`, `docs/live-checklist.md` | docs and version |

---

### Task 1: The mailbox — `internal/inbox` core

**Files:**
- Create: `internal/inbox/inbox.go`
- Test: `internal/inbox/inbox_test.go`

**Interfaces:**
- Produces:
  ```go
  type Message struct { From, To string; TS time.Time; Text string }
  type ThreadSummary struct { With string; Latest Message; Unread int }
  func Dir() (string, error)                      // ~/.be-code/inbox, created 0700
  func ValidID(id string) (string, error)         // lower-cases; error names the rule
  func Send(dir, from, to, text string) (Message, error)
  func Thread(dir, me, other string) ([]Message, error)   // sorted by TS
  func Threads(dir, me string) ([]ThreadSummary, error)   // newest first
  func MarkRead(dir, me string, upTo time.Time) error
  func LastRead(dir, me string) time.Time
  func writeAtomic(path string, data []byte, mode os.FileMode) error
  ```

- [ ] **Step 1: Write the failing tests**

```go
package inbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidID(t *testing.T) {
	for _, ok := range []string{"alice", "Bob", "a.b-c_9", strings.Repeat("x", 32)} {
		if _, err := ValidID(ok); err != nil {
			t.Errorf("%q rejected: %v", ok, err)
		}
	}
	if id, _ := ValidID("Bob"); id != "bob" {
		t.Fatalf("not lower-cased: %q", id)
	}
	for _, bad := range []string{"", "agent", "Agent", "has space", "über", strings.Repeat("x", 33), "a/b"} {
		if _, err := ValidID(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestSendThreadAndThreads(t *testing.T) {
	dir := t.TempDir()
	if _, err := Send(dir, "alice", "bob", "hi bob"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := Send(dir, "bob", "alice", "hi alice"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := Send(dir, "carol", "alice", "lunch?"); err != nil {
		t.Fatal(err)
	}
	th, err := Thread(dir, "alice", "bob")
	if err != nil || len(th) != 2 || th[0].Text != "hi bob" || th[1].Text != "hi alice" || th[1].From != "bob" {
		t.Fatalf("thread: %+v %v", th, err)
	}
	// Both sides see the same thread.
	th2, _ := Thread(dir, "bob", "alice")
	if len(th2) != 2 || th2[0].Text != "hi bob" {
		t.Fatalf("bob's view: %+v", th2)
	}
	ts, err := Threads(dir, "alice")
	if err != nil || len(ts) != 2 || ts[0].With != "carol" || ts[1].With != "bob" {
		t.Fatalf("threads newest first: %+v %v", ts, err)
	}
	if ts[0].Unread != 1 || ts[1].Unread != 1 {
		t.Fatalf("unread: %+v", ts)
	}
	if ts[0].Latest.Text != "lunch?" {
		t.Fatalf("latest: %+v", ts[0].Latest)
	}
	// Reading up to carol's message clears both (bob's is older).
	if err := MarkRead(dir, "alice", ts[0].Latest.TS); err != nil {
		t.Fatal(err)
	}
	ts, _ = Threads(dir, "alice")
	if ts[0].Unread != 0 || ts[1].Unread != 0 {
		t.Fatalf("still unread after MarkRead: %+v", ts)
	}
	// Only received messages count as unread, never one's own.
	Send(dir, "alice", "bob", "one more from me")
	ts, _ = Threads(dir, "alice")
	if ts[0].With != "bob" || ts[0].Unread != 0 {
		t.Fatalf("own message counted as unread: %+v", ts)
	}
}

func TestFilesAreAtomicPrivateAndNamedByTime(t *testing.T) {
	dir := t.TempDir()
	m, err := Send(dir, "alice", "bob", "x")
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "bob"))
	if len(entries) != 1 {
		t.Fatalf("expected one file, got %d", len(entries))
	}
	name := entries[0].Name()
	if !strings.HasSuffix(name, "-alice.json") || strings.Contains(name, ".tmp") {
		t.Fatalf("name %q", name)
	}
	info, _ := entries[0].Info()
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", info.Mode())
	}
	dinfo, _ := os.Stat(filepath.Join(dir, "bob"))
	if dinfo.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", dinfo.Mode())
	}
	if m.TS.IsZero() || m.From != "alice" || m.To != "bob" {
		t.Fatalf("message %+v", m)
	}
}

func TestSendToAnUnknownIDCreatesTheInbox(t *testing.T) {
	dir := t.TempDir()
	if _, err := Send(dir, "alice", "nobody-yet", "waiting for you"); err != nil {
		t.Fatal(err)
	}
	th, _ := Thread(dir, "nobody-yet", "alice")
	if len(th) != 1 {
		t.Fatalf("thread: %+v", th)
	}
}

func TestSendRejectsBadIDsAndEmptyText(t *testing.T) {
	dir := t.TempDir()
	if _, err := Send(dir, "alice", "agent", "hi"); err == nil {
		t.Fatal("sent to the reserved id")
	}
	if _, err := Send(dir, "alice", "bob", "   "); err == nil {
		t.Fatal("sent empty text")
	}
	if _, err := Send(dir, "a b", "bob", "hi"); err == nil {
		t.Fatal("sent from a bad id")
	}
}

func TestACorruptMessageFileIsSkipped(t *testing.T) {
	dir := t.TempDir()
	Send(dir, "alice", "bob", "good")
	os.WriteFile(filepath.Join(dir, "bob", "1-alice.json"), []byte("{not json"), 0o600)
	th, err := Thread(dir, "bob", "alice")
	if err != nil || len(th) != 1 || th[0].Text != "good" {
		t.Fatalf("thread %+v %v", th, err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd be-code && go test ./internal/inbox/ 2>&1 | head`
Expected: build failure, `undefined: Send` etc.

- [ ] **Step 3: Implement**

```go
// Package inbox is the machine-wide mailbox behind /dm and /inbox: one JSON
// file per message under ~/.be-code/inbox/<user>/, written atomically by
// whichever session host sends and read by any. There is no daemon: every
// session host polls the directory (watch.go). It knows nothing about the
// agent — a DM never reaches the model — and nothing about the TUI.
package inbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
)

// Message is one DM as stored: <inbox>/<To>/<unix-nanos>-<From>.json.
type Message struct {
	From string    `json:"from"`
	To   string    `json:"to"`
	TS   time.Time `json:"ts"`
	Text string    `json:"text"`
}

// ThreadSummary is one row of /inbox.
type ThreadSummary struct {
	With   string
	Latest Message
	Unread int
}

// Reserved is the ID no person may claim.
const Reserved = "agent"

var idRe = regexp.MustCompile(`^[a-z0-9_.-]{1,32}$`)

// ValidID lower-cases and checks a user ID.
func ValidID(id string) (string, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == Reserved {
		return "", fmt.Errorf("%q is reserved for the model", id)
	}
	if !idRe.MatchString(id) {
		return "", errors.New("a name is 1–32 of a-z, 0-9, '.', '-' or '_'")
	}
	return id, nil
}

// Dir is ~/.be-code/inbox, created 0700.
func Dir() (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(base, "inbox")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}

// Send writes text from one user to another. The recipient's directory is
// created if it does not exist: an inbox exists before its owner has ever
// attached, so a message to a name not yet claimed waits for whoever claims
// it.
func Send(dir, from, to, text string) (Message, error) {
	var err error
	if from, err = ValidID(from); err != nil {
		return Message{}, fmt.Errorf("from: %w", err)
	}
	if to, err = ValidID(to); err != nil {
		return Message{}, fmt.Errorf("to: %w", err)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return Message{}, errors.New("empty message")
	}
	m := Message{From: from, To: to, TS: time.Now(), Text: text}
	udir := filepath.Join(dir, to)
	if err := os.MkdirAll(udir, 0o700); err != nil {
		return Message{}, err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return Message{}, err
	}
	name := fmt.Sprintf("%d-%s.json", m.TS.UnixNano(), from)
	if err := writeAtomic(filepath.Join(udir, name), b, 0o600); err != nil {
		return Message{}, err
	}
	return m, nil
}

// Thread is every message between me and other, oldest first: what I
// received from them plus what I sent them.
func Thread(dir, me, other string) ([]Message, error) {
	var out []Message
	for _, side := range [][2]string{{me, other}, {other, me}} {
		msgs, err := received(dir, side[0], side[1])
		if err != nil {
			return nil, err
		}
		out = append(out, msgs...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS.Before(out[j].TS) })
	return out, nil
}

// received lists messages in owner's directory from sender ("" = any).
func received(dir, owner, sender string) ([]Message, error) {
	entries, err := os.ReadDir(filepath.Join(dir, owner))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []Message
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || name == "read.json" || strings.HasPrefix(name, ".tmp") {
			continue
		}
		if sender != "" && !strings.HasSuffix(name, "-"+sender+".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, owner, name))
		if err != nil {
			continue
		}
		var m Message
		if json.Unmarshal(b, &m) != nil || m.From == "" {
			continue // a corrupt file is skipped, never fatal
		}
		out = append(out, m)
	}
	return out, nil
}

// Threads is one summary per correspondent of me, newest first.
func Threads(dir, me string) ([]ThreadSummary, error) {
	in, err := received(dir, me, "")
	if err != nil {
		return nil, err
	}
	last := LastRead(dir, me)
	byWith := map[string]*ThreadSummary{}
	note := func(with string, m Message, unread bool) {
		s := byWith[with]
		if s == nil {
			s = &ThreadSummary{With: with}
			byWith[with] = s
		}
		if m.TS.After(s.Latest.TS) {
			s.Latest = m
		}
		if unread {
			s.Unread++
		}
	}
	for _, m := range in {
		note(m.From, m, m.TS.After(last))
	}
	// What I sent: my messages live in the recipients' directories.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.IsDir() || e.Name() == me {
			continue
		}
		sent, _ := received(dir, e.Name(), me)
		for _, m := range sent {
			note(m.To, m, false)
		}
	}
	out := make([]ThreadSummary, 0, len(byWith))
	for _, s := range byWith {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Latest.TS.After(out[j].Latest.TS) })
	return out, nil
}

type readState struct {
	LastReadTS time.Time `json:"last_read_ts"`
}

// MarkRead records that me has read everything at or before upTo. Never
// moves backwards.
func MarkRead(dir, me string, upTo time.Time) error {
	if !upTo.After(LastRead(dir, me)) {
		return nil
	}
	udir := filepath.Join(dir, me)
	if err := os.MkdirAll(udir, 0o700); err != nil {
		return err
	}
	b, _ := json.Marshal(readState{LastReadTS: upTo})
	return writeAtomic(filepath.Join(udir, "read.json"), b, 0o600)
}

// LastRead is the read mark, zero when none.
func LastRead(dir, me string) time.Time {
	b, err := os.ReadFile(filepath.Join(dir, me, "read.json"))
	if err != nil {
		return time.Time{}
	}
	var st readState
	if json.Unmarshal(b, &st) != nil {
		return time.Time{}
	}
	return st.LastReadTS
}

// writeAtomic writes data to a temp file beside path and renames it over.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := filepath.Join(filepath.Dir(path), ".tmp-"+strconv.FormatInt(time.Now().UnixNano(), 36)+"-"+strconv.Itoa(os.Getpid()))
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/inbox/ -v 2>&1 | tail -12`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/inbox/inbox.go internal/inbox/inbox_test.go
git commit -m "feat(inbox): the shared mailbox — send, thread, threads, read mark"
```

---

### Task 2: The watcher — `internal/inbox/watch.go`

**Files:**
- Create: `internal/inbox/watch.go`
- Test: `internal/inbox/watch_test.go`

**Interfaces:**
- Produces: `func Watch(ctx context.Context, dir string, every time.Duration, onNew func(m Message))` — blocks until ctx ends; calls `onNew` for every message file that appears after the watch started (any user directory), in file-name order; never for files present at start; `every <= 0` means 1 s.

- [ ] **Step 1: Write the failing test**

```go
package inbox

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestWatchDeliversNewMessagesOnlyOnce(t *testing.T) {
	dir := t.TempDir()
	Send(dir, "alice", "bob", "before the watch") // must never be delivered
	var mu sync.Mutex
	var got []Message
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Watch(ctx, dir, 20*time.Millisecond, func(m Message) {
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	})
	time.Sleep(60 * time.Millisecond)
	Send(dir, "carol", "bob", "one")
	time.Sleep(2 * time.Millisecond)
	Send(dir, "alice", "dave", "two") // a user directory that did not exist at start
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	time.Sleep(60 * time.Millisecond) // a second poll must not redeliver
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0].Text != "one" || got[1].Text != "two" || got[1].To != "dave" {
		t.Fatalf("got %+v", got)
	}
}

func TestWatchStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Watch(ctx, t.TempDir(), 10*time.Millisecond, func(Message) {}); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not return")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/inbox/ -run TestWatch 2>&1 | head -3`
Expected: `undefined: Watch`.

- [ ] **Step 3: Implement**

```go
package inbox

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Watch polls dir and calls onNew for every message file that appears after
// the watch began, in name order (names begin with the send time). A poll
// rather than inotify: it costs one ReadDir of the root and of each user
// directory whose mtime moved, once a second, and needs no dependency an
// offline install might lack. Files present when the watch starts are the
// inbox's history, not news, and are never delivered.
func Watch(ctx context.Context, dir string, every time.Duration, onNew func(m Message)) {
	if every <= 0 {
		every = time.Second
	}
	seen := map[string]bool{} // "<user>/<file>"
	mtimes := map[string]time.Time{}
	scan := func(deliver bool) {
		users, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		var fresh []string
		for _, u := range users {
			if !u.IsDir() {
				continue
			}
			udir := filepath.Join(dir, u.Name())
			info, err := os.Stat(udir)
			if err != nil {
				continue
			}
			if deliver && info.ModTime().Equal(mtimes[u.Name()]) {
				continue // nothing written here since the last poll
			}
			mtimes[u.Name()] = info.ModTime()
			files, err := os.ReadDir(udir)
			if err != nil {
				continue
			}
			for _, f := range files {
				name := f.Name()
				if f.IsDir() || !strings.HasSuffix(name, ".json") || name == "read.json" || strings.HasPrefix(name, ".tmp") {
					continue
				}
				key := u.Name() + "/" + name
				if seen[key] {
					continue
				}
				seen[key] = true
				if deliver {
					fresh = append(fresh, key)
				}
			}
		}
		sort.Strings(fresh)
		for _, key := range fresh {
			b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(key)))
			if err != nil {
				continue
			}
			var m Message
			if json.Unmarshal(b, &m) != nil || m.From == "" {
				continue
			}
			onNew(m)
		}
	}
	scan(false) // baseline: everything already there is history
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			scan(true)
		}
	}
}
```

Note the mtime shortcut: a directory's mtime changes when a file is added or renamed into it (the rename in `writeAtomic` counts), so a quiet directory costs one `Stat`. The first `scan(true)` after start re-reads every directory because `mtimes` was only filled by the baseline for directories that existed then; that is fine.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/inbox/ -race 2>&1 | tail -3`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/inbox/watch.go internal/inbox/watch_test.go
git commit -m "feat(inbox): poll the mailbox for new messages"
```

---

### Task 3: Identity — `users.json`, the lock, and `Resolve`

**Files:**
- Create: `internal/inbox/users.go`, `internal/inbox/lock_unix.go`, `internal/inbox/lock_windows.go`
- Test: `internal/inbox/users_test.go`

**Interfaces:**
- Produces:
  ```go
  type Device struct { IP, Login, MAC string; FirstSeen, LastSeen time.Time }
  type Users struct { Users map[string]*UserRecord }      // loaded form
  type UserRecord struct { Devices []Device }
  type Terminal struct { IP, Login, User string; PID int } // what the host knows
  type Resolution struct { ID string; How string; Choices []string; Ask bool }
  func UsersPath() (string, error)                        // ~/.be-code/users.json
  func Resolve(path string, t Terminal, lookupMAC func(ip string) string) (Resolution, error)
  func Bind(path, id string, t Terminal, mac string) error
  func withLock(path string, fn func() error) error       // lock_*.go
  ```
  `How` is one of `"config"`, `"ip"`, `"mac"`, `"asked"`, `""` (when `Ask`).

- [ ] **Step 1: Write the failing tests**

```go
package inbox

import (
	"path/filepath"
	"sync"
	"testing"
)

func usersFile(t *testing.T) string { return filepath.Join(t.TempDir(), "users.json") }

func TestResolveConfigNameWinsAndBindsTheIP(t *testing.T) {
	p := usersFile(t)
	r, err := Resolve(p, Terminal{IP: "192.168.1.38", Login: "sbrown", User: "Alice", PID: 1}, nil)
	if err != nil || r.ID != "alice" || r.How != "config" || r.Ask {
		t.Fatalf("%+v %v", r, err)
	}
	// A later terminal from the same IP with no config name is offered alice.
	r, _ = Resolve(p, Terminal{IP: "192.168.1.38", Login: "sbrown", PID: 2}, nil)
	if r.ID != "alice" || r.How != "ip" || r.Ask {
		t.Fatalf("%+v", r)
	}
}

func TestResolveUnknownIPAsksAndBindKeepsIt(t *testing.T) {
	p := usersFile(t)
	r, _ := Resolve(p, Terminal{IP: "10.0.0.5", Login: "x", PID: 3}, nil)
	if !r.Ask || r.ID != "" || len(r.Choices) != 0 {
		t.Fatalf("%+v", r)
	}
	if err := Bind(p, "bob", Terminal{IP: "10.0.0.5", Login: "x", PID: 3}, ""); err != nil {
		t.Fatal(err)
	}
	r, _ = Resolve(p, Terminal{IP: "10.0.0.5", Login: "x", PID: 4}, nil)
	if r.ID != "bob" || r.How != "ip" {
		t.Fatalf("%+v", r)
	}
}

func TestResolveSeveralIDsOnOneIPOffersChoices(t *testing.T) {
	p := usersFile(t)
	tm := Terminal{IP: "10.0.0.9", Login: "shared"}
	Bind(p, "alice", tm, "")
	Bind(p, "bob", tm, "")
	r, _ := Resolve(p, tm, nil)
	if !r.Ask || len(r.Choices) != 2 || r.Choices[0] != "alice" || r.Choices[1] != "bob" {
		t.Fatalf("%+v", r)
	}
}

func TestResolveFallsBackToMACOnlyForAnUnknownIP(t *testing.T) {
	p := usersFile(t)
	Bind(p, "alice", Terminal{IP: "192.168.1.38", Login: "s"}, "aa:bb:cc:dd:ee:ff")
	calls := 0
	lookup := func(ip string) string {
		calls++
		if ip == "192.168.1.77" {
			return "AA:BB:CC:DD:EE:FF" // case must not matter
		}
		return ""
	}
	// Known IP: MAC is never consulted.
	if r, _ := Resolve(p, Terminal{IP: "192.168.1.38"}, lookup); r.ID != "alice" || calls != 0 {
		t.Fatalf("%+v calls=%d", r, calls)
	}
	// New IP, same device: recognised, and the new IP is bound too.
	r, _ := Resolve(p, Terminal{IP: "192.168.1.77", Login: "s"}, lookup)
	if r.ID != "alice" || r.How != "mac" || calls != 1 {
		t.Fatalf("%+v calls=%d", r, calls)
	}
	if r, _ := Resolve(p, Terminal{IP: "192.168.1.77"}, lookup); r.How != "ip" {
		t.Fatalf("the new IP was not bound: %+v", r)
	}
}

func TestBindRejectsBadIDs(t *testing.T) {
	p := usersFile(t)
	if err := Bind(p, "agent", Terminal{IP: "1.2.3.4"}, ""); err == nil {
		t.Fatal("bound the reserved id")
	}
}

func TestConcurrentBindsDoNotLoseEachOther(t *testing.T) {
	p := usersFile(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			Bind(p, "u"+string(rune('a'+i)), Terminal{IP: "10.1.1.1"}, "")
		}(i)
	}
	wg.Wait()
	u, err := Load(p)
	if err != nil || len(u.Users) != 20 {
		t.Fatalf("%d users, %v", len(u.Users), err)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/inbox/ -run 'TestResolve|TestBind|TestConcurrent' 2>&1 | head -3`
Expected: `undefined: Resolve`.

- [ ] **Step 3: Implement**

`internal/inbox/users.go`:

```go
package inbox

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
)

// Device is one place a user has been seen from.
type Device struct {
	IP        string    `json:"ip"`
	Login     string    `json:"login,omitempty"`
	MAC       string    `json:"mac,omitempty"`
	FirstSeen time.Time `json:"first_seen"`
	LastSeen  time.Time `json:"last_seen"`
}

type UserRecord struct {
	Devices []Device `json:"devices"`
}

// Users is ~/.be-code/users.json: which devices may use which IDs.
type Users struct {
	Users map[string]*UserRecord `json:"users"`
}

// Terminal is what the session host knows about an attached terminal.
type Terminal struct {
	IP    string
	Login string
	User  string // chat.name from the terminal's own config, may be ""
	PID   int
}

// Resolution is Resolve's answer. Ask means the terminal must be prompted:
// with Choices when several IDs are bound to its IP, with none when it is
// new.
type Resolution struct {
	ID      string
	How     string // "config" | "ip" | "mac" | "asked" | ""
	Choices []string
	Ask     bool
}

// UsersPath is ~/.be-code/users.json.
func UsersPath() (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "users.json"), nil
}

// Load reads the file; a missing file is an empty record.
func Load(path string) (*Users, error) {
	u := &Users{Users: map[string]*UserRecord{}}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return u, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, u); err != nil {
		return nil, err
	}
	if u.Users == nil {
		u.Users = map[string]*UserRecord{}
	}
	return u, nil
}

func save(path string, u *Users) error {
	b, err := json.MarshalIndent(u, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, b, 0o600)
}

// idsForIP lists the IDs with a device at ip, sorted.
func (u *Users) idsForIP(ip string) []string {
	var out []string
	for id, r := range u.Users {
		for _, d := range r.Devices {
			if d.IP == ip {
				out = append(out, id)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

func (u *Users) idForMAC(mac string) string {
	mac = strings.ToLower(mac)
	if mac == "" {
		return ""
	}
	for id, r := range u.Users {
		for _, d := range r.Devices {
			if strings.ToLower(d.MAC) == mac {
				return id
			}
		}
	}
	return ""
}

// touch records the terminal under id: a device row for its IP is updated
// or added. Caller holds the lock.
func (u *Users) touch(id string, t Terminal, mac string) {
	r := u.Users[id]
	if r == nil {
		r = &UserRecord{}
		u.Users[id] = r
	}
	now := time.Now()
	for i := range r.Devices {
		if r.Devices[i].IP == t.IP {
			r.Devices[i].LastSeen = now
			if t.Login != "" {
				r.Devices[i].Login = t.Login
			}
			if mac != "" {
				r.Devices[i].MAC = strings.ToLower(mac)
			}
			return
		}
	}
	r.Devices = append(r.Devices, Device{IP: t.IP, Login: t.Login, MAC: strings.ToLower(mac), FirstSeen: now, LastSeen: now})
}

// Bind records id as usable from the terminal's IP (the answer to a prompt,
// or the config name). mac may be "".
func Bind(path, id string, t Terminal, mac string) error {
	id, err := ValidID(id)
	if err != nil {
		return err
	}
	return withLock(path, func() error {
		u, err := Load(path)
		if err != nil {
			return err
		}
		u.touch(id, t, mac)
		return save(path, u)
	})
}

// Resolve decides who a terminal is (spec §3.3): the config name; else the
// one ID bound to its IP, or a choice among several; else — and only then —
// a MAC lookup for a device whose IP changed; else a prompt. lookupMAC may
// be nil. A config name or a MAC match binds the IP on the way.
func Resolve(path string, t Terminal, lookupMAC func(ip string) string) (Resolution, error) {
	if t.User != "" {
		id, err := ValidID(t.User)
		if err != nil {
			return Resolution{Ask: true}, err
		}
		if err := Bind(path, id, t, ""); err != nil {
			return Resolution{}, err
		}
		return Resolution{ID: id, How: "config"}, nil
	}
	u, err := Load(path)
	if err != nil {
		return Resolution{}, err
	}
	switch ids := u.idsForIP(t.IP); len(ids) {
	case 1:
		_ = Bind(path, ids[0], t, "") // last_seen
		return Resolution{ID: ids[0], How: "ip"}, nil
	case 0:
	default:
		return Resolution{Ask: true, Choices: ids}, nil
	}
	if lookupMAC != nil {
		if mac := lookupMAC(t.IP); mac != "" {
			if id := u.idForMAC(mac); id != "" {
				if err := Bind(path, id, t, mac); err != nil {
					return Resolution{}, err
				}
				return Resolution{ID: id, How: "mac"}, nil
			}
		}
	}
	return Resolution{Ask: true}, nil
}
```

`internal/inbox/lock_unix.go`:

```go
//go:build !windows

package inbox

import (
	"os"
	"syscall"
)

// withLock runs fn holding an exclusive flock on path+".lock": two session
// hosts binding IDs at once must not overwrite each other's write.
func withLock(path string, fn func() error) error {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return fn()
}
```

`internal/inbox/lock_windows.go`:

```go
//go:build windows

package inbox

import (
	"os"

	"golang.org/x/sys/windows"
)

func withLock(path string, fn func() error) error {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	h := windows.Handle(f.Fd())
	ol := new(windows.Overlapped)
	if err := windows.LockFileEx(h, windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, ol); err != nil {
		return err
	}
	defer windows.UnlockFileEx(h, 0, 1, 0, ol)
	return fn()
}
```

`golang.org/x/sys` is already an indirect dependency (check `go.mod`; if it is only indirect, `go mod tidy` promotes it — no new module is downloaded).

- [ ] **Step 4: Run the tests, including a Windows compile**

Run: `go test ./internal/inbox/ -race 2>&1 | tail -3 && GOOS=windows GOARCH=amd64 go vet ./internal/inbox/`
Expected: PASS; the vet prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/inbox/users.go internal/inbox/lock_unix.go internal/inbox/lock_windows.go internal/inbox/users_test.go go.mod go.sum
git commit -m "feat(inbox): users.json — bind IDs to devices, resolve a terminal"
```

---

### Task 4: MAC lookup, best effort

**Files:**
- Create: `internal/inbox/mac.go`, `internal/inbox/mac_linux.go`, `internal/inbox/mac_other.go`
- Test: `internal/inbox/mac_test.go`

**Interfaces:**
- Produces: `func MACFor(ip string) string` (the `lookupMAC` for `Resolve`; "" on any miss), and the pure parsers `parseProcArp(text, ip string) string`, `parseArpA(text, ip string) string`.

- [ ] **Step 1: Write the failing test**

```go
package inbox

import "testing"

const procArp = `IP address       HW type     Flags       HW address            Mask     Device
192.168.1.38     0x1         0x2         aa:bb:cc:dd:ee:ff     *        eth0
192.168.1.1      0x1         0x2         11:22:33:44:55:66     *        eth0
192.168.1.99     0x1         0x0         00:00:00:00:00:00     *        eth0
`

const arpA = `Interface: 192.168.1.10 --- 0x5
  Internet Address      Physical Address      Type
  192.168.1.38          aa-bb-cc-dd-ee-ff     dynamic
  192.168.1.1           11-22-33-44-55-66     dynamic
? (192.168.1.40) at aa:bb:cc:00:11:22 on en0 ifscope [ethernet]
`

func TestParseProcArp(t *testing.T) {
	if got := parseProcArp(procArp, "192.168.1.38"); got != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("got %q", got)
	}
	if got := parseProcArp(procArp, "192.168.1.99"); got != "" {
		t.Fatalf("an incomplete entry (flags 0x0, zero MAC) must be a miss, got %q", got)
	}
	if got := parseProcArp(procArp, "10.0.0.1"); got != "" {
		t.Fatalf("miss returned %q", got)
	}
}

func TestParseArpA(t *testing.T) {
	if got := parseArpA(arpA, "192.168.1.38"); got != "aa:bb:cc:dd:ee:ff" {
		t.Fatalf("windows form: got %q", got)
	}
	if got := parseArpA(arpA, "192.168.1.40"); got != "aa:bb:cc:00:11:22" {
		t.Fatalf("macOS form: got %q", got)
	}
	if got := parseArpA(arpA, "1.1.1.1"); got != "" {
		t.Fatalf("miss returned %q", got)
	}
}

func TestMACForLoopbackIsAMiss(t *testing.T) {
	if got := MACFor("127.0.0.1"); got != "" {
		t.Fatalf("got %q", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/inbox/ -run 'TestParse|TestMACFor' 2>&1 | head -3`
Expected: `undefined: parseProcArp`.

- [ ] **Step 3: Implement**

`internal/inbox/mac.go`:

```go
package inbox

import (
	"regexp"
	"strings"
)

// MACFor is the hardware address the ARP table holds for ip, or "". Best
// effort by design (spec §3.4): only a device on this subnet appears, and it
// is consulted only for an IP nobody has bound. Never for loopback.
func MACFor(ip string) string {
	if ip == "" || strings.HasPrefix(ip, "127.") || ip == "::1" {
		return ""
	}
	return macFor(ip)
}

var macRe = regexp.MustCompile(`(?i)\b([0-9a-f]{2})[:-]([0-9a-f]{2})[:-]([0-9a-f]{2})[:-]([0-9a-f]{2})[:-]([0-9a-f]{2})[:-]([0-9a-f]{2})\b`)

func normMAC(s string) string {
	m := macRe.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	mac := strings.ToLower(strings.Join(m[1:], ":"))
	if mac == "00:00:00:00:00:00" {
		return ""
	}
	return mac
}

// parseProcArp reads Linux's /proc/net/arp: columns IP, HW type, flags,
// HW address; flags 0x0 is an incomplete entry.
func parseProcArp(text, ip string) string {
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[0] != ip {
			continue
		}
		if f[2] == "0x0" {
			return ""
		}
		return normMAC(f[3])
	}
	return ""
}

// parseArpA reads `arp -a` on Windows ("192.168.1.38  aa-bb-cc-dd-ee-ff
// dynamic") and macOS ("? (192.168.1.40) at aa:bb:cc:00:11:22 on en0").
func parseArpA(text, ip string) string {
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, ip) {
			continue
		}
		fields := strings.Fields(line)
		hit := false
		for _, f := range fields {
			if f == ip || f == "("+ip+")" {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		return normMAC(line)
	}
	return ""
}
```

`internal/inbox/mac_linux.go`:

```go
//go:build linux

package inbox

import "os"

func macFor(ip string) string {
	b, err := os.ReadFile("/proc/net/arp")
	if err != nil {
		return ""
	}
	return parseProcArp(string(b), ip)
}
```

`internal/inbox/mac_other.go`:

```go
//go:build !linux

package inbox

import (
	"context"
	"os/exec"
	"time"

	"github.com/brown-enterprises/be-code/internal/procattr"
)

func macFor(ip string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "arp", "-a")
	procattr.Hide(cmd)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return parseArpA(string(out), ip)
}
```

- [ ] **Step 4: Run the tests and the procattr guard**

Run: `go test ./internal/inbox/ ./internal/procattr/ 2>&1 | tail -3 && GOOS=windows go vet ./internal/inbox/ && GOOS=darwin go vet ./internal/inbox/`
Expected: PASS (the procattr source-scan test sees `procattr.Hide` in `mac_other.go`).

- [ ] **Step 5: Commit**

```bash
git add internal/inbox/mac.go internal/inbox/mac_linux.go internal/inbox/mac_other.go internal/inbox/mac_test.go
git commit -m "feat(inbox): best-effort MAC lookup from the ARP table"
```

---

### Task 5: The wire — `Hello`/`ClientInfo` carry IP, login, PID, user; the live record lists users

**Files:**
- Modify: `internal/live/frame.go` (`Hello`, `ClientInfo`), `internal/live/client.go` (`AttachOptions`, `DefaultAttachOptions`, the `Hello` write at ~line 270), `internal/live/host.go` (`client` struct, `newClient`, `infosLocked`), `internal/live/record.go` (`Record`)
- Test: `internal/live/frame_test.go` (new), `internal/live/record_test.go` (extend)

**Interfaces:**
- Produces:
  ```go
  // frame.go
  type Hello struct { …; IP, Login, User string `json:",omitempty"`; PID int `json:",omitempty"` }
  type ClientInfo struct { …; IP, Login, User string; PID int }
  // client.go
  type AttachOptions struct { …; IP, Login, User string; PID int }
  func ClientIP() string   // first field of SSH_CONNECTION, else "127.0.0.1"
  func LoginName() string  // $USER, else os/user, else ""
  // record.go
  type Record struct { …; Users []string `json:"users,omitempty"` }
  func (r Record) WithUsers(ids []string) Record
  ```
  `DefaultAttachOptions` fills IP/Login/PID; `User` is set by `cmd` from `cfg.Chat.Name` (Task 7).

- [ ] **Step 1: Write the failing tests**

`internal/live/frame_test.go`:

```go
package live

import (
	"encoding/json"
	"testing"
)

// An old client's Hello has none of the identity fields and must still
// decode; a new one carries them.
func TestHelloIdentityFieldsAreOptional(t *testing.T) {
	var h Hello
	if err := json.Unmarshal([]byte(`{"token":"t","cols":80,"rows":24,"label":"ssh from 1.2.3.4 (pid 9)"}`), &h); err != nil {
		t.Fatal(err)
	}
	if h.IP != "" || h.PID != 0 {
		t.Fatalf("%+v", h)
	}
	b, _ := json.Marshal(Hello{Token: "t", IP: "1.2.3.4", Login: "sb", PID: 9, User: "alice"})
	var back Hello
	json.Unmarshal(b, &back)
	if back.IP != "1.2.3.4" || back.Login != "sb" || back.PID != 9 || back.User != "alice" {
		t.Fatalf("%+v", back)
	}
}

func TestClientIPFromSSHConnection(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "192.168.1.38 51234 192.168.1.237 22")
	if got := ClientIP(); got != "192.168.1.38" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("SSH_CONNECTION", "")
	if got := ClientIP(); got != "127.0.0.1" {
		t.Fatalf("local: got %q", got)
	}
}

func TestClientInfoCarriesIdentity(t *testing.T) {
	c := newClient(Hello{Label: "x", IP: "1.2.3.4", Login: "sb", PID: 9, User: "alice"}, nil)
	h := &Host{clients: map[int]*client{1: c}}
	c.id = 1
	infos := h.infosLocked()
	if len(infos) != 1 || infos[0].IP != "1.2.3.4" || infos[0].Login != "sb" || infos[0].PID != 9 || infos[0].User != "alice" {
		t.Fatalf("%+v", infos)
	}
}
```

Check `Host.clients`'s type before writing the last test (`grep -n "clients " internal/live/host.go`); if it is a slice, build the host accordingly. Append to `internal/live/record_test.go`:

```go
func TestRecordWithUsersRoundTrips(t *testing.T) {
	dir := t.TempDir()
	r := Record{Code: "ABCDEF", PID: 1, Socket: "s", Workspace: "w", Model: "m", Token: "t"}.WithUsers([]string{"alice", "bob"})
	if err := r.Save(dir); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir, "ABCDEF")
	if err != nil || len(got.Users) != 2 || got.Users[1] != "bob" {
		t.Fatalf("%+v %v", got, err)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/live/ -run 'TestHello|TestClientIP|TestClientInfoCarries|TestRecordWithUsers' 2>&1 | head -5`
Expected: build errors for the missing fields and functions.

- [ ] **Step 3: Implement**

In `frame.go`, add to `Hello` after `Control`:

```go
	// Who this terminal is, for chat and DMs (spec §3.1). All optional: an
	// older client sends none, and the host treats it as local/unknown.
	IP    string `json:"ip,omitempty"`
	Login string `json:"login,omitempty"`
	PID   int    `json:"pid,omitempty"`
	User  string `json:"user,omitempty"` // chat.name from the client's config
```

and to `ClientInfo`:

```go
	IP    string `json:"ip,omitempty"`
	Login string `json:"login,omitempty"`
	PID   int    `json:"pid,omitempty"`
	User  string `json:"user,omitempty"`
```

In `client.go`: add `IP, Login, User string; PID int` to `AttachOptions`; in `DefaultAttachOptions` set `IP: ClientIP(), Login: LoginName(), PID: os.Getpid()`; in the `Hello` write use `Hello{Token: rec.Token, Cols: cols, Rows: rows, Label: opt.Label, UTF8: opt.UTF8, IP: opt.IP, Login: opt.Login, PID: opt.PID, User: opt.User}`; and add:

```go
// ClientIP is where this terminal's connection comes from, as the session
// host will record it: the client end of SSH_CONNECTION, else loopback.
func ClientIP() string {
	if sc := os.Getenv("SSH_CONNECTION"); sc != "" {
		if f := strings.Fields(sc); len(f) > 0 {
			return f[0]
		}
	}
	return "127.0.0.1"
}

// LoginName is the OS user running this terminal.
func LoginName() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}
```

(import `os/user`). In `host.go`: add `ip, login, userID string; pid int` to `client`; in `newClient` copy `hello.IP`, `hello.Login`, `hello.PID`, `hello.User` (the user through `sanitizeLabel` too — it is displayed); in `infosLocked` add `IP: c.ip, Login: c.login, PID: c.pid, User: c.userID`. In `record.go`:

```go
	// Users are the chat IDs attached to this host right now, so another
	// host can tell whether a DM's recipient is online anywhere (spec §5.2).
	Users []string `json:"users,omitempty"`
```

```go
// WithUsers is r with its attached user IDs replaced.
func (r Record) WithUsers(ids []string) Record {
	r.Users = append([]string(nil), ids...)
	return r
}
```

- [ ] **Step 4: Run the package tests**

Run: `go test ./internal/live/ -race 2>&1 | tail -3`
Expected: PASS (the two known flaky tests may need a rerun; a failure that repeats three times is real).

- [ ] **Step 5: Commit**

```bash
git add internal/live
git commit -m "feat(live): a terminal's Hello says who it is; live records list attached users"
```

---

### Task 6: Storage and config — `Session.Chat`, `Agent.UpdateSession`, `ChatConfig`

**Files:**
- Modify: `internal/store/sessions.go` (`Session`), `internal/config/config.go` (`Config`, `Default`), `README.md` (config reference)
- Create: `internal/agent/session.go`
- Test: `internal/store/sessions_test.go` (extend), `internal/agent/session_test.go` (new), `internal/config/config_test.go` (extend)

**Interfaces:**
- Produces:
  ```go
  // store
  type ChatLine struct { TS time.Time `json:"ts"`; User string `json:"user"`; Text string `json:"text"`; Kind string `json:"kind,omitempty"` }
  // Session gains: Chat []ChatLine `json:"chat,omitempty"`
  // agent
  func (a *Agent) UpdateSession(fn func(s *store.Session))  // fn runs under sessionMu; no-op with no session
  // config
  type ChatConfig struct { Enabled bool `json:"enabled"`; MentionContext int `json:"mention_context"`; Name string `json:"name,omitempty"` }
  // Config gains: Chat ChatConfig `json:"chat"`; Default: {Enabled: true, MentionContext: 10}
  ```

- [ ] **Step 1: Write the failing tests**

Append to `internal/store/sessions_test.go` (use the file's existing helpers for a temp store; the shape below assumes `NewStore(dir)`/`Save`/`Load` — read the file and match its names):

```go
func TestChatRoundTripsAndIsAbsentWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	s := &Session{ID: "20260921-000000-001", Title: "t", Chat: []ChatLine{{TS: time.Unix(1, 0), User: "alice", Text: "hi", Kind: ""}}}
	if err := SaveTo(dir, s); err != nil { // adapt to the store's save function
		t.Fatal(err)
	}
	got, err := LoadFrom(dir, s.ID)
	if err != nil || len(got.Chat) != 1 || got.Chat[0].User != "alice" {
		t.Fatalf("%+v %v", got, err)
	}
	b, _ := json.Marshal(&Session{ID: "x"})
	if strings.Contains(string(b), `"chat"`) {
		t.Fatalf("empty chat serialised: %s", b)
	}
}
```

`internal/agent/session_test.go`:

```go
package agent

import (
	"testing"

	"github.com/brown-enterprises/be-code/internal/store"
)

func TestUpdateSessionRunsUnderTheLockAndSurvivesNoSession(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, nil)
	ran := false
	ag.UpdateSession(func(*store.Session) { ran = true })
	if ran {
		t.Fatal("fn ran with no session")
	}
	ag.SetSession(&store.Session{ID: "s"})
	ag.UpdateSession(func(s *store.Session) { s.Chat = append(s.Chat, store.ChatLine{User: "alice", Text: "hi"}) })
	if len(ag.Session.Chat) != 1 {
		t.Fatalf("%+v", ag.Session.Chat)
	}
}
```

Append to `internal/config/config_test.go`:

```go
func TestChatConfigDefaults(t *testing.T) {
	c := Default()
	if !c.Chat.Enabled || c.Chat.MentionContext != 10 || c.Chat.Name != "" {
		t.Fatalf("%+v", c.Chat)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/store/ ./internal/agent/ ./internal/config/ 2>&1 | grep -v "^compaction" | head -5`
Expected: build failures naming `ChatLine`, `UpdateSession`, `Chat`.

- [ ] **Step 3: Implement**

`internal/store/sessions.go`, after `HostPID`:

```go
	// Chat is the session's chat room (/chat), newest last, capped by the
	// TUI at 2000 lines. Absent for a session that never used it.
	Chat []ChatLine `json:"chat,omitempty"`
```

```go
// ChatLine is one line of the room. User is an ID, "agent", or "" for a
// system line; Kind is "", "join", "leave", "mention" or "reply".
type ChatLine struct {
	TS   time.Time `json:"ts"`
	User string    `json:"user"`
	Text string    `json:"text"`
	Kind string    `json:"kind,omitempty"`
}
```

`internal/agent/session.go`:

```go
package agent

import "github.com/brown-enterprises/be-code/internal/store"

// UpdateSession runs fn on the current session under the session lock, so a
// UI can keep its own part of the session file (the chat room) beside the
// transcript the agent saves. Nothing happens without a session.
func (a *Agent) UpdateSession(fn func(s *store.Session)) {
	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()
	if a.Session != nil {
		fn(a.Session)
	}
}
```

`internal/config/config.go`: after the `IDE IDEConfig` field add `Chat ChatConfig \`json:"chat"\``; define:

```go
// ChatConfig controls /chat, /inbox and /dm (spec §8).
type ChatConfig struct {
	Enabled bool `json:"enabled"`
	// MentionContext is how many recent room lines go to the model with an
	// @agent mention; 0 sends the mention alone.
	MentionContext int `json:"mention_context"`
	// Name is this device's user ID. Empty means the host offers the IDs
	// seen from this IP, or asks once.
	Name string `json:"name,omitempty"`
}
```

and in `Default()`: `Chat: ChatConfig{Enabled: true, MentionContext: 10},`. README config reference, after the `ide` block:

```
- `chat.enabled` (true) — `/chat`, `/inbox` and `/dm`; `chat.mention_context` (10) — room lines sent with an `@agent` mention; `chat.name` — this device's user ID for chat and DMs (unset: the host offers the IDs seen from your IP, or asks once).
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/store/ ./internal/agent/ ./internal/config/ 2>&1 | grep -v "^compaction" | tail -4`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/store internal/agent/session.go internal/agent/session_test.go internal/config README.md
git commit -m "feat: chat room in the session file, chat config, Agent.UpdateSession"
```

---

### Task 7: The room — `Session.Post`, `modeChat`, `/chat`, `/back`

**Files:**
- Create: `internal/tui/chat.go`
- Modify: `internal/tui/session.go` (fields; `SetClients` for leave lines; `Resume`/restore), `internal/tui/view.go` (`mode` consts, `handleKey`, `update` message switch, `View()`, `bottomLine`, `slashCommand`), `internal/ui/common.go` (`SlashCommandTable`), `cmd/live.go` (`AttachOptions.User = cfg.Chat.Name`)
- Test: `internal/tui/chat_test.go`

**Interfaces:**
- Consumes: `store.ChatLine`, `Agent.UpdateSession` (Task 6); `ClientInfo.User` (Task 5).
- Produces:
  ```go
  // on Session
  func (s *Session) Post(user, text, kind string)       // takes mu; appends, caps at 2000, mirrors to the store, broadcasts chatMsg
  func (s *Session) PostLocked(user, text, kind string)
  func (s *Session) Room() []store.ChatLine             // snapshot
  func (s *Session) restoreRoom(lines []store.ChatLine) // called from Resume
  type chatMsg struct{ line store.ChatLine }
  // on View
  const modeChat mode
  func (m *View) enterChat() (tea.Model, tea.Cmd)
  func (m *View) leaveMode() (tea.Model, tea.Cmd)       // back to idleMode(); used by /back and Esc in every new mode
  func (m *View) handleChatKey(k tea.KeyMsg) (tea.Model, tea.Cmd)
  func (m *View) viewChat() string
  chatUnseen int  // on View: lines posted since this view last looked
  ```
  `m.userID()` (Task 8) is not yet available: in this task the poster's name is `m.chatName()`, defined here as `ClientInfo.User` if set, else `live.LabelKey(label)`; Task 8 replaces its body.

- [ ] **Step 1: Write the failing tests**

```go
package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/brown-enterprises/be-code/internal/store"
)

func TestPostBroadcastsToEveryViewAndMirrorsToTheStore(t *testing.T) {
	s := newTestSession(t)
	s.ag.SetSession(&store.Session{ID: "s"})
	a, b := s.NewView(1, "vscode (pid 1)"), s.NewView(2, "ssh from 192.168.1.38 (pid 2)")
	for _, v := range []*View{a, b} {
		v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	}
	s.Post("alice", "hello room", "")
	drainAll(t, a, b)
	if len(a.room) != 1 || len(b.room) != 1 || b.room[0].Text != "hello room" {
		t.Fatalf("views: %+v / %+v", a.room, b.room)
	}
	if got := s.ag.Session.Chat; len(got) != 1 || got[0].User != "alice" {
		t.Fatalf("store: %+v", got)
	}
	if a.chatUnseen != 1 || b.chatUnseen != 1 {
		t.Fatalf("unseen: %d %d", a.chatUnseen, b.chatUnseen)
	}
	if !strings.Contains(a.bottomLine(), "chat (1 new)") {
		t.Fatalf("bottom line: %q", a.bottomLine())
	}
}

func TestChatModeLeavesTheRunStateAloneAndEscReturns(t *testing.T) {
	s := newTestSession(t)
	v := s.NewView(1, "local")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	s.mu.Lock()
	s.setRunStateLocked(true, "thinking")
	s.mu.Unlock()
	drainAll(t, v)
	v.slashCommand("/chat")
	if v.mode != modeChat || !s.running || v.chatUnseen != 0 {
		t.Fatalf("mode %v running %v unseen %d", v.mode, s.running, v.chatUnseen)
	}
	out := v.View()
	if !strings.Contains(out, "chat ·") || !strings.Contains(out, "Esc back") {
		t.Fatalf("chat view:\n%s", out)
	}
	v.input.SetValue("hi there")
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	drainAll(t, v)
	if r := s.Room(); len(r) != 2 || r[1].Text != "hi there" || r[0].Kind != "join" { // join line, then the post
		t.Fatalf("room %+v", r)
	}
	v.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if v.mode != modeBusy || !s.running {
		t.Fatalf("after Esc: mode %v running %v", v.mode, s.running)
	}
}

func TestSlashCommandsWorkInsideChat(t *testing.T) {
	s := newTestSession(t)
	v := s.NewView(1, "local")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	v.slashCommand("/chat")
	v.input.SetValue("/back")
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if v.mode != modeInput {
		t.Fatalf("mode %v", v.mode)
	}
	if r := s.Room(); len(r) != 1 || r[0].Kind != "join" {
		t.Fatalf("/back was posted as text: %+v", r)
	}
}

func TestRoomIsCappedAndRestored(t *testing.T) {
	s := newTestSession(t)
	s.ag.SetSession(&store.Session{ID: "s"})
	for i := 0; i < roomCap+5; i++ {
		s.Post("alice", "line", "")
	}
	r := s.Room()
	if len(r) != roomCap || r[0].Text != "(older chat trimmed)" {
		t.Fatalf("len %d first %+v", len(r), r[0])
	}
	s2 := newTestSession(t)
	s2.restoreRoom(r)
	if len(s2.Room()) != roomCap {
		t.Fatal("restore lost lines")
	}
}
```

`drainAll(t, views...)` — if `helpers_test.go` has no such helper, add one that calls `v.Update(drainMsg{})` on each view until its mailbox is empty (see how existing tests deliver broadcasts; `mailbox.drainInto` is what `Update` calls).

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/tui/ -run 'TestPost|TestChatMode|TestSlashCommandsWorkInsideChat|TestRoomIs' 2>&1 | head -5`
Expected: build failures (`room`, `chatUnseen`, `modeChat`, `Post`).

- [ ] **Step 3: Implement**

`internal/tui/chat.go`:

```go
package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brown-enterprises/be-code/internal/live"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/ui"
)

// The room: one per session, every attached terminal sees it, routed by the
// host through the same mailbox the transcript uses. It is not the
// transcript — the model sees it only through an @agent mention
// (mention.go) — and it is saved in the session file beside the transcript.

// roomCap bounds the room; the oldest lines go, once, with a marker.
const roomCap = 2000

// chatMsg is one new room line, broadcast to every view.
type chatMsg struct{ line store.ChatLine }

// Post appends a line to the room and tells every terminal.
func (s *Session) Post(user, text, kind string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.PostLocked(user, text, kind)
}

// PostLocked is Post for a caller holding mu.
func (s *Session) PostLocked(user, text, kind string) {
	line := store.ChatLine{TS: s.now(), User: user, Text: strings.TrimRight(text, "\n"), Kind: kind}
	s.room = append(s.room, line)
	if len(s.room) > roomCap {
		s.room = append([]store.ChatLine{{TS: line.TS, Text: "(older chat trimmed)"}}, s.room[len(s.room)-roomCap+1:]...)
	}
	room := s.room
	s.ag.UpdateSession(func(ss *store.Session) { ss.Chat = append([]store.ChatLine(nil), room...) })
	s.broadcast(chatMsg{line: line})
}

// Room is a snapshot of the room.
func (s *Session) Room() []store.ChatLine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]store.ChatLine(nil), s.room...)
}

// restoreRoom puts a saved room back (resume).
func (s *Session) restoreRoom(lines []store.ChatLine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.room = append([]store.ChatLine(nil), lines...)
}

// chatName is how this terminal signs a room line. Task 8 replaces the
// body with the resolved user ID.
func (m *View) chatName() string {
	for _, c := range m.clients {
		if c.ID == m.id && c.User != "" {
			return c.User
		}
	}
	return live.LabelKey(m.label)
}

// enterChat opens the room on this terminal. Nothing shared changes: the run
// keeps running, the other terminals keep whatever they were doing.
func (m *View) enterChat() (tea.Model, tea.Cmd) {
	if !m.cfg.Chat.Enabled {
		m.appendEntryLocked(entry{Kind: entryDim, Text: "chat is disabled in config (chat.enabled)"})
		return m, nil
	}
	if !m.joinedChat {
		m.joinedChat = true
		m.PostLocked("", m.chatName()+" joined", "join")
	}
	m.mode = modeChat
	m.chatUnseen = 0
	m.input.Reset()
	m.input.Placeholder = "message the room… (/back to return)"
	m.layoutChat()
	m.chatVP.GotoBottom()
	return m, nil
}

// leaveMode returns this terminal from chat, inbox or dm to the transcript.
func (m *View) leaveMode() (tea.Model, tea.Cmd) {
	m.mode = m.idleMode()
	m.input.Reset()
	m.input.Placeholder = inputPlaceholder
	return m, nil
}

func (m *View) handleChatKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.Type {
	case tea.KeyEsc:
		if m.input.Value() == "" {
			return m.leaveMode()
		}
		m.input.Reset()
		return m, nil
	case tea.KeyEnter:
		text := strings.TrimSpace(m.input.Value())
		m.input.Reset()
		if text == "" {
			return m, nil
		}
		if strings.HasPrefix(text, "/") {
			return m.slashCommand(text)
		}
		m.PostLocked(m.chatName(), text, "")
		return m, nil
	case tea.KeyPgUp:
		m.chatVP.HalfViewUp()
		return m, nil
	case tea.KeyPgDown:
		m.chatVP.HalfViewDown()
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return m, cmd
}

// layoutChat sizes the room viewport: everything above the input rows and
// the footer.
func (m *View) layoutChat() {
	h := m.height - m.inputRows() - 2
	if h < 3 {
		h = 3
	}
	m.chatVP.Width, m.chatVP.Height = m.width, h
	m.chatVP.SetContent(m.renderRoom())
}

func (m *View) renderRoom() string {
	var b strings.Builder
	for _, l := range m.room {
		b.WriteString(m.renderChatLine(l))
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (m *View) renderChatLine(l store.ChatLine) string {
	ts := m.st.Dim.Render(l.TS.Format("15:04"))
	switch {
	case l.User == "":
		return ts + " " + m.st.ChatSystem.Render(l.Text)
	case l.User == "agent":
		return ts + " " + m.st.Assistant.Render("agent: ") + wrapTo(l.Text, m.width-15)
	default:
		return ts + " " + m.st.ChatUser.Render(l.User+": ") + wrapTo(l.Text, m.width-8-lipgloss.Width(l.User))
	}
}

func (m *View) viewChat() string {
	here := 0
	for range m.clients {
		here++
	}
	if here == 0 {
		here = 1
	}
	footer := fmt.Sprintf(" chat · %d here · Esc back", here)
	if m.compact() {
		footer = " chat · Esc"
	}
	if m.mentionBusy != "" {
		footer += m.st.Dim.Render(" · agent is working on " + m.mentionBusy + "'s question")
	}
	return m.chatVP.View() + "\n" + m.inputView() + "\n" + m.st.Dim.Render(footer)
}
```

Helpers this refers to and how to satisfy them: `m.inputRows()` and `m.inputView()` — if `view.go` builds the input rows inline in `View()`, extract those lines into these two methods (they must render the same textarea and its prompt as the transcript layout does); `inputPlaceholder` — the constant the transcript input uses (find the string `describe a task…` in `view.go` and name it); `wrapTo(text, width)` — the package's existing wrap helper (`grep -n "func wrap" internal/tui/*.go`; name accordingly); `m.st.ChatUser`, `m.st.ChatSystem` — add to `styles` and `Palette` in `theme.go` (every theme row: `ChatUser` = the theme's `Accent` colour, `ChatSystem` = its `Dim`); `m.mentionBusy string` — a View field, set in Task 10, declare it now.

`view.go`:
- `mode` consts: add `modeChat`, `modeInbox`, `modeDM` after `modeQueue`.
- `View` fields: `room []store.ChatLine`, `chatVP viewport.Model`, `chatUnseen int`, `joinedChat bool`, `mentionBusy string`.
- `NewView` (in `session.go`): `v.room = append([]store.ChatLine(nil), s.room...)` under the session lock it already holds.
- `handleKey`: `case modeChat: return m.handleChatKey(k)`.
- `update` message switch: 
  ```go
  case chatMsg:
      m.room = append(m.room, msg.line)
      if len(m.room) > roomCap {
          m.room = m.room[len(m.room)-roomCap:]
      }
      if m.mode == modeChat {
          m.layoutChat()
          m.chatVP.GotoBottom()
      } else if msg.line.Kind != "join" && msg.line.Kind != "leave" {
          m.chatUnseen++
      }
      return m, nil
  ```
- `tea.WindowSizeMsg` handling: after the existing layout, `if m.mode == modeChat { m.layoutChat() }`.
- `View()`: `case modeChat: return m.viewChat()` beside `modePicker`.
- `bottomLine()`: after the clients segment, `if m.chatUnseen > 0 && m.mode != modeChat { line += m.st.Accent.Render(fmt.Sprintf(" · chat (%d new)", m.chatUnseen)) }`.
- `slashCommand`: `case "/chat": return m.enterChat()` and `case "/back": if m.mode == modeChat || m.mode == modeInbox || m.mode == modeDM { return m.leaveMode() }; return m, nil`.
- `Session.SetClients`: where a detached client is handled, `if s.joined[c.ID] { s.PostLocked("", s.chatNameOf(c)+" left", "leave"); delete(s.joined, c.ID) }` — add `joined map[int]bool` to `Session`, set in `enterChat` (`m.joined[m.id] = true`, under mu, which `Update` holds) and `chatNameOf(c live.ClientInfo)` = `c.User` else `live.LabelKey(c.Label)`.
- `Session` restore: where `cmd/live.go`/`cmd/root.go` calls `ag.Resume(sess)` and builds the TUI session, call `s.restoreRoom(sess.Chat)`; and `/clear` (`ClearHistory` path in the TUI) calls `s.room = nil` under mu plus `s.ag.UpdateSession(func(ss){ ss.Chat = nil })`.

`internal/ui/common.go`: add rows `{"/chat", "the session's chat room (@agent to ask the model)", true}`, `{"/back", "return to the transcript", true}` to `SlashCommandTable`, and both names to the busy-safe set. `cmd/live.go` and the local attach path: where `live.DefaultAttachOptions()` is built, set `opt.User = cfg.Chat.Name`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/tui/ ./internal/ui/ -race 2>&1 | tail -4`
Expected: PASS, including every existing TUI test (the new modes must not change any existing render).

- [ ] **Step 5: Commit**

```bash
git add internal/tui internal/ui/common.go cmd/live.go cmd/root.go
git commit -m "feat(tui): the chat room — /chat, /back, posts broadcast to every terminal"
```

---

### Task 8: Identity in the TUI — naming prompt, `/whoami`, `/clients` column

**Files:**
- Create: `internal/tui/identity.go`
- Modify: `internal/tui/session.go` (`SetClients` resolves), `internal/tui/chat.go` (`chatName` body), `internal/tui/view.go` (`/clients`, `/whoami`), `internal/ui/common.go`
- Test: `internal/tui/identity_test.go`

**Interfaces:**
- Consumes: `inbox.Resolve`, `inbox.Bind`, `inbox.MACFor`, `inbox.UsersPath` (Tasks 3–4); `ClientInfo.IP/Login/PID/User` (Task 5).
- Produces:
  ```go
  // on Session
  ids      map[int]identity            // per client id
  usersPath string                     // "" in tests that want no file → resolution is "asked" in memory only
  resolveFn func(inbox.Terminal) (inbox.Resolution, error) // test seam; nil = inbox.Resolve with inbox.MACFor
  type identity struct{ ID, How string; Choices []string }
  func (s *Session) identityOf(client int) identity      // caller holds mu
  func (s *Session) bindLocked(client int, id string) error
  // on View
  func (m *View) userID() string                          // "" when unresolved
  func (m *View) needName(then func(*View) (tea.Model, tea.Cmd)) (tea.Model, tea.Cmd) // prompt if needed, else run then
  ```

- [ ] **Step 1: Write the failing tests**

```go
package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/brown-enterprises/be-code/internal/inbox"
	"github.com/brown-enterprises/be-code/internal/live"
)

func TestConfigNameResolvesOnAttach(t *testing.T) {
	s := newTestSession(t)
	s.resolveFn = func(tm inbox.Terminal) (inbox.Resolution, error) {
		if tm.User == "alice" {
			return inbox.Resolution{ID: "alice", How: "config"}, nil
		}
		return inbox.Resolution{Ask: true}, nil
	}
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "vscode (pid 1)", IP: "192.168.1.38", User: "alice"}, {ID: 2, Label: "ssh from 192.168.1.40 (pid 2)", IP: "192.168.1.40"}})
	v1, v2 := s.NewView(1, "vscode (pid 1)"), s.NewView(2, "ssh from 192.168.1.40 (pid 2)")
	if v1.userID() != "alice" || v2.userID() != "" {
		t.Fatalf("%q %q", v1.userID(), v2.userID())
	}
	v1.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	v1.slashCommand("/whoami")
	if !strings.Contains(lastEntryText(s), "you are alice (from config)") {
		t.Fatalf("%q", lastEntryText(s))
	}
	v1.slashCommand("/clients")
	if !strings.Contains(lastEntryText(s), "alice") {
		t.Fatalf("/clients lacks the ID: %q", lastEntryText(s))
	}
}

func TestUnknownTerminalIsAskedOnceThenBound(t *testing.T) {
	s := newTestSession(t)
	bound := ""
	s.resolveFn = func(tm inbox.Terminal) (inbox.Resolution, error) { return inbox.Resolution{Ask: true}, nil }
	s.bindFn = func(id string, tm inbox.Terminal) error { bound = id; return nil }
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "ssh from 10.0.0.5 (pid 1)", IP: "10.0.0.5"}})
	v := s.NewView(1, "ssh from 10.0.0.5 (pid 1)")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	v.slashCommand("/chat")
	if v.mode != modeName {
		t.Fatalf("expected the naming prompt, mode %v", v.mode)
	}
	v.input.SetValue("Bob")
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if bound != "bob" || v.userID() != "bob" || v.mode != modeChat {
		t.Fatalf("bound %q id %q mode %v", bound, v.userID(), v.mode)
	}
	// A bad name re-prompts with the reason.
	v2 := s.NewView(1, "x")
	s.mu.Lock()
	delete(s.ids, 1)
	s.mu.Unlock()
	v2.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	v2.slashCommand("/chat")
	v2.input.SetValue("agent")
	v2.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if v2.mode != modeName || !strings.Contains(v2.View(), "reserved") {
		t.Fatalf("mode %v view %q", v2.mode, v2.View())
	}
}

func TestChoicesAreOfferedForASharedIP(t *testing.T) {
	s := newTestSession(t)
	s.resolveFn = func(tm inbox.Terminal) (inbox.Resolution, error) {
		return inbox.Resolution{Ask: true, Choices: []string{"alice", "bob"}}, nil
	}
	s.bindFn = func(string, inbox.Terminal) error { return nil }
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "l", IP: "10.0.0.9"}})
	v := s.NewView(1, "l")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	v.slashCommand("/chat")
	out := v.View()
	if v.mode != modeName || !strings.Contains(out, "alice") || !strings.Contains(out, "bob") {
		t.Fatalf("mode %v:\n%s", v.mode, out)
	}
	v.Update(tea.KeyMsg{Type: tea.KeyDown}) // bob
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if v.userID() != "bob" {
		t.Fatalf("id %q", v.userID())
	}
}
```

`lastEntryText(s)` — the text of the newest transcript entry; add to `helpers_test.go` if absent.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/tui/ -run 'TestConfigName|TestUnknownTerminal|TestChoicesAre' 2>&1 | head -5`
Expected: build failures (`resolveFn`, `modeName`, `userID`).

- [ ] **Step 3: Implement**

`internal/tui/identity.go`:

```go
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/inbox"
	"github.com/brown-enterprises/be-code/internal/live"
)

// Who each terminal is, for chat and DMs (spec §3). The session resolves an
// identity when a terminal attaches; a terminal that needs a prompt gets it
// the first time it opens /chat, /inbox or /dm, never before.

type identity struct {
	ID      string
	How     string
	Choices []string
}

// terminalOf is what the host told us about a client.
func terminalOf(c live.ClientInfo) inbox.Terminal {
	return inbox.Terminal{IP: c.IP, Login: c.Login, User: c.User, PID: c.PID}
}

// resolveClientLocked fills s.ids[c.ID]. Caller holds mu. Errors are a
// transcript line once; the terminal is then treated as unresolved.
func (s *Session) resolveClientLocked(c live.ClientInfo) {
	if _, done := s.ids[c.ID]; done {
		return
	}
	resolve := s.resolveFn
	if resolve == nil {
		resolve = func(tm inbox.Terminal) (inbox.Resolution, error) {
			return inbox.Resolve(s.usersPath, tm, inbox.MACFor)
		}
	}
	r, err := resolve(terminalOf(c))
	if err != nil {
		s.appendEntryLocked(entry{Kind: entryDim, Text: "chat identity: " + err.Error()})
	}
	s.ids[c.ID] = identity{ID: r.ID, How: r.How, Choices: r.Choices}
}

// bindLocked records a chosen or typed name for a client.
func (s *Session) bindLocked(client int, name string) error {
	id, err := inbox.ValidID(name)
	if err != nil {
		return err
	}
	var tm inbox.Terminal
	for _, c := range s.clients {
		if c.ID == client {
			tm = terminalOf(c)
		}
	}
	bind := s.bindFn
	if bind == nil {
		bind = func(id string, tm inbox.Terminal) error { return inbox.Bind(s.usersPath, id, tm, "") }
	}
	if err := bind(id, tm); err != nil {
		return err
	}
	s.ids[client] = identity{ID: id, How: "asked"}
	return nil
}

func (s *Session) identityOf(client int) identity { return s.ids[client] }

// userID is this terminal's resolved ID, "" when it has none yet.
func (m *View) userID() string { return m.identityOf(m.id).ID }

// needName runs then when this terminal has an ID, else prompts for one and
// runs then once it is bound.
func (m *View) needName(then func(*View) (tea.Model, tea.Cmd)) (tea.Model, tea.Cmd) {
	if m.userID() != "" {
		return then(m)
	}
	m.afterName = then
	m.nameChoices = m.identityOf(m.id).Choices
	m.nameSel, m.nameErr = 0, ""
	m.mode = modeName
	m.input.Reset()
	m.input.Placeholder = "your name for chat and DMs"
	return m, nil
}

func (m *View) handleNameKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.Type {
	case tea.KeyEsc:
		m.afterName = nil
		return m.leaveMode()
	case tea.KeyUp:
		if m.nameSel > 0 {
			m.nameSel--
		}
		return m, nil
	case tea.KeyDown:
		if m.nameSel < len(m.nameChoices) { // len(choices) = "type a name" row
			m.nameSel++
		}
		return m, nil
	case tea.KeyEnter:
		name := strings.TrimSpace(m.input.Value())
		if name == "" && m.nameSel < len(m.nameChoices) {
			name = m.nameChoices[m.nameSel]
		}
		if err := m.bindLocked(m.id, name); err != nil {
			m.nameErr = err.Error()
			return m, nil
		}
		m.input.Reset()
		m.input.Placeholder = inputPlaceholder
		then := m.afterName
		m.afterName = nil
		m.mode = m.idleMode()
		if then != nil {
			return then(m)
		}
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return m, cmd
}

func (m *View) viewName() string {
	var b strings.Builder
	b.WriteString(m.st.ModalTi.Render("Your name for chat and DMs") + "\n\n")
	for i, c := range m.nameChoices {
		cur := "  "
		if i == m.nameSel {
			cur = "> "
		}
		b.WriteString(cur + c + m.st.Dim.Render("  (seen from this address)") + "\n")
	}
	cur := "  "
	if m.nameSel == len(m.nameChoices) {
		cur = "> "
	}
	b.WriteString(cur + "type a name below\n")
	if m.nameErr != "" {
		b.WriteString("\n" + m.st.Err.Render(m.nameErr) + "\n")
	}
	body := m.st.Border.Width(m.width - 4).Render(b.String())
	return body + "\n" + m.inputView() + "\n" + m.st.Dim.Render(" Enter choose · Esc cancel")
}

// whoami is /whoami.
func (m *View) whoami() {
	id := m.identityOf(m.id)
	if id.ID == "" {
		m.appendEntryLocked(entry{Kind: entryDim, Text: "no name yet; /chat, /inbox or /dm will ask"})
		return
	}
	how := map[string]string{"config": "from config", "ip": "bound to " + m.clientIP(), "mac": "recognised by MAC", "asked": "asked this session"}[id.How]
	m.appendEntryLocked(entry{Kind: entryDim, Text: fmt.Sprintf("you are %s (%s)", id.ID, how)})
}

func (m *View) clientIP() string {
	for _, c := range m.clients {
		if c.ID == m.id {
			return c.IP
		}
	}
	return ""
}
```

Wiring: `Session` gains `ids map[int]identity` (made in `NewSession`), `usersPath string` (set in `NewSession` from `inbox.UsersPath()`, error → ""), `resolveFn`, `bindFn`; `SetClients` calls `s.resolveClientLocked(c)` for each info; `View` gains `afterName func(*View) (tea.Model, tea.Cmd)`, `nameChoices []string`, `nameSel int`, `nameErr string`; `mode` gains `modeName`; `handleKey` routes `modeName` to `handleNameKey`; `View()` returns `viewName()` in `modeName`; `enterChat` becomes `return m.needName(func(m *View) (tea.Model, tea.Cmd) { … the body from Task 7 … })` so the prompt precedes the room; `chatName()` returns `m.userID()` (falling back to the label only for a `""` ID, which cannot happen past `needName`); `/whoami` in `slashCommand` calls `m.whoami()`; `/clients` appends ` · <id>` to each row when the client has one; `SlashCommandTable` gains `{"/whoami", "your chat name and how it was decided", true}`. In `SetClients`, a `resolveFn` that hits the disk (`inbox.Resolve`) runs on the caller's goroutine, which is the runner's `onClients` — fine, it is one small file read.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/tui/ -race 2>&1 | tail -3`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui internal/ui/common.go
git commit -m "feat(tui): chat identity — naming prompt, /whoami, IDs in /clients"
```

---

### Task 9: `/inbox` and `/dm` — the views, unread counts, the watcher in the host

**Files:**
- Create: `internal/tui/dm.go`
- Modify: `internal/tui/session.go` (fields), `internal/tui/view.go` (modes, key routing, `View()`, `bottomLine`, `slashCommand`, `WindowSizeMsg`), `internal/tui/served.go` (`RunServed` starts the watcher; roster → `Record.WithUsers`), `internal/ui/common.go`, `internal/ui/repl.go` (plain-mode refusal)
- Test: `internal/tui/dm_test.go`

**Interfaces:**
- Consumes: `inbox.Send/Thread/Threads/MarkRead/Watch/Dir` (Tasks 1–2); `userID`, `needName` (Task 8); `Record.WithUsers`, `live.List` (Task 5).
- Produces:
  ```go
  // on Session
  inboxDir string                      // "" disables DMs (tests set a temp dir)
  onlineFn func(id string) bool        // nil = scan live.List for a record whose Users has id
  func (s *Session) deliverDM(m inbox.Message)     // from the watcher: transient + broadcast inboxMsg
  type inboxMsg struct{ m inbox.Message }
  // on View
  const modeInbox, modeDM mode
  func (m *View) enterInbox() (tea.Model, tea.Cmd)
  func (m *View) enterDM(with string) (tea.Model, tea.Cmd)   // "" = most recent thread
  func (m *View) unreadDMs() int
  ```

- [ ] **Step 1: Write the failing tests**

```go
package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/brown-enterprises/be-code/internal/inbox"
	"github.com/brown-enterprises/be-code/internal/live"
)

func dmSession(t *testing.T, id string) (*Session, *View) {
	s := newTestSession(t)
	s.inboxDir = t.TempDir()
	s.onlineFn = func(string) bool { return false }
	s.resolveFn = func(inbox.Terminal) (inbox.Resolution, error) { return inbox.Resolution{ID: id, How: "config"}, nil }
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "l", IP: "1.1.1.1", User: id}})
	v := s.NewView(1, "l")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	return s, v
}

func TestInboxListsThreadsAndEnterOpensOne(t *testing.T) {
	s, v := dmSession(t, "alice")
	inbox.Send(s.inboxDir, "bob", "alice", "can you look at the picking tests when you have a moment")
	time.Sleep(2 * time.Millisecond)
	inbox.Send(s.inboxDir, "carol", "alice", "lunch?")
	v.slashCommand("/inbox")
	out := v.View()
	if v.mode != modeInbox || !strings.Contains(out, "● carol") || !strings.Contains(out, "lunch?") || strings.Index(out, "carol") > strings.Index(out, "bob") {
		t.Fatalf("mode %v:\n%s", v.mode, out)
	}
	if v.unreadDMs() != 2 || !strings.Contains(v.bottomLine(), "inbox (2)") {
		t.Fatalf("unread %d bottom %q", v.unreadDMs(), v.bottomLine())
	}
	v.Update(tea.KeyMsg{Type: tea.KeyEnter}) // carol
	out = v.View()
	if v.mode != modeDM || !strings.Contains(out, "dm carol") || !strings.Contains(out, "(not online)") || !strings.Contains(out, "lunch?") {
		t.Fatalf("mode %v:\n%s", v.mode, out)
	}
	if v.unreadDMs() != 1 { // opening marks carol's thread read; bob's stays
		t.Fatalf("unread after open: %d", v.unreadDMs())
	}
}

func TestDMSendsAFileAndAgentMentionIsPlainText(t *testing.T) {
	s, v := dmSession(t, "alice")
	started := false
	s.startTurnHook = func(string) { started = true }
	v.slashCommand("/dm bob")
	v.input.SetValue("@agent this must not reach the model")
	v.Update(tea.KeyMsg{Type: tea.KeyEnter})
	th, _ := inbox.Thread(s.inboxDir, "alice", "bob")
	if len(th) != 1 || th[0].From != "alice" || !strings.Contains(th[0].Text, "@agent") {
		t.Fatalf("thread %+v", th)
	}
	if started || s.ag.Pending() != 0 {
		t.Fatal("a DM reached the agent")
	}
	if !strings.Contains(v.View(), "alice: @agent") {
		t.Fatalf("own message not shown:\n%s", v.View())
	}
}

func TestAnArrivingDMPingsTheOwnerOnly(t *testing.T) {
	s := newTestSession(t)
	s.inboxDir = t.TempDir()
	s.resolveFn = func(tm inbox.Terminal) (inbox.Resolution, error) { return inbox.Resolution{ID: tm.User, How: "config"}, nil }
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "a", User: "alice"}, {ID: 2, Label: "b", User: "bob"}})
	va, vb := s.NewView(1, "a"), s.NewView(2, "b")
	for _, v := range []*View{va, vb} {
		v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	}
	m, _ := inbox.Send(s.inboxDir, "carol", "alice", "hey")
	s.deliverDM(m)
	drainAll(t, va, vb)
	if va.unreadDMs() != 1 || vb.unreadDMs() != 0 {
		t.Fatalf("unread a=%d b=%d", va.unreadDMs(), vb.unreadDMs())
	}
	if !strings.Contains(va.toast, "DM from carol") {
		t.Fatalf("toast %q", va.toast)
	}
	// With alice's thread open, the line appears at once.
	va.slashCommand("/dm carol")
	m2, _ := inbox.Send(s.inboxDir, "carol", "alice", "second")
	s.deliverDM(m2)
	drainAll(t, va)
	if !strings.Contains(va.View(), "second") {
		t.Fatalf("open thread missed the line:\n%s", va.View())
	}
}

func TestNarrowDMHidesTheContactColumn(t *testing.T) {
	s, v := dmSession(t, "alice")
	inbox.Send(s.inboxDir, "bob", "alice", "x")
	v.Update(tea.WindowSizeMsg{Width: 60, Height: 20})
	v.slashCommand("/dm bob")
	if out := v.View(); strings.Contains(out, "│") || !strings.Contains(out, "Tab") {
		t.Fatalf("narrow view:\n%s", out)
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/tui/ -run 'TestInboxLists|TestDMSends|TestAnArrivingDM|TestNarrowDM' 2>&1 | head -5`
Expected: build failures.

- [ ] **Step 3: Implement**

`internal/tui/dm.go`:

```go
package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/brown-enterprises/be-code/internal/inbox"
	"github.com/brown-enterprises/be-code/internal/live"
)

// DMs and the inbox (spec §5, §7). The mailbox is the disk; this file is the
// views over it and the ping when the host's watcher sees a new file. A DM
// never goes near the agent: nothing here imports internal/agent's queue.

// inboxMsg is a new DM for one of this session's terminals.
type inboxMsg struct{ m inbox.Message }

// deliverDM is the watcher's callback: a transient for the terminals that
// own the recipient ID, and a broadcast so their views update.
func (s *Session) deliverDM(m inbox.Message) {
	s.mu.Lock()
	defer s.mu.Unlock()
	owned := false
	for _, c := range s.clients {
		if s.ids[c.ID].ID == m.To {
			owned = true
		}
	}
	if !owned {
		return // another host's user, or nobody's yet
	}
	s.toastLocked("DM from " + m.From)
	s.broadcast(inboxMsg{m: m})
}

// online reports whether any session host on this machine has id attached.
func (s *Session) online(id string) bool {
	if s.onlineFn != nil {
		return s.onlineFn(id)
	}
	dir, err := live.Dir()
	if err != nil {
		return false
	}
	recs, _ := live.List(dir)
	for _, r := range recs {
		for _, u := range r.Users {
			if u == id {
				return true
			}
		}
	}
	return false
}

// dmState is one terminal's view state for the inbox and threads.
type dmState struct {
	threads []inbox.ThreadSummary
	sel     int
	with    string
	msgs    []inbox.Message
	vp      viewport.Model
}

func (m *View) reloadThreads() {
	m.dm.threads, _ = inbox.Threads(m.inboxDir, m.userID())
}

// unreadDMs is the count on the bottom line.
func (m *View) unreadDMs() int {
	n := 0
	for _, t := range m.dm.threads {
		n += t.Unread
	}
	return n
}

func (m *View) enterInbox() (tea.Model, tea.Cmd) {
	if !m.cfg.Chat.Enabled || m.inboxDir == "" {
		m.appendEntryLocked(entry{Kind: entryDim, Text: "DMs are disabled (chat.enabled, or no inbox directory)"})
		return m, nil
	}
	return m.needName(func(m *View) (tea.Model, tea.Cmd) {
		m.reloadThreads()
		m.dm.sel = 0
		m.mode = modeInbox
		m.input.Reset()
		return m, nil
	})
}

func (m *View) enterDM(with string) (tea.Model, tea.Cmd) {
	if !m.cfg.Chat.Enabled || m.inboxDir == "" {
		m.appendEntryLocked(entry{Kind: entryDim, Text: "DMs are disabled (chat.enabled, or no inbox directory)"})
		return m, nil
	}
	return m.needName(func(m *View) (tea.Model, tea.Cmd) {
		m.reloadThreads()
		if with == "" {
			if len(m.dm.threads) == 0 {
				m.appendEntryLocked(entry{Kind: entryDim, Text: "no messages yet · /dm <name> to send one"})
				return m, nil
			}
			with = m.dm.threads[0].With
		}
		id, err := inbox.ValidID(with)
		if err != nil {
			m.appendEntryLocked(entry{Kind: entryDim, Text: "/dm: " + err.Error()})
			return m, nil
		}
		m.openThread(id)
		m.mode = modeDM
		m.input.Reset()
		m.input.Placeholder = "message " + id + "… (/back to return)"
		return m, nil
	})
}

// openThread loads a thread and marks it read up to its newest line.
func (m *View) openThread(with string) {
	m.dm.with = with
	m.dm.msgs, _ = inbox.Thread(m.inboxDir, m.userID(), with)
	if n := len(m.dm.msgs); n > 0 {
		_ = inbox.MarkRead(m.inboxDir, m.userID(), m.dm.msgs[n-1].TS)
	}
	m.reloadThreads()
	for i, t := range m.dm.threads {
		if t.With == with {
			m.dm.sel = i
		}
	}
	m.layoutDM()
	m.dm.vp.GotoBottom()
}

func (m *View) handleInboxKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.Type {
	case tea.KeyEsc:
		return m.leaveMode()
	case tea.KeyUp:
		if m.dm.sel > 0 {
			m.dm.sel--
		}
	case tea.KeyDown:
		if m.dm.sel < len(m.dm.threads)-1 {
			m.dm.sel++
		}
	case tea.KeyEnter:
		if m.dm.sel < len(m.dm.threads) {
			return m.enterDM(m.dm.threads[m.dm.sel].With)
		}
	case tea.KeyRunes:
		switch string(k.Runes) {
		case "d":
			if m.dm.sel < len(m.dm.threads) {
				_ = inbox.MarkRead(m.inboxDir, m.userID(), m.dm.threads[m.dm.sel].Latest.TS)
				m.reloadThreads()
			}
		case "/":
			m.input.SetValue("/")
			return m, nil
		}
	}
	if v := m.input.Value(); strings.HasPrefix(v, "/") && k.Type == tea.KeyEnter {
		return m.slashCommand(strings.TrimSpace(v))
	}
	return m, nil
}

func (m *View) handleDMKey(k tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch k.Type {
	case tea.KeyEsc:
		if m.input.Value() == "" {
			return m.leaveMode()
		}
		m.input.Reset()
		return m, nil
	case tea.KeyTab:
		if n := len(m.dm.threads); n > 1 {
			m.openThread(m.dm.threads[(m.dm.sel+1)%n].With)
		}
		return m, nil
	case tea.KeyEnter:
		text := strings.TrimSpace(m.input.Value())
		m.input.Reset()
		if text == "" {
			return m, nil
		}
		if strings.HasPrefix(text, "/") {
			return m.slashCommand(text)
		}
		if _, err := inbox.Send(m.inboxDir, m.userID(), m.dm.with, text); err != nil {
			m.toastLocked("could not send: " + err.Error())
			return m, nil
		}
		m.openThread(m.dm.with)
		return m, nil
	case tea.KeyPgUp:
		m.dm.vp.HalfViewUp()
		return m, nil
	case tea.KeyPgDown:
		m.dm.vp.HalfViewDown()
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(k)
	return m, cmd
}

const contactWidth = 18

func (m *View) showContacts() bool { return m.width >= 70 }

func (m *View) layoutDM() {
	w := m.width
	if m.showContacts() {
		w = m.width - contactWidth - 1
	}
	h := m.height - m.inputRows() - 2
	if h < 3 {
		h = 3
	}
	m.dm.vp.Width, m.dm.vp.Height = w, h
	var b strings.Builder
	for _, x := range m.dm.msgs {
		who := x.From
		style := m.st.ChatUser
		if x.From == m.userID() {
			style = m.st.Accent
		}
		b.WriteString(m.st.Dim.Render(x.TS.Format("15:04")) + " " + style.Render(who+": ") + wrapTo(x.Text, w-8-len(who)) + "\n")
	}
	m.dm.vp.SetContent(strings.TrimRight(b.String(), "\n"))
}

func (m *View) viewInbox() string {
	var b strings.Builder
	b.WriteString(m.st.ModalTi.Render("Inbox — "+m.userID()) + "\n\n")
	if len(m.dm.threads) == 0 {
		b.WriteString(m.st.Dim.Render("no messages yet · /dm <name> to send one") + "\n")
	}
	for i, t := range m.dm.threads {
		cur, dot := "  ", " "
		if i == m.dm.sel {
			cur = "> "
		}
		if t.Unread > 0 {
			dot = "●"
		}
		snippet := strings.SplitN(t.Latest.Text, "\n", 2)[0]
		room := m.width - 30
		if room > 0 && len(snippet) > room {
			snippet = snippet[:room-1] + "…"
		}
		b.WriteString(fmt.Sprintf("%s%s %-16s %s  %s\n", cur, dot, t.With, m.st.Dim.Render(t.Latest.TS.Format("15:04")), snippet))
	}
	body := b.String()
	footer := " inbox · Enter open · d mark read · Esc back"
	if m.compact() {
		footer = " inbox · Esc"
	}
	return body + "\n" + m.inputView() + "\n" + m.st.Dim.Render(footer)
}

func (m *View) viewDM() string {
	right := m.dm.vp.View()
	body := right
	if m.showContacts() {
		var col strings.Builder
		for i, t := range m.dm.threads {
			cur, dot := "  ", " "
			if i == m.dm.sel {
				cur = "> "
			}
			if t.Unread > 0 {
				dot = "●"
			}
			name := t.With
			if len(name) > contactWidth-4 {
				name = name[:contactWidth-5] + "…"
			}
			col.WriteString(fmt.Sprintf("%s%s%s\n", cur, dot, name))
		}
		left := lipgloss.NewStyle().Width(contactWidth).Height(m.dm.vp.Height).Render(col.String())
		body = lipgloss.JoinHorizontal(lipgloss.Top, left, m.st.Dim.Render(strings.Repeat("│\n", m.dm.vp.Height)), right)
	}
	name := m.dm.with
	if !m.online(m.dm.with) {
		name += " (not online)"
	}
	footer := " dm " + name + " · Esc back"
	if !m.showContacts() {
		footer += " · Tab next thread"
	}
	return body + "\n" + m.inputView() + "\n" + m.st.Dim.Render(footer)
}
```

Wiring: `Session` gains `inboxDir string` (set in `NewSession` from `inbox.Dir()`, "" on error), `onlineFn`; `View` gains `dm dmState` and reads `m.inboxDir` through the embedded session; `toastLocked` — the existing toast setter's name (`grep -n "toastUntil =" internal/tui/session.go`; use it, or add `toastLocked(text)` that sets `toast`, `toastUntil = now+4s` and broadcasts what the existing transient path broadcasts); `handleKey` routes `modeInbox`/`modeDM`; `View()` returns `viewInbox()`/`viewDM()`; `WindowSizeMsg` calls `layoutDM()` in `modeDM`; `update` handles `inboxMsg`: `if msg.m.To == m.userID() { m.reloadThreads(); if m.mode == modeDM && m.dm.with == msg.m.From { m.openThread(m.dm.with) } }`; `bottomLine` adds ` · inbox (N)` when `unreadDMs() > 0` and the mode is not inbox/dm; `slashCommand`: `/inbox` → `enterInbox()`, `/dm [name]` → `enterDM(arg)`; on attach (`NewView`) call `m.reloadThreads()` once the ID is known — simplest: in `needName`'s success path and in `SetClients` after resolution, broadcast `inboxMsg{}` with an empty message so every view reloads its counts. `SlashCommandTable`: `{"/inbox", "your DMs from every session on this machine", true}`, `{"/dm [name]", "message a person directly", true}`. `internal/ui/repl.go`: `case "/chat", "/inbox", "/dm", "/back", "/whoami": fmt.Println("chat and DMs need the TUI")`.

`served.go` `RunServed`: after `h.OnClients(r.onClients)`, if `s.cfg.Chat.Enabled && s.inboxDir != ""`, `go inbox.Watch(ctx, s.inboxDir, 0, s.deliverDM)`. And in `r.onClients` (or `SetClients`), after resolving, rewrite the live record with the attached IDs: the runner has the record (see how `cmd/live.go` passes it; add `rec *live.Record` and `recDir string` to the runner if not there) — `rec.WithUsers(ids).Save(recDir)` on a goroutine, errors ignored.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/tui/ ./internal/ui/ -race 2>&1 | tail -3 && stat -c '%y' ~/.be-code/config.json`
Expected: PASS; the config mtime unchanged from before the run.

- [ ] **Step 5: Commit**

```bash
git add internal/tui internal/ui
git commit -m "feat(tui): /inbox and /dm over the shared mailbox; hosts watch for new DMs"
```

---

### Task 10: `@agent` — mention, request, reply

**Files:**
- Create: `internal/tui/mention.go`
- Modify: `internal/tui/chat.go` (post path calls `noteMention`), `internal/tui/session.go` (`flushLocked`/`finishTurnLocked` post the reply)
- Test: `internal/tui/mention_test.go`

**Interfaces:**
- Consumes: `Session.PostLocked`, `Session.room`, `startTurnLocked`, `ag.EnqueueFrom`, `s.running`, `cfg.Chat.MentionContext`.
- Produces:
  ```go
  var mentionRe = regexp.MustCompile(`(?i)(^|[^\w.@])@agent\b`)
  func IsMention(text string) bool
  func (s *Session) mentionRequest(from string) string       // builds §6.2 text from the room tail
  func (s *Session) noteMentionLocked(line store.ChatLine)   // queue it, start it if idle
  mentionQueue []mentionItem; mentionActive *mentionItem     // on Session
  ```

- [ ] **Step 1: Write the failing tests**

```go
package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/brown-enterprises/be-code/internal/store"
)

func TestIsMention(t *testing.T) {
	for _, yes := range []string{"@agent look", "hey @Agent", "(@agent)", "@AGENT?"} {
		if !IsMention(yes) {
			t.Errorf("%q should match", yes)
		}
	}
	for _, no := range []string{"@agents assemble", "mail x@agent.com", "@@agent", "agent", "email@agent"} {
		if IsMention(no) {
			t.Errorf("%q should not match", no)
		}
	}
}

func TestMentionBuildsTheRequestAndStartsATurn(t *testing.T) {
	s := newTestSession(t)
	s.cfg.Chat.MentionContext = 3
	var got string
	s.startTurnHook = func(text string) { got = text }
	s.Post("bob", "the picking tests are red again", "")
	s.Post("", "carol joined", "join") // system lines are omitted from the context
	s.Post("alice", "I think it's the max_dist check", "")
	s.Post("alice", "@agent can you look at tests/test_picking.py?", "")
	if !strings.HasPrefix(got, "Chat mention from alice (room, ") {
		t.Fatalf("request:\n%s", got)
	}
	for _, want := range []string{"Recent chat:", "bob: the picking tests are red again", "alice: @agent can you look", "Reply to alice in the chat."} {
		if !strings.Contains(got, want) {
			t.Fatalf("request lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "carol joined") {
		t.Fatalf("system line in the request:\n%s", got)
	}
	if lines := strings.Count(got, "\n  "); lines != 3 {
		t.Fatalf("expected 3 context lines, got %d:\n%s", lines, got)
	}
	if r := s.Room(); r[len(r)-1].Kind != "mention" {
		t.Fatalf("kind %q", r[len(r)-1].Kind)
	}
}

func TestMentionMidRunIsEnqueuedAndReplyIsPostedOnce(t *testing.T) {
	s := newTestSession(t)
	s.startTurnHook = func(string) {}
	s.mu.Lock()
	s.setRunStateLocked(true, "thinking")
	s.mu.Unlock()
	s.Post("alice", "@agent status?", "")
	if s.ag.Pending() != 1 {
		t.Fatalf("pending %d", s.ag.Pending())
	}
	// The run ends with an answer: it is posted to the room as agent, once.
	s.mu.Lock()
	s.streaming.WriteString("All green.\nDetails:\n" + strings.Repeat("line\n", 50))
	s.finishTurnLocked(nil, nil)
	s.mu.Unlock()
	r := s.Room()
	last := r[len(r)-1]
	if last.User != "agent" || last.Kind != "reply" || !strings.HasPrefix(last.Text, "All green.") {
		t.Fatalf("reply line %+v", last)
	}
	if !strings.Contains(last.Text, "(full reply in the transcript)") || strings.Count(last.Text, "\n") > 41 {
		t.Fatalf("not cut at 40 lines: %d newlines", strings.Count(last.Text, "\n"))
	}
	s.mu.Lock()
	s.streaming.WriteString("unrelated later answer")
	s.finishTurnLocked(nil, nil)
	s.mu.Unlock()
	if r := s.Room(); r[len(r)-1].User == "agent" && strings.Contains(r[len(r)-1].Text, "unrelated") {
		t.Fatal("a turn nobody mentioned was posted to the room")
	}
}

func TestSecondMentionQueuesBehindTheFirst(t *testing.T) {
	s := newTestSession(t)
	starts := 0
	s.startTurnHook = func(string) { starts++ }
	s.Post("alice", "@agent one", "")
	s.Post("bob", "@agent two", "")
	if starts != 1 || len(s.mentionQueue) != 1 {
		t.Fatalf("starts %d queued %d", starts, len(s.mentionQueue))
	}
	if r := s.Room(); !strings.HasSuffix(r[len(r)-1].Text, "(queued)") {
		t.Fatalf("second mention not marked: %+v", r[len(r)-1])
	}
	s.mu.Lock()
	s.streaming.WriteString("answer one")
	s.finishTurnLocked(nil, nil)
	s.mu.Unlock()
	if starts != 2 || len(s.mentionQueue) != 0 {
		t.Fatalf("second mention did not start: starts %d queued %d", starts, len(s.mentionQueue))
	}
	v := s.NewView(1, "l")
	v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	v.slashCommand("/chat")
	if !strings.Contains(v.View(), "agent is working on bob's question") {
		t.Fatalf("footer:\n%s", v.View())
	}
}
```

Check `finishTurnLocked`'s behaviour when `startTurnHook` is set: the test relies on it not starting a goroutine. If `finishTurnLocked` calls `setRunStateLocked(false, "")` then starts the next queued turn through `startTurnLocked`, the hook captures it — good.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/tui/ -run 'TestIsMention|TestMention|TestSecondMention' 2>&1 | head -5`
Expected: build failures.

- [ ] **Step 3: Implement**

`internal/tui/mention.go`:

```go
package tui

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/brown-enterprises/be-code/internal/store"
)

// @agent (spec §6): a room line that names the model becomes a request —
// the line and a few before it — through the same path typed input takes,
// and the turn's final answer is copied back to the room. One at a time;
// later mentions wait in order.

var mentionRe = regexp.MustCompile(`(?i)(^|[^\w.@])@agent\b`)

// IsMention reports whether text addresses the model.
func IsMention(text string) bool { return mentionRe.MatchString(text) }

type mentionItem struct {
	from string
	line store.ChatLine
}

// mentionRequest is the text sent to the model for a mention by from, built
// from the room as it stands (the mention is its last line).
func (s *Session) mentionRequest(from string, at store.ChatLine) string {
	n := s.cfg.Chat.MentionContext
	var ctx []store.ChatLine
	for i := len(s.room) - 1; i >= 0 && len(ctx) < n+1; i-- {
		l := s.room[i]
		if l.User == "" {
			continue
		}
		ctx = append([]store.ChatLine{l}, ctx...)
	}
	if n <= 0 {
		ctx = []store.ChatLine{at}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Chat mention from %s (room, %s). Recent chat:\n", from, at.TS.Format("15:04"))
	for _, l := range ctx {
		fmt.Fprintf(&b, "  %s %s: %s\n", l.TS.Format("15:04"), l.User, strings.TrimSuffix(l.Text, " (queued)"))
	}
	fmt.Fprintf(&b, "\nReply to %s in the chat.", from)
	return b.String()
}

// noteMentionLocked is called by PostLocked for a person's line that
// mentions the model. Caller holds mu.
func (s *Session) noteMentionLocked(line store.ChatLine) {
	item := mentionItem{from: line.User, line: line}
	if s.mentionActive != nil {
		s.mentionQueue = append(s.mentionQueue, item)
		last := &s.room[len(s.room)-1]
		last.Text += " (queued)"
		return
	}
	s.startMentionLocked(item)
}

func (s *Session) startMentionLocked(item mentionItem) {
	s.mentionActive = &item
	req := s.mentionRequest(item.from, item.line)
	s.broadcast(mentionBusyMsg(item.from))
	if s.running {
		s.ag.EnqueueFrom(req, 0) // lands after the current tool results
		return
	}
	s.appendEntryLocked(entry{Kind: entryUser, Label: "chat> ", Text: item.from + " mentioned the agent"})
	s.startTurnLocked(req)
}

// mentionBusyMsg names whose question the model is on ("" when none).
type mentionBusyMsg string

// finishMentionLocked runs from finishTurnLocked with the turn's answer:
// posts it to the room and starts the next queued mention.
func (s *Session) finishMentionLocked(answer string) {
	if s.mentionActive == nil {
		return
	}
	if answer != "" {
		lines := strings.Split(strings.TrimRight(answer, "\n"), "\n")
		if len(lines) > 40 {
			lines = append(lines[:40], "(full reply in the transcript)")
		}
		for i := 1; i < len(lines); i++ {
			lines[i] = "  " + lines[i]
		}
		s.PostLocked("agent", strings.Join(lines, "\n"), "reply")
	}
	s.mentionActive = nil
	s.broadcast(mentionBusyMsg(""))
	if len(s.mentionQueue) > 0 {
		next := s.mentionQueue[0]
		s.mentionQueue = s.mentionQueue[1:]
		s.startMentionLocked(next)
	}
}
```

Wiring: in `PostLocked`, after appending and before broadcasting, `if kind == "" && user != "" && user != "agent" && IsMention(text) { line.Kind = "mention"; s.room[len(s.room)-1].Kind = "mention" }` and after the broadcast `if line.Kind == "mention" { s.noteMentionLocked(line) }` (the request must include the mention, so it is appended first). `Session` gains `mentionQueue []mentionItem`, `mentionActive *mentionItem`. In `finishTurnLocked`, right after `s.flushLocked()`, `s.finishMentionLocked(s.lastReply)` when `err == nil` (on an error, `finishMentionLocked("")` so the queue moves on). Note the mid-run case: the enqueued mention is delivered inside the *current* run; its answer is that run's final answer, which is what `finishTurnLocked` sees — correct. A `mentionBusyMsg` in `update` sets `m.mentionBusy = string(msg)`. `PostLocked` must not be called from `finishMentionLocked` with the reply while holding a view's lock — it isn't; `finishTurnLocked` runs under `mu` only.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/tui/ -race 2>&1 | tail -3`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui
git commit -m "feat(tui): @agent in the room asks the model and posts its answer back"
```

---

### Task 11: Two terminals, end to end in-process; docs; version 0.15.0

**Files:**
- Test: `internal/tui/chat_e2e_test.go`
- Modify: `README.md` (a "Chat and DMs" section under the shared-sessions material; the status line), `CHANGELOG.md`, `build.mk` (`VERSION := 0.15.0`), `docs/live-checklist.md` (a "Chat and DMs" section), `CLAUDE.md` at the workspace root (a short section under Shared sessions)

- [ ] **Step 1: Write the end-to-end test**

```go
package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/brown-enterprises/be-code/internal/inbox"
	"github.com/brown-enterprises/be-code/internal/live"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// Two terminals on one session: a room line reaches both; a DM from one
// reaches the other's inbox; a mention produces one reply in the transcript
// and one in the room.
func TestTwoTerminalsChatDMAndMention(t *testing.T) {
	s := newTestSession(t, func(ag *agent.Agent) {
		ag.Provider = &funcProviderTUI{fn: func(provider.ChatRequest) (*provider.ChatResponse, error) {
			return &provider.ChatResponse{Content: "The picking tests need max_dist honoured."}, nil
		}}
	})
	s.inboxDir = t.TempDir()
	s.onlineFn = func(string) bool { return true }
	s.resolveFn = func(tm inbox.Terminal) (inbox.Resolution, error) { return inbox.Resolution{ID: tm.User, How: "config"}, nil }
	s.SetClients([]live.ClientInfo{{ID: 1, Label: "vscode (pid 1)", User: "alice"}, {ID: 2, Label: "ssh from 192.168.1.38 (pid 2)", User: "bob"}})
	a, b := s.NewView(1, "vscode (pid 1)"), s.NewView(2, "ssh from 192.168.1.38 (pid 2)")
	for _, v := range []*View{a, b} {
		v.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
		v.slashCommand("/chat")
	}
	a.input.SetValue("tests are red")
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	drainAll(t, a, b)
	if !strings.Contains(b.View(), "alice: tests are red") {
		t.Fatalf("bob's room:\n%s", b.View())
	}
	b.input.SetValue("/dm alice")
	b.Update(tea.KeyMsg{Type: tea.KeyEnter})
	b.input.SetValue("private: lunch?")
	b.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ := inbox.Thread(s.inboxDir, "alice", "bob")
	s.deliverDM(m[0])
	drainAll(t, a, b)
	if a.unreadDMs() != 1 || strings.Contains(a.View(), "lunch?") {
		t.Fatalf("alice: unread %d; a DM must not appear in the room:\n%s", a.unreadDMs(), a.View())
	}
	a.input.SetValue("@agent why are they red?")
	a.Update(tea.KeyMsg{Type: tea.KeyEnter})
	waitForTUI(t, func() bool { return !s.isRunning() })
	drainAll(t, a, b)
	room := s.Room()
	last := room[len(room)-1]
	if last.User != "agent" || !strings.Contains(last.Text, "max_dist") {
		t.Fatalf("room reply: %+v", last)
	}
	if n := strings.Count(transcriptText(s), "max_dist"); n != 1 {
		t.Fatalf("transcript has the answer %d times", n)
	}
	if !strings.Contains(b.View(), "agent: The picking tests") {
		t.Fatalf("bob did not see the reply:\n%s", b.View())
	}
}
```

Use the package's existing provider double and wait helper (`grep -n "type .*Provider struct\|func waitFor" internal/tui/*_test.go`) instead of `funcProviderTUI`/`waitForTUI` if they exist under other names; `s.isRunning()` and `transcriptText(s)` are small helpers to add in `helpers_test.go` if absent.

- [ ] **Step 2: Run it**

Run: `go test ./internal/tui/ -run TestTwoTerminals -race -v 2>&1 | tail -5`
Expected: PASS. If it fails, the defect is in Tasks 7–10 — fix there, do not weaken the test.

- [ ] **Step 3: Docs and version**

README: a `### Chat and DMs` subsection after the shared-sessions section:

```
Every terminal attached to a session shares a chat room: `/chat` opens it on your terminal
(the session keeps running underneath; `Esc` or `/back` returns), Enter posts, and `@agent`
in a line hands it — with the last `chat.mention_context` lines of the room — to the
session's model, whose answer appears in the transcript and in the room. DMs span every
session on the machine: `/dm <name>` opens a thread, `/inbox` lists them newest first
(`●` unread), and a message to someone not attached anywhere waits for them. Your name
comes from `chat.name` in your own config, else the names already seen from your address,
else you are asked once (`/whoami` says which). Messages live under `~/.be-code/inbox/`;
identity is advisory, not security — anyone with a shell on the host can read them. Plain
mode and headless runs have neither.
```

Status line: prepend `v0.15.0 — a per-session chat room with @agent, and machine-wide DMs.` CHANGELOG `## v0.15.0 — chat and messaging` with the decisions table's substance in prose and the fsnotify → poll ruling. `build.mk` `VERSION := 0.15.0`. `docs/live-checklist.md`: a `## Chat and DMs` section listing the spec §10 live checks verbatim. Workspace `CLAUDE.md`: under Shared sessions, a paragraph naming `internal/inbox` (mailbox on disk, poll, `users.json` under a lock, MAC only for an unknown IP), the room on `Session` (`Post`, `chatMsg`, saved as `store.Session.Chat`), the three modes, `mention.go`'s regexp and one-at-a-time queue, and the invariant that a DM never reaches the agent.

- [ ] **Step 4: Full verification**

Run: `stat -c '%y' ~/.be-code/config.json && make -f build.mk verify && sh test/e2e/run_e2e.sh && GOOS=windows GOARCH=amd64 go build -o /dev/null . && stat -c '%y' ~/.be-code/config.json`
Expected: verify green, `E2E PASS`, Windows builds, config mtime unchanged.

- [ ] **Step 5: Commit**

```bash
git add -A
git commit -m "feat: chat and messaging — docs, live checklist, 0.15.0"
```

---

## Self-review

**Spec coverage.** §2 architecture → Tasks 1–3, 7, 9. §3 identity: `Hello` fields → 5; `users.json`, resolution, lock → 3; MAC → 4; `/whoami` → 8. §4 room: data and cap → 6, 7; join/leave → 7; `@agent` → 10. §5 mailbox: layout, package, `Send` to unknown ID → 1; `Watch` → 2 (poll only — recorded ruling); online via live records → 5, 9; host wiring → 9. §6 mention: match, request, queue, reply, not in DMs → 10 (DM path tested in 9). §7 TUI: three modes, bottom line, naming prompt, `/clients` column, narrow layout → 7–9; mouse selection in the new modes is **not** implemented (the modes have their own viewports; `selection.go` is bound to the transcript viewport) — a gap, accepted for this version and to be listed in the CHANGELOG as a limit; theme roles → 7. §8 config → 6. §9 edge cases: concurrent binds → 3; unwritable inbox → 9 (toast); poll → 2; resume on another machine → 6/7; room cap → 7; `@agent` while the backend is down → the ordinary error path posts nothing to the room, so `finishMentionLocked("")` must run on error — covered in 10's wiring note. §10 tests: all listed except the pty-driven e2e (replaced by Task 11's in-process two-view test; the live checklist covers the real terminals — ruling). §11 unchanged.

**Placeholder scan.** No TBD/TODO. Task 6's store test is marked "adapt to the store's save function" — the implementer reads `sessions.go` for the real names; the assertion is complete. Task 7 names helpers (`inputRows`, `inputView`, `inputPlaceholder`, `wrapTo`, `drainAll`, `lastEntryText`) that may need extracting or adding — each is specified.

**Type consistency.** `store.ChatLine{TS, User, Text, Kind}` used identically in 6, 7, 10. `inbox.Terminal{IP, Login, User, PID}` in 3, 8. `inbox.Resolution{ID, How, Choices, Ask}` in 3, 8. `Session.resolveFn func(inbox.Terminal) (inbox.Resolution, error)` and `bindFn func(string, inbox.Terminal) error` in 8, 9, 11. `deliverDM(inbox.Message)` in 9, 11. `mentionBusy string` declared in 7, set in 10. `modeName` declared in 8, `modeChat` in 7, `modeInbox`/`modeDM` in 9.
