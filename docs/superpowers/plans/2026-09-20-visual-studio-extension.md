# Visual Studio Extension Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give Visual Studio 2022 and 2026 the same editor bridge VS Code has, as one VSIX whose protocol layer is built and proven on Linux and whose thin Visual Studio layer is built on Windows.

**Architecture:** `BECode.Bridge` (netstandard2.0, no Visual Studio reference) is the wire: line-framed JSON-RPC over localhost TCP, token auth, the lock file, and all eighteen tools written once against an `IEditorHost` interface. Tool names and schemas come from one checked-in manifest that both extensions are tested against, so they cannot drift. `BECode.VisualStudio` (net472 VSIX) implements `IEditorHost` over DTE, the Error List, the debugger and the difference viewer. A Go contract test drives the real `internal/ide` client against the bridge running over a fake host.

**Tech Stack:** C# (.NET SDK 10.0.401 here; netstandard2.0, net8.0, net472), xunit, `System.Text.Json`; Visual Studio SDK (Windows only); Go 1.22+; the existing vitest suite in `vscode/`.

**Spec:** `docs/superpowers/specs/2026-09-20-visual-studio-extension-design.md` — read it first.

## Global Constraints

- **Wire contract is fixed.** TCP on `127.0.0.1`, ephemeral port, newline-delimited JSON-RPC 2.0. No `id` means a notification: no reply. Requests on one connection are handled in order. `initialize` needs `params.auth.token` equal to the lock's token, else error `-32001` `"bad token"` and the socket closes; any other method before a successful `initialize` is refused with `-32002` `"not initialized"`. `initialize` replies `{protocolVersion:"2024-11-05",capabilities:{tools:{}},serverInfo:{name:"be-code-visualstudio",version}}`. `tools/call` replies `{content:[{type:"text",text}],isError}`; a tool failure is `isError:true`, never a JSON-RPC error. Unknown tool: `isError:true`, text `unknown tool <name>`.
- **Lock file:** `<home>/.be-code/ide/<pid>.json`, `{pid,port,token,workspaceFolders,ideName:"visualstudio",version}`, written atomically (temp file then rename), mode 0600 where the platform has modes, removed on unload. Token: 32 random bytes, lower-case hex.
- **Hidden tools:** `review_diff` and `review_cancel` are callable and never listed.
- **`BECode.Bridge` never references a Visual Studio assembly.** That is what lets it build and test on Linux.
- **No network beyond localhost. No telemetry.**
- **Honesty about what is unproven:** the Visual Studio layer cannot be compiled on this machine. Nothing in it is claimed to work; the README says so until the owner's Windows checklist has been run.
- `make -f build.mk verify` stays green on a machine without `dotnet`: anything needing it is skipped with a clear message.
- Commit trailers on every commit:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9
  ```

---

## File Structure

| path | responsibility |
|---|---|
| `vscode/tools.manifest.json` | every tool's `name`, `description`, `inputSchema`, `hidden` — the single source of truth |
| `vscode/test/manifest.test.ts` | the VS Code registry equals the manifest |
| `visualstudio/BECode.VisualStudio.sln` | the three Linux-buildable projects (the VSIX project is in a Windows-only solution filter) |
| `visualstudio/src/BECode.Bridge/` | `LineFramer.cs`, `JsonRpc.cs`, `BridgeServer.cs`, `LockFile.cs`, `ToolManifest.cs`, `ToolRegistry.cs`, `IEditorHost.cs`, `Tools/*.cs` |
| `visualstudio/src/BECode.Bridge.FakeHost/` | console app: the bridge over a scripted `IEditorHost`, for the contract test |
| `visualstudio/test/BECode.Bridge.Tests/` | xunit |
| `visualstudio/src/BECode.VisualStudio/` | the VSIX: `BECodePackage.cs`, `VisualStudioEditorHost.cs`, `ReviewInfoBar.cs`, `source.extension.vsixmanifest` |
| `visualstudio/build.ps1`, `visualstudio/README.md`, `visualstudio/WINDOWS-CHECKLIST.md` | build and validation on Windows |
| `internal/ide/contract_test.go` | the Go client against the C# bridge |
| `cmd/root.go` | the attach rule |

---

### Task 1: The tool manifest, shared by both extensions

**Files:**
- Create: `vscode/tools.manifest.json`, `vscode/test/manifest.test.ts`

**Interfaces:**
- Produces: `vscode/tools.manifest.json`, a JSON array of `{name, description, inputSchema, hidden}` in registration order, eighteen entries, `hidden:true` on `review_diff` and `review_cancel`. Task 3 embeds this file; Task 4's contract test compares against it.

- [ ] **Step 1: Write the failing test** — `vscode/test/manifest.test.ts` builds a `ToolRegistry`, registers every tool family exactly as `extension.ts` does (against the existing `vscode` mock), and asserts deep equality with the parsed manifest, including `hidden` and order. Read `extension.ts` for the registration calls and `test/review.test.ts` for how the mock is used. The registry's `list()` filters hidden tools, so add a small `all()` accessor to `ToolRegistry` returning every definition without its handler.
- [ ] **Step 2: Run it** — `cd vscode && npx vitest run test/manifest.test.ts`. Expected: FAIL, manifest missing.
- [ ] **Step 3: Generate the manifest** from that same registry (a one-off script or the test in update mode), and commit the file. Review it by eye: eighteen entries, two hidden.
- [ ] **Step 4: Run the whole vscode suite** — `cd vscode && npm test`. Expected: PASS.
- [ ] **Step 5: Commit** — `vscode: a tool manifest, the single source of truth both editor extensions are tested against`.

---

### Task 2: The wire — framing, JSON-RPC, the server, the lock file

**Files:**
- Create: `visualstudio/BECode.VisualStudio.sln`, `visualstudio/src/BECode.Bridge/{BECode.Bridge.csproj,LineFramer.cs,JsonRpc.cs,BridgeServer.cs,LockFile.cs}`, `visualstudio/test/BECode.Bridge.Tests/{BECode.Bridge.Tests.csproj,FramerTests.cs,ServerTests.cs,LockFileTests.cs}`

**Interfaces:**
- Produces:

```csharp
public sealed class LineFramer { public IEnumerable<string> Push(ReadOnlySpan<byte> chunk); }

public interface IToolDispatcher {
    IReadOnlyList<ToolInfo> List();                       // never includes hidden tools
    Task<ToolResult> CallAsync(string name, JsonElement args, object connection, CancellationToken ct);
    void ConnectionClosed(object connection);
}
public sealed record ToolInfo(string Name, string Description, JsonElement InputSchema);
public sealed record ToolResult(string Text, bool IsError);

public sealed class BridgeServer : IAsyncDisposable {
    public BridgeServer(IToolDispatcher tools, string token, string version);
    public Task<int> StartAsync(int port = 0);           // returns the bound port, 127.0.0.1 only
    public int ConnectionCount { get; }
}

public sealed record LockInfo(int Pid, int Port, string Token, IReadOnlyList<string> WorkspaceFolders, string IdeName, string Version);
public static class LockFile {
    public static string NewToken();                                  // 32 random bytes, lower-case hex
    public static string PathFor(string home, int pid);               // <home>/.be-code/ide/<pid>.json
    public static Task WriteAsync(string home, LockInfo info);        // atomic: temp then move
    public static void Remove(string home, int pid);                  // missing file is not an error
}
```

- [ ] **Step 1: Write the failing tests.** They must pin the constraints, not just exercise happy paths:
  - `FramerTests`: a message split across three `Push` calls; two messages in one chunk; `\r\n` endings; a multi-byte UTF-8 character split across chunks; an empty line yields nothing.
  - `ServerTests` (real sockets on `127.0.0.1`): wrong token gets `-32001` `"bad token"` and the socket is closed; `tools/list` before `initialize` gets `-32002`; a notification gets no reply (send one then a request and assert exactly one line comes back); `initialize` reply shape exactly as the Global Constraints give it; `tools/call` wraps a result as `{content:[{type:"text",text}],isError}`; an unknown tool is `isError:true` with `unknown tool <name>`; **ordering**: with a dispatcher whose first call delays 200 ms, two pipelined calls reply in request order; a thrown exception in a tool is `isError:true`, not a JSON-RPC error; `ConnectionClosed` is called when the client disconnects; the listener is bound to loopback only.
  - `LockFileTests` (temp dir as home): written file has exactly the six keys with the JSON names the Go side reads (`pid`, `port`, `token`, `workspaceFolders`, `ideName`, `version`); a second write replaces atomically and leaves no temp file; `Remove` of a missing file does not throw; `NewToken` is 64 lower-case hex characters and two calls differ.
- [ ] **Step 2: Run them** — `cd visualstudio && dotnet test`. Expected: FAIL, types missing.
- [ ] **Step 3: Implement.** `BECode.Bridge.csproj` targets `netstandard2.0` with `LangVersion` latest and a `System.Text.Json` package reference; the test project targets `net8.0`. One accept loop; per connection a read loop feeding `LineFramer` and a **single sequential processing task** (a `Channel<string>` or a chained `Task`) so replies keep request order; writes to a socket are serialised.
- [ ] **Step 4: Run the tests** — `cd visualstudio && dotnet test`. Expected: PASS.
- [ ] **Step 5: Commit** — `visualstudio: the bridge's wire — line-framed JSON-RPC over loopback, token auth, the lock file`.

---

### Task 3: The tools, written once against `IEditorHost`

**Files:**
- Create: `visualstudio/src/BECode.Bridge/{IEditorHost.cs,ToolManifest.cs,ToolRegistry.cs}`, `visualstudio/src/BECode.Bridge/Tools/{EditorTools.cs,DiagnosticsTools.cs,DebugTools.cs,ReviewTools.cs}`, `visualstudio/test/BECode.Bridge.Tests/{ToolTests.cs,ManifestParityTests.cs,FakeEditorHost.cs}`
- Modify: `BECode.Bridge.csproj` (embed `../../../vscode/tools.manifest.json` as a resource)

**Interfaces:**
- Consumes: the manifest (Task 1), `IToolDispatcher`, `ToolInfo`, `ToolResult` (Task 2).
- Produces:

```csharp
public interface IEditorHost {
    Task<EditorContext> GetContextAsync(CancellationToken ct);                       // active file, selection, open files, workspace folders
    Task OpenAsync(string path, int? line, CancellationToken ct);
    Task<IReadOnlyList<Location>?> DefinitionAsync(string path, int line, int col, CancellationToken ct);   // null = not available for this file type
    Task<IReadOnlyList<Location>?> ReferencesAsync(string path, int line, int col, int max, CancellationToken ct);
    Task<string?> HoverAsync(string path, int line, int col, CancellationToken ct);
    Task<IReadOnlyList<Diagnostic>> DiagnosticsAsync(string? path, string severity, CancellationToken ct);
    IDebugHost Debug { get; }
    Task<ReviewDecision> ReviewDiffAsync(ReviewRequest request, CancellationToken ct);
    Task<bool> ReviewCancelAsync(string path);
}
public enum ReviewDecision { Accept, Reject, AcceptAll, Cancelled }
public sealed class ToolRegistry : IToolDispatcher { public ToolRegistry(IEditorHost host); }
```

`IDebugHost` mirrors the ten `debug_*` tools one method each. `ToolRegistry` takes names, descriptions and schemas **from the embedded manifest** and binds a handler to each name; a manifest entry with no handler, or a handler with no manifest entry, throws at construction.

- [ ] **Step 1: Write the failing tests.**
  - `ManifestParityTests`: every manifest entry has a handler and vice versa; `List()` omits exactly the hidden ones; schemas in `List()` are byte-equal (after JSON normalisation) to the manifest's.
  - `ToolTests` over `FakeEditorHost`: required arguments missing is `isError:true` naming the argument; `definition` on a host that returns `null` answers `not available for this file type` with `isError:true`, never an empty success; `diagnostics` defaults `severity` to `all`; `review_diff` returns exactly `{"decision":"accept"}`, `{"decision":"reject"}`, `{"decision":"accept_all"}` or `{"decision":"cancelled"}`; after `accept_all` on a connection the next `review_diff` on **that** connection returns `accept` without asking the host, and a different connection still asks; `ConnectionClosed` drops that state; `review_cancel` returns `{"cancelled":true}` or `{"cancelled":false}`; `debug_start` with the VS Code-only `program`/`type` shape is `isError:true` with text saying Visual Studio debugs the startup project; text formats for `context`, `diagnostics`, `debug_stack` and `debug_variables` match the VS Code extension's (read `vscode/src/lib/format.ts` and `vscode/test/format.test.ts` and port the same cases).
- [ ] **Step 2: Run them.** Expected: FAIL.
- [ ] **Step 3: Implement.** Paths in arguments are resolved against the host's workspace folders and must stay inside one of them, as `vscode/src/lib/paths.ts` does; port its tests too.
- [ ] **Step 4: Run** `dotnet test`. Expected: PASS.
- [ ] **Step 5: Commit** — `visualstudio: eighteen tools from the shared manifest, written once against IEditorHost`.

---

### Task 4: The fake host and the Go contract test

**Files:**
- Create: `visualstudio/src/BECode.Bridge.FakeHost/{BECode.Bridge.FakeHost.csproj,Program.cs,ScriptedEditorHost.cs}`, `internal/ide/contract_test.go`

**Interfaces:**
- Consumes: everything from Tasks 2–3, and the harness's real client (`ide.LockDir`, `ide.Discover`, `ide.Connect`, `Client.CallTool`).
- Produces: `dotnet run --project visualstudio/src/BECode.Bridge.FakeHost -- --home <dir> --workspace <dir>`: starts the bridge over a `ScriptedEditorHost` with deterministic canned answers, writes the lock file under `<dir>/.be-code/ide/`, prints `READY <port>` on stdout, and exits cleanly (removing the lock) on stdin EOF or SIGTERM.

- [ ] **Step 1: Write the failing Go test.** `TestVisualStudioBridgeSpeaksTheHarnesssProtocol`: `t.Skip("dotnet not on PATH; the Visual Studio bridge contract test needs the .NET SDK")` when `exec.LookPath("dotnet")` fails. Otherwise: build the fake host once, start it with a temp home and a temp workspace, wait for `READY`, then with `HOME`/`USERPROFILE` pointed at the temp home: `Discover` must find the lock with `ideName == "visualstudio"`; `Connect` must succeed; the advertised tool list must equal the non-hidden entries of `vscode/tools.manifest.json` (names and schemas); every one of the eighteen tools is called once with valid arguments and returns the scripted answer with `isError == false`; `review_diff` returns a decision the harness's own `ReviewDiff` parser accepts; a connection with a wrong token is refused; after the process is stopped the lock file is gone.
- [ ] **Step 2: Run it** — `go test ./internal/ide/ -run TestVisualStudioBridgeSpeaksTheHarnesssProtocol -v`. Expected: FAIL (no fake host).
- [ ] **Step 3: Implement the fake host.**
- [ ] **Step 4: Run** the contract test, then `make -f build.mk verify`. Expected: PASS; and with `PATH` stripped of `dotnet` the test reports SKIP, not FAIL.
- [ ] **Step 5: Commit** — `ide: a contract test — the real Go client against the Visual Studio bridge, every tool`.

---

### Task 5: The harness attaches to Visual Studio without `--ide`

**Files:**
- Modify: `cmd/root.go` (`attachIDE`), `cmd/root_test.go` or the existing attach tests; `README.md` (the `ide` config reference).

- [ ] **Step 1: Write the failing tests.** With `ide.enabled` on and `TERM_PROGRAM` unset: a live lock with `ideName:"visualstudio"` covering the workspace attaches; a live lock with `ideName:"vscode"` does **not** (unchanged behaviour: VS Code still needs its own terminal or `--ide`); a Visual Studio lock for a different workspace does not; `--no-ide` wins over everything; `--ide` still attaches to either.
- [ ] **Step 2: Run.** Expected: FAIL on the first case.
- [ ] **Step 3: Implement** the rule from spec §6: auto-attach when `ide.enabled` and (`TERM_PROGRAM == "vscode"` **or** the discovered lock's `IDEName == "visualstudio"`). Discovery happens before the gate, so do not print a not-found warning on the quiet path.
- [ ] **Step 4: Run** `go test ./cmd/ ./internal/ide/ ./internal/review/` and `make -f build.mk verify`. Expected: PASS.
- [ ] **Step 5: Commit** — `cmd: attach to a running Visual Studio without --ide`.

---

### Task 6: The Visual Studio layer (written here, built on Windows)

**Files:**
- Create: `visualstudio/src/BECode.VisualStudio/{BECode.VisualStudio.csproj,BECodePackage.cs,VisualStudioEditorHost.cs,VisualStudioDebugHost.cs,ReviewInfoBar.cs,source.extension.vsixmanifest}`, `visualstudio/build.ps1`, `visualstudio/BECode.VisualStudio.Windows.slnf`

**This task cannot be compiled on this machine.** Its acceptance is: the Linux solution still builds and tests green with this project excluded; the code is complete, not stubbed; every Visual Studio API it calls is named in the report with the reason it was chosen; and `WINDOWS-CHECKLIST.md` (Task 7) covers every path in it.

- [ ] **Step 1:** `source.extension.vsixmanifest`: `InstallationTarget` `Microsoft.VisualStudio.Community` (and Pro, Enterprise) with `Version="[17.0,19.0)"`, `ProductArchitecture` `amd64` and `arm64`; prerequisite `Microsoft.VisualStudio.Component.CoreEditor` `[17.0,19.0)`.
- [ ] **Step 2:** `BECodePackage : AsyncPackage`, auto-loaded on `UIContextGuids80.SolutionExists` and `NoSolution` with `PackageAutoLoadFlags.BackgroundLoad`. On load: build the host and `ToolRegistry`, start `BridgeServer`, write the lock with `ideName:"visualstudio"`. Republish `workspaceFolders` (solution directory plus each loaded project's directory) on solution open, close, and project add or remove. On dispose: stop the server and remove the lock. Every failure is logged to the Activity Log and never thrown into Visual Studio.
- [ ] **Step 3:** `VisualStudioEditorHost`: context and open through `DTE2` and `IVsTextManager`; diagnostics from the Error List (`IErrorList` table entries: path, line, column, severity, code, text); definition, references and hover through Roslyn's workspace (`VisualStudioWorkspace`, `SymbolFinder`, `QuickInfo`) for C# and VB, returning `null` for other file types. Every Visual Studio call switches to the main thread with `JoinableTaskFactory.SwitchToMainThreadAsync`.
- [ ] **Step 4:** `VisualStudioDebugHost` over `EnvDTE.Debugger`/`Debugger2` and `IVsDebugger` events: configurations are the startup projects and their launch profiles; `continue` and `step` wait for the next break with a timeout, the way `vscode/src/lib/stopwaiter.ts` does; output is captured from the Debug pane of the Output window with a cursor.
- [ ] **Step 5:** `ReviewInfoBar` + `IVsDifferenceService.OpenComparisonWindow2` between the file on disk and a temp file holding the proposed content, with an info bar offering Accept, Accept all, Reject; closing the window is `cancelled`; `ReviewCancelAsync` closes the frame.
- [ ] **Step 6:** `build.ps1`: locate MSBuild with `vswhere`, restore, build Release, print the `.vsix` path; fail with a readable message when the "Visual Studio extension development" workload is missing.
- [ ] **Step 7: Verify what can be verified here** — `cd visualstudio && dotnet build BECode.VisualStudio.sln && dotnet test`. Expected: PASS, the VSIX project not in that solution.
- [ ] **Step 8: Commit** — `visualstudio: the VSIX layer for 2022 and 2026 — written against the SDK, unbuilt until Windows`.

---

### Task 7: Documentation and the Windows checklist

**Files:**
- Create: `visualstudio/README.md`, `visualstudio/WINDOWS-CHECKLIST.md`
- Modify: `README.md`, `CHANGELOG.md`, `build.mk` (a `visualstudio-test` target running `dotnet test`, not part of `verify`), `/home/sbrown/Documents/Development/BE-CodeRedux/CLAUDE.md` (edited in place; it is outside this repository)

- [ ] **Step 1:** `visualstudio/README.md`: what it is, requirements (Visual Studio 2022 17.0+ or 2026, the extension-development workload to build), `.\build.ps1`, installing the `.vsix` into each version, that BE-Code attaches on its own once Visual Studio has the project open, the tool list, and a **Status** section stating plainly that the Visual Studio layer has not yet been built or run, and which parts have been proven on Linux.
- [ ] **Step 2:** `WINDOWS-CHECKLIST.md`, numbered and tickable: build; install into 2022; install into 2026; lock file appears in `%USERPROFILE%\.be-code\ide\` and disappears on close; `be-code` in a separate terminal attaches without `--ide`; `/tools` lists the sixteen advertised `ide_*` tools; one check per tool family with the exact prompt to type and what to expect; a proposed write opens the diff and each of Accept, Accept all, Reject and closing the window does the right thing; two Visual Studio instances on two solutions each get their own lock; what to send back if a step fails (Activity Log path, the lock file, `be-code --ide` output).
- [ ] **Step 3:** main `README.md` editor section and `CHANGELOG.md` entry (`v0.12.0`), `build.mk` `VERSION := 0.12.0`; the parent `CLAUDE.md` gains a short "Visual Studio" paragraph under the editor bridge section.
- [ ] **Step 4:** `make -f build.mk verify`, `cd visualstudio && dotnet test`, `cd vscode && npm test`. Expected: all PASS.
- [ ] **Step 5: Commit** — `docs: the Visual Studio extension, its Windows checklist, 0.12.0`.
