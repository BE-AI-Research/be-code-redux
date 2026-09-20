# BE-Code — Editor Bridge

Connects the [BE-Code](https://github.com/BE-AI-Research/be-code) offline coding agent to
VS Code. When `be-code` runs from the integrated terminal it finds this extension and
gains a live view into the editor: diagnostics, symbol lookups (definition/references/
hover), a debugger it can drive (breakpoints, step, variables, evaluate), and in-editor
diff review for every file write — plus a one-line note about the file and selection
you're looking at, folded into each prompt automatically.

## Install

- From a packaged release: open the Extensions view, `...` menu → **Install from VSIX...**,
  and pick `dist/be-code-vscode-<version>.vsix` (built by `make -f build.mk vscode` or
  `make -f build.mk release` in the `be-code` repo).
- From source: `npm install && npm run build` in this directory, then use VS Code's
  "Run Extension" launch configuration to try it in an Extension Development Host.

## Commands

- **BE-Code: Open terminal** — opens an integrated terminal already positioned so a
  `be-code` run in it will discover this extension's bridge.
- **BE-Code: Show connection status** — reports whether the bridge is listening and
  how many BE-Code sessions are attached.

## Settings

- `be-code.port` — port for the editor bridge (`0` picks a random free port).
- `be-code.autoStart` — start the bridge automatically when VS Code starts (default `true`).

The status bar shows "BE-Code: listening" while the bridge is up. A connected BE-Code
session writes a lock file under `~/.be-code/ide/` (pid, port, token, workspace) that it
uses to find and authenticate to the bridge; shell command approvals still happen in the
terminal, not in VS Code.
