package ui

import "strings"

// TaskViewer is what TaskLines needs from the working-memory store: the
// whole tree, or one branch of it. It is deliberately narrow — two methods,
// nothing that could mutate the store and nothing that needs the workspace
// root — so both UIs and their tests can share this one renderer without
// depending on the rest of engine.Store.
type TaskViewer interface {
	TreeText() string
	ShowText(id string) string
}

// TaskLines renders the lines "/task" and "/task show <id>" print. "/task
// open" (it needs the workspace root, which TaskViewer does not carry — the
// record lives under the project root, never the dotdir, so a caller must
// resolve that path itself rather than have this renderer guess at it) and
// "/task clear" (it mutates the store) are not handled here; both stay with
// the caller, exactly as 0.10.0 kept clear there.
func TaskLines(eng TaskViewer, args []string) []string {
	if len(args) == 0 || args[0] == "" {
		return splitLines(eng.TreeText())
	}
	switch args[0] {
	case "show":
		if len(args) < 2 || strings.TrimSpace(args[1]) == "" {
			return []string{"usage: /task show <id>"}
		}
		return splitLines(eng.ShowText(args[1]))
	default:
		return []string{"usage: /task [show <id>|open|clear]"}
	}
}

// splitLines is strings.Split guarded against turning "" into a single
// empty line.
func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
