package packs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ContentHash fingerprints the part of a pack that decides what runs: the
// manifest, and every file under tools/.
//
// Not the prose. A pack's chunks and core are text the model reads, and
// re-asking for approval because somebody improved a paragraph would train
// the person to approve without looking — which is the only way this gate
// can actually fail. A changed verify command or a rewritten build script is
// a different matter, and re-asks.
//
// The file mode is part of it: making a script executable changes what the
// pack can do without changing a byte of its contents.
func ContentHash(root string) (string, error) {
	h := sha256.New()
	add := func(label string, b []byte, mode fs.FileMode) {
		fmt.Fprintf(h, "%s\x00%d\x00%o\x00", label, len(b), mode.Perm())
		h.Write(b)
	}

	manifest := filepath.Join(root, "pack.json")
	b, err := os.ReadFile(manifest)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(manifest)
	if err != nil {
		return "", err
	}
	add("pack.json", b, st.Mode())

	toolsDir := filepath.Join(root, "tools")
	var paths []string
	err = filepath.WalkDir(toolsDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // no tools/ at all is normal
		}
		if !d.IsDir() {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return "", err
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

// Trust records that this exact pack content is approved for this workspace.
func Trust(path, pack, hash string) error {
	return mutate(path, func(st *state) {
		if st.Trusted == nil {
			st.Trusted = map[string]string{}
		}
		st.Trusted[pack] = hash
	})
}

// mutate is every write to the file: re-read, change, write atomically.
// Re-reading matters because sessions on one workspace share this file, and
// one of them recording a trust decision must not drop another's.
func mutate(path string, f func(*state)) error {
	trustMu.Lock()
	defer trustMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	st := readState(path)
	f(&st)
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
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
	return nil
}

// readState reads the file; missing or corrupt reads as empty. A decision
// nobody can produce is not a decision, and this must never fail a session.
func readState(path string) state {
	var st state
	b, err := os.ReadFile(path)
	if err != nil {
		return state{}
	}
	if json.Unmarshal(b, &st) != nil {
		return state{}
	}
	return st
}

// TrustDetail is what the approval prompt shows: everything about the pack
// that decides what it can do. The commands are listed in full, because
// approving a pack is approving these to run without being asked again.
func TrustDetail(p *Pack, workspace string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "pack %q (%s) came with this project, in %s\n", p.Name, p.Version, p.Root)
	if p.Description != "" {
		fmt.Fprintf(&b, "%s\n", p.Description)
	}
	b.WriteString("\nIt adds rules to every request, and may run these commands when verifying:\n")
	checks := p.Checks(workspace)
	if len(checks) == 0 {
		b.WriteString("  (none)\n")
	}
	for _, c := range checks {
		fmt.Fprintf(&b, "  %s: %s\n", c.Name, c.Display())
	}
	if len(p.mcpServers) > 0 {
		b.WriteString("\nAnd would start these servers:\n")
		for _, n := range sortedKeys(p.mcpServers) {
			fmt.Fprintf(&b, "  %s\n", n)
		}
	}
	b.WriteString("\nApprove only if you trust this repository.")
	return b.String()
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
