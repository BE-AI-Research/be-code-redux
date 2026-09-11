# VS Code editor bridge — live checklist

1. make -f build.mk release; install dist/be-code-<version>.vsix (Extensions → ... → Install from VSIX); reload window.
2. Status bar shows "BE-Code: listening". Run "BE-Code: Open terminal". Expect "VS Code connected: 16 tools" and the ⌘ ide marker.
3. Go project: introduce a compile error; ask "what errors are there?" → expect ide_diagnostics with the error line.
4. Ask "where is X defined and who calls it?" → expect ide_definition / ide_references.
5. Ask "set a breakpoint at main.go:NN, start debugging with the 'Launch' config, and tell me the value of Y when it stops" → expect debug_breakpoint, debug_start, debug_variables.
6. Python project (WorldSim): same as 5 with a debugpy launch config.
7. Ask for a small file edit → expect an editor diff and the Accept/Reject dialog; Reject once, then Accept.
8. Select some lines, ask "explain this" → expect the [editor: …] note in the transcript.
9. Close VS Code's window with be-code running → expect the "editor bridge disconnected" behaviour: ide_* calls fail clearly, the next file change asks in the TUI.
