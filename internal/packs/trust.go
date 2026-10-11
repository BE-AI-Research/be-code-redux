package packs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ContentHash fingerprints the part of a pack that decides what runs:
// everything in it except the prose.
//
// Not the prose. A pack's core, chunks, playbooks and README are text the
// model reads, and re-asking for approval because somebody improved a
// paragraph would train the person to approve without looking — which is the
// only way this gate can actually fail. core.md is the sharpest case, since
// it is injected as standing instructions; it is handled by showing it in the
// approval prompt and saying plainly that it is not covered, rather than by
// hashing it. See TrustDetail.
//
// Everything else is covered, which is a deny-list and used to be an
// allow-list of pack.json plus tools/. That was the wrong shape: a tools/
// script is free to exec ../scripts/real-build, so approving one command
// covered the wrapper and not the code it actually ran, and a later commit
// could rewrite that with no further prompt. A config a script reads, a
// template the model copies and a command file telling it what to do are all
// in the same position.
//
// An executable is never prose, wherever it sits: a program dropped into
// chunks/ is not text the model reads. The file mode is part of the hash for
// the same reason — making a script executable changes what the pack can do
// without changing a byte of its contents.
func ContentHash(root string) (string, error) {
	h := sha256.New()
	add := func(label string, b []byte, mode fs.FileMode) {
		fmt.Fprintf(h, "%s\x00%d\x00%o\x00", label, len(b), mode.Perm())
		h.Write(b)
	}

	var paths []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			// Version-control metadata is not part of what a pack does, and
			// a pack installed with `git clone` would otherwise hash
			// thousands of objects and change its fingerprint on every
			// commit — re-asking for approval each session, which is the
			// habit this gate most needs to avoid.
			switch d.Name() {
			case ".git", ".hg", ".svn":
				if p != root {
					return fs.SkipDir
				}
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if isProse(rel) {
			// Exempt unless it can be executed, which prose cannot.
			info, ierr := d.Info()
			if ierr != nil || info.Mode().Perm()&0o111 == 0 {
				return nil
			}
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		return "", err
	}
	if len(paths) == 0 {
		// No manifest and nothing else: let the caller's own load error say so
		// rather than returning a hash of nothing that would match anything.
		if _, serr := os.Stat(filepath.Join(root, "pack.json")); serr != nil {
			return "", serr
		}
	}
	// Sorted: a directory listing's order is not part of what was approved.
	sort.Strings(paths)
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return "", err
		}
		info, err := os.Lstat(p)
		if err != nil {
			return "", err
		}
		rel, _ := filepath.Rel(root, p)
		add(filepath.ToSlash(rel), b, info.Mode())
	}
	return hex.EncodeToString(h.Sum(nil))[:32], nil
}

// isProse reports whether a pack-relative path is text the model reads rather
// than something that decides what runs. Kept deliberately short: anything
// not named here is covered by the trust hash, so a new kind of file in a pack
// errs toward re-asking rather than toward being changed unnoticed.
func isProse(rel string) bool {
	switch {
	case rel == "core.md":
		return true
	case strings.HasPrefix(rel, "chunks/"):
		return true
	case strings.HasPrefix(rel, "playbooks/"):
		return true
	}
	base := strings.ToUpper(path.Base(rel))
	return strings.HasPrefix(base, "README") || strings.HasPrefix(base, "LICENSE") ||
		strings.HasPrefix(base, "CHANGELOG")
}

// state is what one workspace has decided about packs, in
// engine/<key>/packs.json. Trusted maps a pack name to the content hash that
// was approved; Forced records a pack switched on or off by hand, which
// overrides detection either way.
//
// One file, because it is one subject — "what this project has decided about
// packs" — and because a person reading it should not have to find two.
type state struct {
	Trusted map[string]string `json:"trusted,omitempty"`
	Forced  map[string]bool   `json:"forced,omitempty"`
}

// trustMu serialises the read-modify-write below. Sessions on one workspace
// share the file, so each save re-reads it rather than overwriting what
// another session recorded.
var trustMu sync.Mutex

var trustSeq int

// TrustedHash is the pack content hash a person approved for this workspace,
// or "" for none. A missing or unreadable store reads as empty: a decision
// nobody can produce is not a decision, and this must never fail a session.
func TrustedHash(path, pack string) string {
	trustMu.Lock()
	defer trustMu.Unlock()
	return readState(path).Trusted[pack]
}

// Forced reports a pack switched on or off by hand for this workspace: on,
// and whether any decision was recorded at all. Unset means detection
// decides.
func Forced(path, pack string) (on, set bool) {
	trustMu.Lock()
	defer trustMu.Unlock()
	v, ok := readState(path).Forced[pack]
	return v, ok
}

// SetForced switches a pack on or off for this workspace, overriding
// detection until it is cleared.
func SetForced(path, pack string, on bool) error {
	return mutate(path, func(st *state) {
		if st.Forced == nil {
			st.Forced = map[string]bool{}
		}
		st.Forced[pack] = on
	})
}

// ClearForced returns a pack to being decided by detection.
func ClearForced(path, pack string) error {
	return mutate(path, func(st *state) { delete(st.Forced, pack) })
}

// Untrust forgets a pack's approval for this workspace. Removing a pack
// without it leaves an approval recorded for content that is no longer there,
// which a later reinstall — or a different pack that happens to share the
// name — would inherit.
func Untrust(path, pack string) error {
	return mutate(path, func(st *state) { delete(st.Trusted, pack) })
}

// Trust records that this exact pack content is approved for this workspace.
func Trust(path, pack, hash string) error {
	return mutate(path, func(st *state) {
		if st.Trusted == nil {
			st.Trusted = map[string]string{}
		}
		st.Trusted[pack] = hash
	})
}

// beforeWriteHook fires between mutate's read and its write. Nil in
// production; a test sets it to drive the interleaving that loses a delta,
// rather than hoping for it.
var beforeWriteHook func()

// writeState persists st atomically. Split out of mutate so a test can stand
// in for another process's write.
func writeState(path string, st state) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Written through a temp file in the same directory and renamed, the way
	// everything else in the dotdir is, so a crash mid-write cannot leave a
	// half-written record of what somebody approved.
	trustSeq++
	tmp := path + ".tmp" + strconv.Itoa(trustSeq)
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	// What we wrote is what this session last saw, or the work ledger's
	// record of this file is stale from the moment it is written.
	if OnStateRead != nil {
		OnStateRead(path, b)
	}
	return nil
}

// mutate is every write to the file: read, change, write atomically.
//
// trustMu serialises writers inside this process. It cannot serialise another
// process — `be-code packs add` in a second terminal while a session is live
// — so before writing, the file is compared against the bytes this attempt
// read: if it moved, the delta is applied again to the new state. That
// narrows the window to the instant between the comparison and the rename;
// it does not close it, and nothing short of a lock could.
//
// This is the mechanical race of work-ledger spec §2, and it deliberately
// does not prompt: both deltas were wanted, nothing is lost by retrying, and
// a modal raised on a microsecond interleaving is noise nobody can answer.
func mutate(path string, f func(*state)) error {
	trustMu.Lock()
	defer trustMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	const attempts = 3
	for i := 0; i < attempts; i++ {
		before := rawState(path)
		st := parseState(path, before)
		f(&st)
		if beforeWriteHook != nil {
			beforeWriteHook()
		}
		if !bytes.Equal(before, rawState(path)) {
			continue // somebody else wrote between our read and our write
		}
		return writeState(path, st)
	}
	return fmt.Errorf("%s kept changing under %d attempts to update it; nothing was written",
		filepath.Base(path), attempts)
}

// rawState is the file's exact bytes, or nil. The comparison is on bytes
// rather than on a parsed state so a change this version does not understand
// still counts as a change.
func rawState(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

// OnStateRead is told the trust file's bytes whenever it is read or written,
// so the work ledger can tell a later outside change from our own write. Nil
// in production until cmd/root.go assigns it; a hook rather than an import,
// because internal/workledger reads internal/live and internal/gitctx and
// this package must not depend on either.
var OnStateRead func(path string, content []byte)

// readState reads the file; missing or corrupt reads as empty. A decision
// nobody can produce is not a decision, and this must never fail a session.
func readState(path string) state {
	return parseState(path, rawState(path))
}

// parseState is readState over bytes already in hand, so mutate reads the
// file once per attempt and the hook sees exactly the bytes that were used.
func parseState(path string, b []byte) state {
	if b == nil {
		return state{}
	}
	if OnStateRead != nil {
		OnStateRead(path, b)
	}
	var st state
	if json.Unmarshal(b, &st) != nil {
		return state{}
	}
	return st
}

// coreExcerptLines bounds how much of core.md the approval prompt shows. Core
// is budgeted at up to 1500 tokens — well over a hundred lines — and a modal
// that long is one people scroll past rather than read, which defeats the
// point of showing it at all.
const coreExcerptLines = 15

// TrustDetail is what the approval prompt shows: everything about the pack
// that decides what it can do. The commands are listed in full, because
// approving a pack is approving these to run without being asked again.
//
// And the rules, because they are the other half of what is being authorised
// and the prompt used to describe them in four words. A pack's core goes into
// the system prompt introduced as instructions to follow — the inverse of the
// framing BECODE.md gets, which says explicitly that project notes are not
// instructions — so a person approving a pack on the strength of a build
// command is also consenting to arbitrary standing instructions for the
// model. That is worth seeing.
//
// The prompt says plainly that the rules are not covered by the approval.
// They are deliberately outside the trust hash (see ContentHash) so that
// improving a paragraph does not re-ask, which means a repository can change
// them later without another prompt. Relying on that silently is the part
// that was wrong; saying it is the fix.
func TrustDetail(p *Pack, workspace string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pack %q (%s) came with this project, in %s\n", p.Name, p.Version, p.Root)
	if p.Description != "" {
		fmt.Fprintf(&b, "%s\n", p.Description)
	}

	b.WriteString("\nIt adds rules to every request. They are given to the model as\n")
	b.WriteString("instructions, and they are NOT covered by this approval -- the\n")
	b.WriteString("repository can change them later without asking again.\n")
	if excerpt, total, shown := coreExcerpt(p.Core); shown > 0 {
		b.WriteString("\n")
		b.WriteString(excerpt)
		if total > shown {
			fmt.Fprintf(&b, "  ... (%d of %d lines)\n", shown, total)
		}
		fmt.Fprintf(&b, "\nFull text: be-code packs info %s\n", p.Name)
	}

	b.WriteString("\nIt may run these commands when verifying:\n")
	checks := p.Checks(workspace)
	if len(checks) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, c := range checks {
		fmt.Fprintf(&b, "  %s: %s\n", c.Name, c.Display())
	}
	if len(p.mcpServers) > 0 {
		// Said accurately. mcp_servers is parsed and nothing starts it, so
		// "would start these servers" described behaviour this build does not
		// have — in the one prompt where a person is deciding what to trust,
		// which is the worst place to overstate anything. When they are
		// wired, this wording changes with the code.
		b.WriteString("\nIt declares these tool servers, which this version does not start:\n")
		for _, n := range sortedKeys(p.mcpServers) {
			fmt.Fprintf(&b, "  %s\n", n)
		}
	}
	b.WriteString("\nApprove only if you trust this repository.")
	return b.String()
}

// coreExcerpt is the first coreExcerptLines non-empty lines of a pack's core,
// indented so they read as quoted material rather than as the prompt's own
// words, with the count so the reader knows what is missing.
//
// Blank lines are skipped and are not counted in the total either. Counting
// them made the prompt say "5 of 7 lines" when it had in fact shown every
// rule there was, which understates the pack in the one place a person is
// deciding whether to trust it.
func coreExcerpt(core string) (excerpt string, total, shown int) {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimSpace(core), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		total++
		if shown < coreExcerptLines {
			fmt.Fprintf(&b, "  %s\n", l)
			shown++
		}
	}
	return b.String(), total, shown
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
