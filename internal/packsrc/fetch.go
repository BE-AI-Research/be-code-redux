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
		return nil, fmt.Errorf("%s offers no packs (looked for pack.json there and in its packs/ subdirectories)", s.Spec)
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
