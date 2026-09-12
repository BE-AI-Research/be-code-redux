// Package ui implements BE-Code's interfaces: the plain inline REPL
// (this package) and shared rendering helpers used by the TUI as well.
package ui

import (
	"os"
	"strings"
)

// ANSI helpers — plain enough for any terminal, disabled when not a TTY.
var useColor = isTTY()

func isTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && (fi.Mode()&os.ModeCharDevice) != 0
}

func color(code, s string) string {
	if !useColor {
		return s
	}
	return "\x1b[" + code + "m" + s + "\x1b[0m"
}

func dim(s string) string  { return color("2", s) }
func cyan(s string) string { return color("36", s) }
func yell(s string) string { return color("33", s) }
func red(s string) string  { return color("31", s) }
func grn(s string) string  { return color("32", s) }

// ColorizeDiff applies green/red to +/- lines of an uncolored diff preview.
func ColorizeDiff(preview string, enable bool) string {
	if !enable {
		return preview
	}
	lines := strings.Split(preview, "\n")
	for i, l := range lines {
		switch {
		case strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"):
			lines[i] = "\x1b[1m" + l + "\x1b[0m"
		case strings.HasPrefix(l, "+"):
			lines[i] = "\x1b[32m" + l + "\x1b[0m"
		case strings.HasPrefix(l, "-"):
			lines[i] = "\x1b[31m" + l + "\x1b[0m"
		case strings.HasPrefix(l, "@@"):
			lines[i] = "\x1b[36m" + l + "\x1b[0m"
		}
	}
	return strings.Join(lines, "\n")
}

// SlashCommandInfo describes one slash command for the palette and menu.
type SlashCommandInfo struct {
	Name string
	Desc string
	Args bool // takes an argument, so the palette fills the input instead of running
}

// SlashCommandTable is the shared command set (order = palette order).
var SlashCommandTable = []SlashCommandInfo{
	{"/menu", "grouped menu with status", false},
	{"/help", "command reference", false},
	{"/model", "switch model: /model <name>", true},
	{"/models", "pick a model from the backend", false},
	{"/provider", "switch provider: /provider <name>", true},
	{"/sessions", "pick a saved session to resume", false},
	{"/resume", "resume by code or id: /resume <code>", true},
	{"/plan", "read-only planning phase, then approve and execute", true},
	{"/undo", "roll back the last turn's file changes", false},
	{"/verify", "run the workspace's build/lint/test checks", false},
	{"/commit", "commit all changes with a model-written message", false},
	{"/init", "generate BECODE.md project notes", false},
	{"/compact", "summarize older conversation now", false},
	{"/handoff", "show the briefing carried over from a resumed session", false},
	{"/map", "show the repo map", false},
	{"/stats", "requests, tool calls, tokens", false},
	{"/tools", "list available tools", false},
	{"/config", "show effective configuration", false},
	{"/theme", "pick a colour theme, or /theme <name> (dracula, nord, gruvbox, …)", true},
	{"/queue", "list, edit or drop messages queued for the agent: /queue [edit N|drop N]", true},
	{"/copy", "copy selection, last reply, tool output or all: /copy [reply|tool|all]", true},
	{"/clear", "start a fresh session", false},
	{"/quit", "exit (writes the resume briefing)", false},
}

// SlashCommands is the flat name list for autocomplete in both UIs.
var SlashCommands = func() []string {
	out := make([]string, 0, len(SlashCommandTable)+2)
	for _, c := range SlashCommandTable {
		out = append(out, c.Name)
	}
	return append(out, "/exit", "/q")
}()

// SetMono disables ANSI color output (theme "mono" or --plain piping).
func SetMono() { useColor = false }
