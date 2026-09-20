package agent

// IDEGuidance is appended to the system prompt when editor tools are
// attached, so a small model reaches for the language server and debugger
// instead of grepping and guessing.
const IDEGuidance = `Editor tools are available (ide_*). Prefer ide_diagnostics over running a build to find errors. Before guessing at an API, use ide_definition, ide_references or ide_hover on the symbol. To check runtime behaviour, use the debugger (ide_debug_start with a launch config or program, ide_debug_breakpoint, ide_debug_step, ide_debug_variables, ide_debug_evaluate) instead of adding print statements. A message starting with [editor: ...] tells you which file and lines the user is looking at.`

// visualStudioGuidance is IDEGuidance for Visual Studio, which debugs a
// startup project: it has no launch.json and refuses the program form, so
// telling the model to reach for it would cost a failed call every time.
const visualStudioGuidance = `Editor tools are available (ide_*). Prefer ide_diagnostics over running a build to find errors. Before guessing at an API, use ide_definition, ide_references or ide_hover on the symbol. To check runtime behaviour, use the debugger (ide_debug_configs lists what can be debugged; ide_debug_start with one of those names, or with nothing for the current startup project; then ide_debug_breakpoint, ide_debug_step, ide_debug_variables, ide_debug_evaluate) instead of adding print statements. A message starting with [editor: ...] tells you which file and lines the user is looking at.`

// IDEGuidanceFor is the guidance for the editor a lock file names.
func IDEGuidanceFor(ideName string) string {
	if ideName == "visualstudio" {
		return visualStudioGuidance
	}
	return IDEGuidance
}

// EditorLabel is what to call the attached editor in front of the user. The
// lock file's ideName is an identifier, not a name; an editor that does not
// say is VS Code, the only one there was before lock files carried it.
func EditorLabel(ideName string) string {
	switch ideName {
	case "", "ide", "vscode":
		return "VS Code"
	case "visualstudio":
		return "Visual Studio"
	}
	return ideName
}
