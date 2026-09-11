// Package commands loads user-defined slash commands from
// <workspace>/.becode/commands/*.md and ~/.be-code/commands/*.md — prompt
// templates invoked as /<name> [args], with $ARGS substituted. Workspace
// commands shadow global ones of the same name.
package commands

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/brown-enterprises/be-code/internal/config"
)

// Command is one custom slash command.
type Command struct {
	Name     string
	Template string
	Source   string // "workspace" or "global"
}

// Load gathers commands for a workspace.
func Load(workspaceRoot string) map[string]Command {
	out := map[string]Command{}
	if d, err := config.Dir(); err == nil {
		loadDir(filepath.Join(d, "commands"), "global", out)
	}
	loadDir(filepath.Join(workspaceRoot, ".becode", "commands"), "workspace", out)
	return out
}

func loadDir(dir, source string, out map[string]Command) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil || len(data) == 0 {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		out[name] = Command{Name: name, Template: string(data), Source: source}
	}
}

// Expand renders a command's prompt with arguments substituted. When the
// template has no $ARGS marker, args are appended.
func (c Command) Expand(args string) string {
	if strings.Contains(c.Template, "$ARGS") {
		return strings.ReplaceAll(c.Template, "$ARGS", args)
	}
	if strings.TrimSpace(args) == "" {
		return c.Template
	}
	return c.Template + "\n\n" + args
}

// Names lists command names sorted.
func Names(cmds map[string]Command) []string {
	out := make([]string, 0, len(cmds))
	for n := range cmds {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
