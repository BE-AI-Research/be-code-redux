package workledger

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/gitctx"
)

// Now is the clock restore points are stamped with. A var so a test can fix it.
var Now = func() time.Time { return time.Now() }

// sidecarMark is what makes a sidecar recognisable to PruneSidecars.
const sidecarMark = ".overwritten-"

type RestoreKind int

const (
	// RestoreNone is "there was nothing to keep": the file does not exist.
	RestoreNone RestoreKind = iota
	// RestoreGit is a branch under gitctx.SnapshotPrefixOverwrite.
	RestoreGit
	// RestoreSidecar is a timestamped copy beside the file.
	RestoreSidecar
)

// Restore is where an old version went, and the line that brings it back.
type Restore struct {
	Kind    RestoreKind
	Branch  string // RestoreGit
	Sidecar string // RestoreSidecar
	HowTo   string // the command a person can run
}

// KeepOld makes the current contents of path recoverable before a caller
// overwrites them without asking.
//
// A file inside a git workspace goes to a branch, which is what the owner
// asked for: "marked in the git tree so if it does overwrite something, then
// it can be restored." Anything else — a workspace that is not a repository,
// or a file in the dotdir, which is in no tree at all — goes to a timestamped
// copy beside it.
//
// An error here means nothing could be kept, and the caller must not proceed:
// writing over somebody's work unattended is only acceptable while it is
// recoverable (spec §12).
func KeepOld(ctx context.Context, workspace, path string) (Restore, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return Restore{Kind: RestoreNone}, nil
		}
		return Restore{}, err
	}
	if inside(workspace, path) && gitctx.IsRepo(ctx, workspace) {
		branch, err := gitctx.SnapshotTo(ctx, workspace, gitctx.SnapshotPrefixOverwrite,
			"be-code: before overwriting a change this session did not make")
		if err == nil {
			rel, rerr := filepath.Rel(workspace, path)
			if rerr != nil {
				rel = "."
			}
			return Restore{
				Kind:   RestoreGit,
				Branch: branch,
				HowTo: fmt.Sprintf("git restore --source=%s --staged --worktree -- %s",
					branch, filepath.ToSlash(rel)),
			}, nil
		}
		// Fall through: a repository that cannot be snapshotted still has a
		// directory the old bytes can go in.
	}
	return sidecar(path)
}

func sidecar(path string) (Restore, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Restore{}, err
	}
	name := path + sidecarMark + Now().Format("20060102-150405")
	for i := 2; ; i++ {
		if _, err := os.Stat(name); os.IsNotExist(err) {
			break
		}
		name = fmt.Sprintf("%s%s%s-%d", path, sidecarMark, Now().Format("20060102-150405"), i)
	}
	// 0600 and not the original's mode: a sidecar of a dotdir file holds
	// somebody's approvals, and a sidecar of a workspace file is nobody's
	// business but this user's either.
	if err := os.WriteFile(name, b, 0o600); err != nil {
		return Restore{}, fmt.Errorf("nowhere to keep the old %s: %w", filepath.Base(path), err)
	}
	return Restore{
		Kind:    RestoreSidecar,
		Sidecar: name,
		HowTo:   fmt.Sprintf("cp %s %s", name, path),
	}, nil
}

// inside reports whether path is under workspace. A dotdir file is not, and
// has no tree to be snapshotted into.
func inside(workspace, path string) bool {
	rel, err := filepath.Rel(workspace, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// PruneSidecars removes sidecars in dir older than keep, and returns how many
// it took. It runs when the directory is next written, so a workspace that
// goes quiet keeps its sidecars until it is used again — the honest cost of
// not running a timer for this.
func PruneSidecars(dir string, keep time.Duration) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	cut := Now().Add(-keep)
	n := 0
	for _, e := range entries {
		if e.IsDir() || !strings.Contains(e.Name(), sidecarMark) {
			continue
		}
		info, err := e.Info()
		if err != nil || info.ModTime().After(cut) {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			n++
		}
	}
	return n
}
