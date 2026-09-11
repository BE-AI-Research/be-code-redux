package agent

// IDEGuidance is appended to the system prompt when editor tools are
// attached, so a small model reaches for the language server and debugger
// instead of grepping and guessing.
const IDEGuidance = `Editor tools are available (ide_*). Prefer ide_diagnostics over running a build to find errors. Before guessing at an API, use ide_definition, ide_references or ide_hover on the symbol. To check runtime behaviour, use the debugger (ide_debug_start with a launch config or program, ide_debug_breakpoint, ide_debug_step, ide_debug_variables, ide_debug_evaluate) instead of adding print statements. A message starting with [editor: ...] tells you which file and lines the user is looking at.`
