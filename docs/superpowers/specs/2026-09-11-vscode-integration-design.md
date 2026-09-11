# BE-Code VS Code integration — design

Date: 2026-09-11. Status: approved in conversation, pending written review.

## Goal

Let BE-Code, running as the TUI in VS Code's integrated terminal, use the
editor: language-server diagnostics and symbol information, the debugger
(Go via delve, Python via debugpy first), file-change review in an editor
diff, and automatic "what the user is looking at" context. The extension is
a thin adapter so BE-Code stays a portable CLI; any editor could implement
the same server.

## Shape

The VS Code extension hosts a local MCP server. BE-Code connects to it with
the MCP client it already has, over a new TCP transport. Everything the
integration needs is a tool call that BE-Code makes:

- the model calls `ide_*` tools through the normal tool loop;
- BE-Code itself calls `ide_context` before each prompt;
- BE-Code's approval seam calls `ide_review_diff` for file changes.

## Transport and discovery

- MCP JSON-RPC, newline-delimited, over 127.0.0.1 TCP. Go gains
  `mcp.DialTCP(addr, token)` sharing the stdio client's read loop, pending
  map and `CallTool`.
- Lock file `~/.be-code/ide/<pid>.json`:
  `{pid, port, token, workspaceFolders[], ideName, version}`. Written on
  activate, removed on deactivate. Stale files (dead pid) are ignored and
  deleted by BE-Code.
- BE-Code discovers when `TERM_PROGRAM=vscode` or `--ide`; `--no-ide` or
  config `ide.enabled=false` disables. It picks the lock whose workspace
  folder contains the `-C` directory, else the newest. The token is sent in
  `initialize` params; a wrong token is refused.
- TUI prints `VS Code connected: N tools`; bottom line shows an `⌘ ide`
  marker. If the connection drops, IDE tools return a clear error and
  approvals fall back to the TUI.
- IDE tools register as `ide_<name>` (`AttachMCP` gains a prefix option).

## Tool set (exposed by the extension)

Editor and language:

| Tool | Args | Returns |
|---|---|---|
| `ide_context` | – | active file, cursor line, selection range and text (≤2KB), open files, workspace folders |
| `ide_diagnostics` | `path?`, `severity?` (error/warning/all) | `path:line:col severity source: message`, grouped by file, count first |
| `ide_open` | `path`, `line?` | reveals in editor |
| `ide_definition` | `path`, `line`, `col` | definition location(s) via language server |
| `ide_references` | `path`, `line`, `col`, `max?` | `path:line: text` |
| `ide_hover` | `path`, `line`, `col` | type/signature text |

Debug (DAP through the VS Code debug API; one session at a time; start,
continue and step wait for the next stop or exit, 60s cap):

| Tool | Args | Returns |
|---|---|---|
| `ide_debug_configs` | – | launch.json configurations |
| `ide_debug_start` | `config` or `program`+`args`+`type` (go/python) | stop reason and location |
| `ide_debug_breakpoint` | `path`, `line`, `action` (add/remove), `condition?` | breakpoints in that file |
| `ide_debug_continue` | – | next stop |
| `ide_debug_step` | `step` (over/into/out) | next stop with top frames |
| `ide_debug_stack` | `depth?` | `#n function path:line` |
| `ide_debug_variables` | `frame?`, `scope?` | `name = value (type)`, one level nested |
| `ide_debug_evaluate` | `expression`, `frame?` | result |
| `ide_debug_output` | `since?` | console output since cursor, new cursor |
| `ide_debug_stop` | – | ends session |

All results are plain text, workspace-relative paths, capped by the
registry's output limit. The debug console is kept in a ring buffer per
session.

System prompt addition when IDE tools are attached: prefer
`ide_diagnostics` over building to find errors; use
`ide_definition`/`ide_references`/`ide_hover` before guessing at APIs; use
the debugger to verify runtime behaviour instead of adding prints.

## Approvals and automatic context

- `ide_review_diff(path, original, proposed, summary)` is called by the
  approval seam, never by the model. The extension opens a side-by-side
  diff from in-memory content (nothing on disk until accepted), scrolled to
  the change, with a modal: Accept, Reject, Accept all this session. The
  decision returns to BE-Code, which applies or reports rejection exactly
  as today; "accept all" sets the same session flag as the TUI's `a`.
- While waiting, the TUI bottom line shows `reviewing change in VS Code…`
  and Esc still cancels the run. On IDE error or disconnect the TUI modal
  appears. Shell approvals always stay in the terminal. Headless `run`
  never uses the editor.
- Before each prompt BE-Code calls `ide_context` and prepends one line:
  `[editor: path, cursor line N, selection lines A–B]` plus the selected
  text when present and ≤2KB. Omitted when no file is active, for queued
  mid-task messages, and for repair prompts. Shown dimmed in the
  transcript. Config `ide.auto_context=false` disables.

## Extension layout

```
be-code/vscode/
  package.json  tsconfig.json  esbuild.mjs
  src/extension.ts   activate/deactivate, status bar, commands
  src/server.ts      TCP listener, JSON-RPC framing, token, MCP methods
  src/lock.ts
  src/tools/editor.ts  diagnostics.ts  debug.ts  review.ts
  test/              vitest
```

Commands: `BE-Code: Open terminal` (integrated terminal running `be-code`
in the workspace folder), `BE-Code: Show connection status`. Settings:
`be-code.port` (0 = random), `be-code.autoStart` (true). Status bar:
`BE-Code: listening` / `connected`.

Build: `npm install`, `npm run build` (esbuild), `npm run package` (vsce →
`.vsix`). `build.mk` gains a `vscode` target; `release` copies the `.vsix`
into `dist/`. Extension version tracks `tui.PublicVersion`.

## Go side, files touched

`internal/mcp/client.go` (TCP dial, token), `internal/ide/` (new: lock
discovery, connect, context fetch, review call), `internal/tools/mcp_tool.go`
(prefix option), `cmd/root.go` (discovery, `--ide/--no-ide`, config
`ide.enabled`, `ide.auto_context`), `internal/agent` (context note, system
prompt paragraph), `internal/tui` (approval routing, marker, bottom-line note).

## Testing

- Go: TCP transport against a fake server; lock discovery (workspace
  match, newest, stale cleanup, bad token); prefix registration; context
  note formatting/omission; approval routing with fallback.
- Extension (vitest): framing, lock lifecycle, diagnostics formatting,
  wait-for-stop state machine with a mocked session.
- Live, with the user: install the `.vsix`; Go and Python projects; ask for
  diagnostics, set a breakpoint and step, accept a change via the editor
  diff. Scripted as a checklist.

## Out of scope (v1)

Chat panel inside VS Code; running VS Code tasks; Node debugging (works via
DAP but untested); editing through the VS Code workspace edit API (writes
stay on disk with editor auto-reload).
