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
	return readTrust(path)[pack]
}

// Trust records that this exact pack content is approved for this workspace.
func Trust(path, pack, hash string) error {
	trustMu.Lock()
	defer trustMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Re-read before writing: another session on this workspace may have
	// approved a different pack since this one looked.
	m := readTrust(path)
	m[pack] = hash
	b, err := json.MarshalIndent(m, "", "  ")
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

func readTrust(path string) map[string]string {
	m := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	if json.Unmarshal(b, &m) != nil {
		return map[string]string{}
	}
	return m
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
