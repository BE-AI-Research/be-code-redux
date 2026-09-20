# Visual Studio Extension — Design

**Date:** 2026-09-20
**Status:** approved for planning

## 1. Why

BE-Code's editor bridge gives the model the editor's own view of a project: the active file and selection, diagnostics, the debugger, and a side-by-side diff review of every proposed write. It exists only for VS Code (`vscode/`, TypeScript). People who work in Visual Studio 2022 or 2026 get none of it. The harness side of the bridge is editor-agnostic, so this is one new extension plus one small change to how the harness decides to attach.

## 2. Decisions taken

- **Topology:** BE-Code runs natively on the same Windows machine as Visual Studio (`be-code-windows-amd64.exe`), on the same checkout. WSL and remote attach are out of scope.
- **Scope:** full parity with the VS Code extension: the same tools, the same names, the same input shapes, so the model and the harness see no difference.
- **Extension model:** one classic in-process VSIX (`AsyncPackage`, VSSDK). The newer out-of-process `VisualStudio.Extensibility` model does not reach the debugger or the diff viewer well enough for parity, and does not cover older 2022 builds.
- **Versions:** one package declaring `[17.6,19.0)`, which covers Visual Studio 2022 (17.6 and later) and 2026 (18.x), for `amd64` and `arm64`. *(First written as `[17.0,19.0)`; the floor rose to 17.6 with the host design's §1.1, agreed with the owner: the bridge runs inside `devenv.exe` against Visual Studio's own `System.Text.Json`.)*
- **Build:** the Visual Studio layer can only be built on Windows. Everything else is built and tested on Linux with the .NET SDK (10.0.401 is installed here).

## 3. The wire contract (unchanged, restated so the C# side can be checked against it)

The extension is a server; the harness is the client.

- **Transport:** TCP on `127.0.0.1`, an ephemeral port. Newline-delimited JSON-RPC 2.0, one message per line. A message with no `id` is a notification and gets no reply. Requests on one connection are read and dispatched in order; `initialize`, `tools/list`, `ping` and every refusal are answered inline, in that order. A `tools/call` runs on its own and is answered when it finishes, carrying its own `id`, so replies to tool calls may arrive out of request order. *(Amended 2026-09-20 during implementation: this line first read "handled in order", restating what `vscode/src/server.ts` did. That was a defect, not a contract — the harness sends `review_cancel` on the same connection as the `review_diff` it withdraws, so a server that finishes one request before reading the next can never process the cancel. Both extensions are fixed to this rule.)* A connection that closes cancels its in-flight tool calls.
- **`initialize`:** `params.auth.token` must equal the lock file's token, else error `-32001 "bad token"` and the socket is closed. Reply: `{protocolVersion:"2024-11-05", capabilities:{tools:{}}, serverInfo:{name, version}}`. Every other method before a successful `initialize` is refused.
- **`tools/list`:** `{tools:[{name, description, inputSchema}]}`.
- **`tools/call`:** `params.name`, `params.arguments`; reply `{content:[{type:"text", text}], isError}`. A tool failure is `isError:true` with the message as text, never a JSON-RPC error.
- **Lock file:** `<home>/.be-code/ide/<pid>.json`, written atomically, removed on unload: `{pid, port, token, workspaceFolders, ideName, version}`. `ideName` is `"visualstudio"`. `workspaceFolders` is the solution directory plus each loaded project's directory, republished when the solution changes. The token is 32 random bytes, hex.

## 4. Tools (parity list)

Eighteen tools, names and input schemas taken verbatim from `vscode/src/tools/*.ts`, which stays the single source of truth:

`context`, `open`, `definition`, `references`, `hover`, `diagnostics`, `review_diff`, `review_cancel`, `debug_configs`, `debug_start`, `debug_breakpoint`, `debug_continue`, `debug_step`, `debug_stack`, `debug_variables`, `debug_evaluate`, `debug_output`, `debug_stop`.

Where Visual Studio differs, the tool keeps its name and shape and adapts its meaning:

- **`debug_configs`** lists the solution's startup projects and launch profiles rather than `launch.json` entries.
- **`debug_start`** starts a listed configuration; the VS Code extension's ad-hoc Go and Python launch shapes are answered with a clear `isError` saying Visual Studio debugs the startup project.
- **`definition` / `references` / `hover`** go through the language service where Roslyn provides one (C#, VB), and return a clear "not available for this file type" otherwise, never an empty success.
- **`review_diff`** opens Visual Studio's own difference viewer between the file on disk and the proposed content, with accept, accept-all and reject in an info bar, and replies with the same JSON the VS Code extension does: `{"decision":"accept"|"reject"|"accept_all"|"cancelled"}`. `review_cancel` closes it and replies `{"cancelled":true|false}`. Both are **hidden** tools: callable, never advertised by `tools/list`, because the harness drives them and the model must not. Accept-all for the connection is dropped when the connection closes, as in VS Code.

## 5. Structure

```
visualstudio/
  BECode.VisualStudio.sln
  src/BECode.Bridge/            netstandard2.0, no Visual Studio reference
    LineFramer, JsonRpc, BridgeServer      the wire
    LockFile                               write, republish, remove
    ToolRegistry + the 18 tool definitions names, schemas, argument parsing
    IEditorHost                            what the tools need from an editor
  src/BECode.Bridge.FakeHost/   net8.0 console app: the bridge over a scripted IEditorHost
  src/BECode.VisualStudio/      net472 VSIX: AsyncPackage + VisualStudioEditorHost
  test/BECode.Bridge.Tests/     xunit, runs on Linux
  build.ps1                     -> BECode.VisualStudio.vsix (Windows only)
  README.md
```

`IEditorHost` is the seam. Every tool is written once in `BECode.Bridge` against it; `VisualStudioEditorHost` implements it over DTE, the text manager, the Error List, `EnvDTE.Debugger` and `IVsDifferenceService`, marshalling to the UI thread with `JoinableTaskFactory`. Nothing in `BECode.Bridge` may reference a Visual Studio assembly, which is what lets it build and test here.

## 6. The harness change

`cmd/root.go:attachIDE` auto-attaches only when `TERM_PROGRAM=vscode`; everywhere else it needs `--ide`. Visual Studio's terminals do not set that variable. The rule becomes: attach automatically when `ide.enabled` is on **and** either `TERM_PROGRAM=vscode` **or** a live lock whose `ideName` is `visualstudio` covers the workspace. `--ide` and `--no-ide` keep their meaning. `internal/review`'s `auto` mode, which treats a lone `vscode`-labelled client as "the editor is the only screen", is unchanged: a Visual Studio user is in a separate terminal, so `auto` resolves to `both` and the first answer wins, which is the right behaviour.

## 7. Proof

1. **Unit tests (here):** framing across split reads, auth refusal, ordering, tool argument parsing, lock-file write/republish/remove and its atomicity.
2. **Contract test (here):** a Go test starts `BECode.Bridge.FakeHost` with `dotnet`, points the real `internal/ide` client at its lock file, and calls every one of the eighteen tools, asserting names, schemas and reply shapes against the VS Code extension's. Skipped with a clear message when `dotnet` is absent, so `make verify` stays green on machines without it.
3. **Schema parity (here):** a test compares the C# tool list with the list extracted from `vscode/src/tools/*.ts`, so the two extensions cannot drift.
4. **Windows checklist (the owner):** build with `build.ps1`, install into 2022 and 2026, and walk a written checklist: lock file appears and disappears, `be-code` attaches without `--ide`, each tool family works, a write is reviewed in the diff viewer.

Nothing in the Visual Studio layer is claimed to work until item 4 has been run; the README says so until it has.

## 8. Out of scope

WSL and remote attach; Visual Studio for Mac; build, test-runner and solution-structure tools beyond parity; publishing to the Visual Studio Marketplace.
