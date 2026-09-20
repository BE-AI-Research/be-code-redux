// Package gitctx gives the agent lightweight git awareness: branch and
// working-tree state in the prompt, and model-written commits on demand.
// Everything degrades to no-ops outside a git repository or without the
// git binary — BE-Code never requires git.
package gitctx

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/procattr"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// git runs one git command as an argument vector, never as a shell line.
// Revisions, paths and — above all — commit messages come from the model,
// and no quoting scheme is safe across sh and PowerShell: a message
// containing $(...) would otherwise execute.
func git(ctx context.Context, root string, args ...string) (string, error) {
	out, err := tools.RunArgv(ctx, root, 30*time.Second, "git", args...)
	return strings.TrimSpace(out), err
}

// IsRepo reports whether root is inside a git work tree.
func IsRepo(ctx context.Context, root string) bool {
	out, err := git(ctx, root, "rev-parse", "--is-inside-work-tree")
	return err == nil && out == "true"
}

// Summary returns a compact state block for the system prompt, or "".
func Summary(ctx context.Context, root string) string {
	if !IsRepo(ctx, root) {
		return ""
	}
	branch, _ := git(ctx, root, "branch", "--show-current")
	status, _ := git(ctx, root, "status", "--porcelain")
	lines := strings.Split(status, "\n")
	if status == "" {
		lines = nil
	}
	shown := lines
	extra := ""
	if len(shown) > 20 {
		shown = shown[:20]
		extra = fmt.Sprintf("\n... %d more changed files", len(lines)-20)
	}
	state := "clean working tree"
	if len(lines) > 0 {
		state = strings.Join(shown, "\n") + extra
	}
	return fmt.Sprintf("git branch: %s\n%s", branch, state)
}

// Commit stages everything and commits with message. Returns the short log.
func Commit(ctx context.Context, root, message string) (string, error) {
	if !IsRepo(ctx, root) {
		return "", fmt.Errorf("not a git repository")
	}
	if _, err := git(ctx, root, "add", "-A"); err != nil {
		return "", err
	}
	// Refuse empty commits gracefully.
	if out, _ := git(ctx, root, "status", "--porcelain"); out == "" {
		return "", fmt.Errorf("nothing to commit")
	}
	// The message is one argv element: whatever the model wrote is the
	// subject, never something a shell could reinterpret.
	if _, err := git(ctx, root, "commit", "-m", message); err != nil {
		return "", err
	}
	return git(ctx, root, "log", "-1", "--oneline")
}

// DiffStat returns a bounded diff of uncommitted changes for commit-message
// generation and reviewer routing.
func DiffStat(ctx context.Context, root string) string {
	if !IsRepo(ctx, root) {
		return ""
	}
	stat, _ := git(ctx, root, "diff", "--stat", "HEAD")
	diff, _ := git(ctx, root, "diff", "HEAD")
	const capBytes = 8 * 1024
	if len(diff) > capBytes {
		diff = diff[:capBytes] + "\n... [diff truncated]"
	}
	return stat + "\n\n" + diff
}

// SnapshotPrefix is the branch namespace Snapshot creates under.
const SnapshotPrefix = "be-code/pre-init/"

// gitEnv runs git directly (not through a shell) with extra environment, for
// the few operations that need a private index.
func gitEnv(ctx context.Context, root string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = root
	procattr.Hide(cmd) // this runs before every request; on Windows it would flash a console each time
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	s := strings.TrimSpace(string(out))
	if err != nil {
		if s != "" {
			return s, fmt.Errorf("git %s: %s", args[0], s)
		}
		return s, fmt.Errorf("git %s: %w", args[0], err)
	}
	return s, nil
}

// Snapshot records the whole working tree — tracked and untracked files alike,
// honouring .gitignore — as one commit on a new branch under SnapshotPrefix
// (stamped with the current time), and returns the branch name. The user's
// checkout is left exactly as it was: the work is done in a private index
// (GIT_INDEX_FILE), the commit is written with commit-tree, and only the new
// ref is created — HEAD, the real index and the working tree never move. In a
// repository with no commits yet the snapshot simply has no parent. Every call
// makes a fresh branch, so the earliest one stays as the initial restore point.
func Snapshot(ctx context.Context, root, message string) (string, error) {
	if !IsRepo(ctx, root) {
		return "", fmt.Errorf("not a git repository")
	}
	tmp, err := os.CreateTemp("", "be-code-index-*")
	if err != nil {
		return "", err
	}
	tmp.Close()
	os.Remove(tmp.Name()) // git wants to create it itself
	defer os.Remove(tmp.Name())
	env := []string{"GIT_INDEX_FILE=" + tmp.Name()}

	head, headErr := gitEnv(ctx, root, nil, "rev-parse", "--verify", "-q", "HEAD")
	if headErr == nil && head != "" {
		if _, err := gitEnv(ctx, root, env, "read-tree", "HEAD"); err != nil {
			return "", err
		}
	}
	if _, err := gitEnv(ctx, root, env, "add", "-A", "--", "."); err != nil {
		return "", err
	}
	tree, err := gitEnv(ctx, root, env, "write-tree")
	if err != nil {
		return "", err
	}
	args := []string{"commit-tree", tree, "-m", message}
	if headErr == nil && head != "" {
		args = append(args, "-p", head)
	}
	// A restore point must not depend on a configured identity (a fresh
	// machine, a scratch repo): fall back to a fixed one only when none is set.
	var ident []string
	if _, err := gitEnv(ctx, root, nil, "config", "user.email"); err != nil {
		ident = []string{
			"GIT_AUTHOR_NAME=be-code", "GIT_AUTHOR_EMAIL=be-code@localhost",
			"GIT_COMMITTER_NAME=be-code", "GIT_COMMITTER_EMAIL=be-code@localhost",
		}
	}
	commit, err := gitEnv(ctx, root, ident, args...)
	if err != nil {
		return "", err
	}
	branch := SnapshotPrefix + time.Now().Format("20060102-150405")
	for i := 2; ; i++ {
		if _, err := gitEnv(ctx, root, nil, "rev-parse", "--verify", "-q", "refs/heads/"+branch); err != nil {
			break // free
		}
		branch = fmt.Sprintf("%s%s-%d", SnapshotPrefix, time.Now().Format("20060102-150405"), i)
	}
	if _, err := gitEnv(ctx, root, nil, "update-ref", "refs/heads/"+branch, commit); err != nil {
		return "", err
	}
	return branch, nil
}

// Head is the full HEAD commit hash, or "" outside a repository or before
// the first commit. Any hex hash is accepted, not just a 40-character
// sha1, so a sha256 repository still records a baseline.
func Head(ctx context.Context, root string) string {
	if !IsRepo(ctx, root) {
		return ""
	}
	out, err := git(ctx, root, "rev-parse", "HEAD")
	if err != nil || !isHex(out) {
		return ""
	}
	return out
}

// isHex reports whether s is a non-empty run of hex digits.
func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

// Porcelain is `git status --porcelain`, or "" outside a repository.
func Porcelain(ctx context.Context, root string) string {
	if !IsRepo(ctx, root) {
		return ""
	}
	out, _ := git(ctx, root, "status", "--porcelain")
	return out
}
