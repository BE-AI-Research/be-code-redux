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

// KeepSidecarsFor is how long a sidecar is kept, set from
// work.keep_overwritten. Pruning happens beside a file as one is written
// there, which is the only directory that ever holds them.
var KeepSidecarsFor = 168 * time.Hour

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
	// The git route only exists for a file a snapshot can actually hold.
	// SnapshotTo runs `git add -A`, which honours .gitignore, so an ignored
	// file is not in the commit — and `.be-code/` is ignored in BE-Code's own
	// repository, which makes a task document (the spec's own worked example
	// of a conflict) exactly this case. Reporting a branch that cannot hold
	// the file would lose the old version while promising it is recoverable,
	// which is the one thing §12 forbids. Checked before the snapshot so an
	// ignored file does not pay for a whole-tree commit it cannot use.
	if inside(workspace, path) && gitctx.IsRepo(ctx, workspace) && !gitctx.Ignored(ctx, workspace, path) {
		rel, rerr := filepath.Rel(workspace, path)
		if rerr != nil {
			rel = "."
		}
		branch, err := gitctx.SnapshotTo(ctx, workspace, gitctx.SnapshotPrefixOverwrite,
			"be-code: before overwriting a change this session did not make")
		// And verify rather than assume: anything else that keeps a path out
		// of the commit leaves the same empty promise.
		if err == nil && gitctx.InTree(ctx, workspace, branch, filepath.ToSlash(rel)) {
			return Restore{
				Kind:   RestoreGit,
				Branch: branch,
				HowTo: fmt.Sprintf("git restore --source=%s --staged --worktree -- %s",
					branch, filepath.ToSlash(rel)),
			}, nil
		}
		// Fall through: a repository that cannot snapshot this path still has
		// a directory the old bytes can go in.
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
	pruneBeside(path, KeepSidecarsFor)
	return Restore{
		Kind:    RestoreSidecar,
		Sidecar: name,
		HowTo:   fmt.Sprintf("cp %s %s", name, path),
	}, nil
}

// pruneBeside drops sidecars older than keep in the directory a sidecar was
// just written to. Open pruned ~/.be-code/work instead, where no sidecar is
// ever written, so work.keep_overwritten did nothing at all.
func pruneBeside(path string, keep time.Duration) {
	if keep > 0 {
		PruneSidecars(filepath.Dir(path), keep)
	}
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
