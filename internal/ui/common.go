// Package ui implements BE-Code's interfaces: the plain inline REPL
// (this package) and shared rendering helpers used by the TUI as well.
package ui

import (
	"os"
	"strings"
	"unicode"
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
	{"/init", "map the workspace and write BECODE.md project notes", false},
	{"/compact", "summarize older conversation now", false},
	{"/handoff", "show the briefing carried over from a resumed session", false},
	{"/map", "show the repo map", false},
	{"/stats", "session metrics: context, model cost, tools, tasks", false},
	{"/tools", "list available tools", false},
	{"/config", "show effective configuration", false},
	{"/theme", "pick this terminal's colour theme, or /theme <name> · /theme default <name>", false},
	{"/review", "show or set where file changes are reviewed: /review [auto|editor|tui|both]", true},
	{"/coworkers", "list co-working models and how often each was consulted", false},
	{"/consult", "ask a co-working model directly: /consult [name] <question>", true},
	{"/task", "task record: /task [show <id>|open|clear]", true},
	{"/notes", "durable project notes: /notes [add <text>|drop N|clear]", true},
	{"/queue", "list, edit or drop messages queued for the agent: /queue [edit N|drop N]", true},
	{"/copy", "copy selection, last reply, tool output or all: /copy [reply|tool|all]", true},
	{"/clients", "list terminals attached to this session", false},
	{"/detach", "detach this terminal (the session keeps running)", false},
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

// busySafe lists the slash commands that never touch the running agent and
// so may run in the middle of a turn: settings, information, copying,
// queue editing, and leaving (quit cancels the run first).
var busySafe = map[string]bool{
	"/menu": true, "/help": true, "/theme": true, "/config": true, "/stats": true, "/tools": true,
	"/map": true, "/handoff": true, "/copy": true, "/queue": true, "/clients": true, "/detach": true,
	"/review": true, "/quit": true, "/exit": true, "/q": true,
	// A consultation is the co-worker's own scratch agent: it never touches
	// the primary's history, so both may run mid-turn.
	"/coworkers": true, "/consult": true,
	// Listings and edits of the store; the agent reads it under its own lock.
	"/task": true, "/notes": true,
}

// BusySafeCommand reports whether a slash command line may run while the
// agent is busy. Everything else waits until the turn ends.
func BusySafeCommand(line string) bool {
	f := strings.Fields(line)
	return len(f) > 0 && busySafe[f[0]]
}

// dropWord returns what follows the first whitespace-delimited word,
// trimmed; "" when there is nothing after it.
func dropWord(s string) string {
	s = strings.TrimSpace(s)
	i := strings.IndexFunc(s, unicode.IsSpace)
	if i < 0 {
		return ""
	}
	return strings.TrimSpace(s[i:])
}

// NoteArgument is the text of a "/notes add ..." line: everything after the
// add token, whatever spacing was typed, so "/notes  add  spaced text"
// notes "spaced text" and never a stray "add".
func NoteArgument(line string) string { return dropWord(dropWord(line)) }
