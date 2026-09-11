// Package gitctx gives the agent lightweight git awareness: branch and
// working-tree state in the prompt, and model-written commits on demand.
// Everything degrades to no-ops outside a git repository or without the
// git binary — BE-Code never requires git.
package gitctx

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/tools"
)

func git(ctx context.Context, root, args string) (string, error) {
	out, err := tools.RunShell(ctx, root, "git "+args, 30*time.Second)
	return strings.TrimSpace(out), err
}

// IsRepo reports whether root is inside a git work tree.
func IsRepo(ctx context.Context, root string) bool {
	out, err := git(ctx, root, "rev-parse --is-inside-work-tree")
	return err == nil && out == "true"
}

// Summary returns a compact state block for the system prompt, or "".
func Summary(ctx context.Context, root string) string {
	if !IsRepo(ctx, root) {
		return ""
	}
	branch, _ := git(ctx, root, "branch --show-current")
	status, _ := git(ctx, root, "status --porcelain")
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
	if _, err := git(ctx, root, "add -A"); err != nil {
		return "", err
	}
	// Refuse empty commits gracefully.
	if out, _ := git(ctx, root, "status --porcelain"); out == "" {
		return "", fmt.Errorf("nothing to commit")
	}
	msg := strings.ReplaceAll(message, `"`, `'`)
	if _, err := git(ctx, root, fmt.Sprintf("commit -m %q", msg)); err != nil {
		return "", err
	}
	return git(ctx, root, "log -1 --oneline")
}

// DiffStat returns a bounded diff of uncommitted changes for commit-message
// generation and reviewer routing.
func DiffStat(ctx context.Context, root string) string {
	if !IsRepo(ctx, root) {
		return ""
	}
	stat, _ := git(ctx, root, "diff --stat HEAD")
	diff, _ := git(ctx, root, "diff HEAD")
	const capBytes = 8 * 1024
	if len(diff) > capBytes {
		diff = diff[:capBytes] + "\n... [diff truncated]"
	}
	return stat + "\n\n" + diff
}
