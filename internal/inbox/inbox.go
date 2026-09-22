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
		// The sender is what follows the first '-': the prefix is all
		// digits, and an ID may itself contain '-', so a suffix match
		// would hand "b-ob"'s messages to "ob".
		if sender != "" && senderOf(name) != sender {
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

// senderOf is the sender a message file name records, "" if malformed.
func senderOf(name string) string {
	i := strings.IndexByte(name, '-')
	if i < 0 || !strings.HasSuffix(name, ".json") {
		return ""
	}
	return strings.TrimSuffix(name[i+1:], ".json")
}

// Threads is one summary per correspondent of me, newest first.
//
// What I sent lives in the recipients' directories, so this reads every user
// directory on each call. Deliberate: the mailbox has no index and no daemon
// to keep one, a person's inbox is a few hundred files at most, and /inbox is
// opened by a hand, not a loop.
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
