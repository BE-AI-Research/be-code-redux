# Visual Studio Host — Design for the layer that cannot be run here

Companion to `2026-09-20-visual-studio-extension-design.md`. Written before Task 6 is implemented, because Task 6 is the part of this work that tests cannot rescue: it is C# against the Visual Studio SDK, it cannot be *run* anywhere but Windows, and the first time it runs is on the owner's machine. The implementer transcribes this document; it does not explore.

**It can, however, be compiled here** (found by a spike after this document was first written, and it changes the risk a great deal): the Visual Studio SDK ships on NuGet as plain reference assemblies, so an SDK-style `net472` project referencing `Microsoft.VisualStudio.SDK`, `Microsoft.VisualStudio.LanguageServices` and `Microsoft.NETFramework.ReferenceAssemblies` builds with `dotnet build` on Linux. A wrong property name is a compile error on this machine, and the SDK's own analyzers — unchecked `GetService` results, UI-thread misuse (the `VSTHRD` rules) — run here too. Only two things need Windows: packaging the `.vsix` (the `Microsoft.VSSDK.BuildTools` targets, referenced under an `'$(OS)' == 'Windows_NT'` condition) and *behaviour*. So "unbuilt" is wrong and "blind" is half wrong: this layer is type-checked and analyzer-checked, and unproven only in what it does at run time.

Everything below the seam is already built and tested on Linux (`BECode.Bridge`: 206 tests, plus the Go contract test). This document covers only `src/BECode.VisualStudio/`: what implements `IEditorHost`/`IDebugHost`, which Visual Studio API backs each member, on which thread, and what happens when it fails.

**Confidence is stated per item.** *Solid* means a long-stable public API used the documented way. *Check* means the API is right but a detail (a property name, an indexing base) must be confirmed on Windows and is listed in `WINDOWS-CHECKLIST.md`. *Risk* means it may not work as designed and a fallback is named.

## 1. The three risks, first

### 1.1 `System.Text.Json` inside `devenv.exe` — **Risk, and the most likely first failure**

`BECode.Bridge` referenced `System.Text.Json` 8.0.5 when this was written (plus `System.Memory`, `System.Threading.Channels`, `Microsoft.Bcl.AsyncInterfaces`). Visual Studio is a .NET Framework 4.7.2 process that already loads its own copies of these, with its own binding redirects in `devenv.exe.config`. An extension cannot edit that file. If the version the VSIX carries is *newer* than the one Visual Studio redirects to, the load fails with `FileLoadException` at package load; if it is older or equal, Visual Studio's copy is used and all is well.

Visual Studio 2022 17.0 shipped a 5.x/6.x `System.Text.Json`; only later 17.x servicing releases moved to 8.x. So 8.0.5 is too new for the floor the manifest declares.

Decision: **`BECode.Bridge` drops to `System.Text.Json` 6.0.x** (with the matching 6.0.0 `Microsoft.Bcl.AsyncInterfaces` and `System.Threading.Channels`), the VSIX does **not** ship those assemblies (`ExcludeAssets="runtime"` / `Private=false`), and the manifest floor rises from 17.0 to **17.6** (agreed with the owner) — the first long-term-servicing 2022 release, and comfortably inside what ships 6.x or later. Nothing in the bridge uses an API newer than 6.0 (it uses `JsonDocument`, `JsonElement`, `JsonSerializer`, `JavaScriptEncoder`, `JsonPropertyName`); and that is now proven: it compiles against 6.0.11 with no warnings, with all 206 tests and the Go contract test green (the compile is the proof — the net8.0 test host runs the in-box copy). If the owner's checklist still shows a `FileLoadException`, the fallback is `[ProvideCodeBase]` on the package for the carried assemblies — named here so it is a known move, not a discovery.

### 1.2 Roslyn versions across 2022 and 2026 — **Check**

`definition`/`references`/`hover` use Roslyn. The VSIX compiles against the **lowest** supported Roslyn (`Microsoft.VisualStudio.LanguageServices` 4.6.0, matching 17.6) with `ExcludeAssets="runtime"`, and never ships Roslyn. Only public, long-stable surface is used (§3.3), so a newer Roslyn in 2026 binds forward without change.

### 1.3 Code that has never run — **Risk, accepted and bounded**

Nothing here has ever executed. Two bounds on that. First, it compiles on this machine against the real SDK with warnings as errors and the SDK analyzers on, so names, signatures, nullability and the most common threading mistakes are settled before the owner ever sees it; what remains unknown is behaviour (an indexing base, which events fire, whether a frame has an info bar host). Second, every member either returns data or throws; every throw is caught at one place (`Guard`, §2.3) and becomes an `isError` tool result with the exception's message, logged to the Activity Log. A wrong assumption therefore costs one tool its answer and tells the owner exactly which one. **No failure in this layer may take down Visual Studio, hang its UI thread, or fail package load** — those are the three outcomes the design is organised around avoiding.

### 1.4 What the owner asked for

The goal is the harness having as full access to the IDE as possible, on Visual Studio 2022 (17.6 and later) and 2026. Where this design stops short of that (§5) it is because a stable public API does not exist or could not be trusted unrun, never for convenience; each limit is stated in the README, and each is a candidate for a later version once the base is proven on Windows.

## 2. Shape

```
BECodePackage : AsyncPackage          load, lifetime, lock file, solution events
  └─ VisualStudioEditorHost : IEditorHost
       ├─ VisualStudioDebugHost : IDebugHost
       ├─ RoslynNavigator             definition / references / hover
       ├─ ErrorListReader             diagnostics
       └─ DiffReview                  review_diff: difference viewer + info bar
  Threading.cs                        OnMainThreadAsync helpers, Guard
  ActivityLog.cs                      one logging seam
```

### 2.1 Package

`[PackageRegistration(UseManagedResourcesOnly = true, AllowsBackgroundLoading = true)]`, `[ProvideAutoLoad(UIContextGuids80.NoSolution, PackageAutoLoadFlags.BackgroundLoad)]` and the same for `SolutionExists`. *Solid.*

`InitializeAsync` (background thread): build the host → `new ToolRegistry(host)` → `new BridgeServer(registry, token, version)` with `OnError` wired to the Activity Log → `StartAsync()` → write the lock. The whole body is in one `try`; a failure is logged and swallowed, leaving Visual Studio with an extension that does nothing rather than one that breaks load. `Dispose(bool)`: remove the lock **first** (so the harness stops finding a dying bridge), then dispose the server with a 2 s bound, never blocking the UI thread on it (`JoinableTaskFactory.RunAsync` + bounded wait).

Home directory: `Environment.GetFolderPath(SpecialFolder.UserProfile)` — the same directory Go's `os.UserHomeDir()` resolves on Windows (`%USERPROFILE%`). *Check.*

### 2.2 Workspace folders and the lock

Folders = the solution directory, plus the directory of every loaded project, de-duplicated, absolute; in Open Folder mode, the opened folder alone. Read through `IVsSolution` (`GetSolutionInfo` for the directory; `GetProjectEnum(EPF_LOADEDINSOLUTION)` then `IVsHierarchy` → `VSHPROPID_ProjectDir`), not `DTE.Solution.Projects`, which needs a recursive walk through solution folders and throws on unloaded projects. *Solid.*

Republish on `IVsSolutionEvents`: `OnAfterOpenSolution`, `OnAfterCloseSolution`, `OnAfterOpenProject`, `OnBeforeCloseProject`, and `OnAfterOpenFolder`/`OnAfterCloseFolder` (`IVsSolutionEvents7`). Events arrive in bursts while a solution loads, so republishing is debounced (250 ms) and runs off the UI thread. The folder list is cached in a field the host reads for `GetWorkspaceFoldersAsync` — that member is called on every path-taking tool and must not touch the UI thread. With no solution and no folder open the list is empty, and the tools already answer `no solution or folder is open`.

### 2.3 Threading and the guard

`IEditorHost` is called concurrently from thread-pool threads. Two rules:

1. **Every Visual Studio call happens after `await JoinableTaskFactory.SwitchToMainThreadAsync(ct)`**, and the method leaves the main thread again (`await TaskScheduler.Default`) before any wait. Nothing in this layer ever calls `.Result`, `.Wait()`, `JoinableTaskFactory.Run` or `Thread.Sleep`. The UI thread is only ever borrowed for short, synchronous reads and writes.
2. **Waits happen off the main thread on a `TaskCompletionSource`** completed by an event handler (debugger break, info-bar click, frame close), raced against `ct` and a timeout.

`Guard.RunAsync(name, ct, body)` wraps every member: switches threads, runs the body, lets `OperationCanceledException` through only when `ct` is cancelled, logs anything else and rethrows it as `InvalidOperationException("<member>: <message>")`, which the tools layer reports as `isError`. COM collections are 1-based and are iterated with `foreach` or `Item(i)` from 1 — called out because it is the classic blind-code mistake.

## 3. `IEditorHost`, member by member

### 3.1 `GetContextAsync` — *Solid*

`DTE2.ActiveDocument`: null, or `FullName` not a rooted existing file → `File = ""`, `Line = 0`, empty selection. Otherwise `(TextSelection)ActiveDocument.Selection`: `ActivePoint.Line` (already 1-based) → `Line`; `IsEmpty` → `SelStart = SelEnd = 0`, `Selection = ""`; else `TopPoint.Line`, `BottomPoint.Line`, `Text` (raw — the tool truncates). `Open` = `DTE.Documents` whose `FullName` is a rooted existing file. A non-text document (a designer) has a null `Selection` cast: treat as no selection.

### 3.2 `OpenAsync` — *Solid*

`File.Exists` false → `FileNotFoundException` (the tool turns it into `file not found`). `VsShellUtilities.OpenDocument(serviceProvider, path, VSConstants.LOGVIEWID_Code, out _, out _, out IVsWindowFrame frame, out IVsTextView view)`; when `line` is given, `view.SetCaretPos(line - 1, 0)` then `view.CenterLines(line - 1, 1)` — `IVsTextView` is **0-based**. Focus: `frame.ShowNoActivate()` rather than `Show()`, which is Visual Studio's equivalent of `preserveFocus`. *Check:* a terminal outside Visual Studio never had VS focus to lose, so this only matters for the integrated terminal.

### 3.3 `DefinitionAsync` / `ReferencesAsync` / `HoverAsync` — *Check*

`VisualStudioWorkspace` from MEF (`IComponentModel.GetService<VisualStudioWorkspace>()`). `workspace.CurrentSolution.GetDocumentIdsWithFilePath(path)` empty → return **null** (not a Roslyn document: C++, JSON, anything else → the tool says `not available for this file type`). Otherwise, all off the UI thread — Roslyn is free-threaded:

- position = `text.Lines[line - 1].Start + (col - 1)`, clamped to the line's length;
- `SymbolFinder.FindSymbolAtPositionAsync(semanticModel, position, workspace, ct)`; null symbol → empty list / `""`;
- **definition:** `symbol.Locations` where `IsInSource` → `GetLineSpan()` → `Location(path, StartLinePosition.Line + 1, .Character + 1)`. A metadata-only symbol (a framework type) → empty list: there is no file to point at;
- **references:** `SymbolFinder.FindReferencesAsync(symbol, solution, ct)` → every `ReferenceLocation` → path, 1-based line and column, `Text` = that source line trimmed. Unbounded by design: the tool counts and truncates;
- **hover:** `symbol.ToDisplayString(SymbolDisplayFormat.MinimallyQualifiedFormat)`, then a blank line, then the `<summary>` text of `symbol.GetDocumentationCommentXml()` when there is one. This is deliberately **not** Visual Studio's Quick Info: that service needs a live text view under the mouse and returns UI objects, and this answer is what the model wants anyway.

Unsaved edits: Roslyn reads the open buffer, so answers reflect what is on screen, not what is on disk. Same as VS Code.

### 3.4 `DiagnosticsAsync` — *Check*

`((DTE2)dte).ToolWindows.ErrorList.ErrorItems`, 1-based COM collection, each `ErrorItem`: `FileName`, `Line`, `Column`, `Description`, `ErrorLevel` (`vsBuildErrorLevelHigh` → `error`, `Medium` → `warning`, `Low` → `info`), `Project`. Chosen over the newer `IErrorList`/table API because it is five properties on an interface unchanged since 2005; the table API is richer and much easier to get wrong blind. `Source` = the leading error code parsed from `Description` when it looks like `CS1002:`/`C2065`, else the project name, else `-`.

Two caveats, both for the README rather than the code: the collection reflects the Error List's **current filter** (a user who set it to "Current Document" gets that), and reading it forces the tool window to be created once. *Check:* whether `Line`/`Column` are 1-based here. Every source says yes; it is on the checklist because diagnostics off by one line is the most visible failure this extension could have.

### 3.5 `ReviewDiffAsync` — *Check, the most intricate member*

Left side: `request.Original` when supplied, written to a temp file; else the file on disk; else (new file) an empty temp file. Right side: `request.Proposed` in a temp file named `<name>.proposed<ext>` so the language service colours it. `IVsDifferenceService.OpenComparisonWindow2(left, right, caption: "BE-Code: <relative path>", tooltip, leftLabel: "Current", rightLabel: "Proposed", inlineLabel, roles: null, options: VSDIFFOPT_RightFileIsTemporary | (left is temp ? VSDIFFOPT_LeftFileIsTemporary : 0))` → `IVsWindowFrame`.

Info bar on that frame: `frame.GetProperty((int)__VSFPROPID7.VSFPROPID_InfoBarHost)` → `IVsInfoBarHost`; model = `InfoBarModel(text: request.Summary ?? "Apply this change?", actionItems: [Accept, Accept all this session, Reject], image: KnownMonikers.StatusInformation)`; `IVsInfoBarUIFactory.CreateInfoBar(model)` → `Advise` an `IVsInfoBarUIEvents`: `OnActionItemClicked` → complete the `TaskCompletionSource` with the matching decision; `OnClosed` → nothing (the user dismissed the bar, the diff is still open). When `request.Shared`, the text gains "— also waiting in the terminal". The diff is **never modal**: a modal dialog would block a `review_cancel` from closing it.

Closing the diff window without answering = `Cancelled`: `IVsWindowFrame2.Advise(IVsWindowFrameNotify)` → `OnShow(FRAMESHOW_WinClosed)` completes the source with `Cancelled`.

Cancellation (`ct`, from `review_cancel` or the connection dropping): `ct.Register` → on the main thread `frame.CloseFrame(FRAMECLOSE_NoSave)` → the source completes `Cancelled`. `TrySetResult` throughout, so a click racing a cancel is decided once. In a `finally`: unadvise both cookies, close the frame if still open, delete the temp files (best effort — a file the diff viewer still holds is left for the OS temp cleaner).

**The editor decides; it never writes the file.** On accept the harness writes it, exactly as with VS Code.

Fallback if the info bar host is unavailable on the comparison frame (*Risk*: I believe document frames have one; I have not seen it done on this particular frame): the main window's info bar host, `IVsShell` → `VSSPROPID_MainWindowInfoBarHost`, with the file name in the text. The decision logic is identical; only where the bar appears changes.

## 4. `IDebugHost`, member by member

Backed by `EnvDTE.Debugger` (`dte.Debugger`, cast to `Debugger5` where named) and `DebuggerEvents`. **`dte.Events.DebuggerEvents` is stored in a field for the package's lifetime** — a local is collected and the events silently stop; this is the best-known DTE trap. *Solid, once the field is there.*

One `DebugSession` object holds the state, guarded by one lock: the pending-stop `TaskCompletionSource<StopResult>`, and the output cursor. Handlers: `OnEnterBreakMode(reason, ref action)` → `Stopped` with `reason` mapped (`dbgEventReasonBreakpoint` → `breakpoint`, `Step` → `step`, `ExceptionThrown`/`ExceptionNotHandled` → `exception`, `UserBreak` → `pause`, else `stopped`); `OnEnterDesignMode(reason)` → `Exited` when `reason == dbgEventReasonEndProgram`, else `Terminated`. `ExitCode` is left null: DTE does not report it.

- **`ConfigsAsync`** — *Check.* One `DebugConfigInfo(name, "startup project")` per loaded project that can be started (`IVsHierarchy` with an `IVsDebuggableProjectCfg`, or, simpler and what v1 does: every project whose `Project.Properties` has an `OutputType` of exe/winexe or is a web project), with the current startup project listed first. **Launch profiles are listed but not selectable in v1**: `launchSettings.json` profiles appear as `"<project> › <profile>" ("launch profile")` so the model can see them, but `StartAsync` with such a name answers a clear error saying to select that profile in Visual Studio's toolbar. Switching the active profile programmatically has no stable public API, and guessing at one blind is how an extension ends up corrupting a `.user` file. Recorded as a known limitation, not hidden.
- **`StartAsync(config)`** — *Check.* Already debugging → `Stop(false)` and wait for design mode (bounded 10 s). `config` given → `dte.Solution.SolutionBuild.StartupProjects = <unique project name>`. Arm the stop source, then `dte.ExecuteCommand("Debug.Start")`. `ExecuteCommand` returns at once; a build failure never enters run mode, so alongside the 60 s stop wait there is a 5 s "did we leave design mode" check: if `Debugger.CurrentMode` is still `dbgDesignMode` and the last build failed (`SolutionBuild.LastBuildInfo > 0`), answer `Terminated` with reason `build failed (<n> projects)`.
- **`ContinueAsync` / `StepAsync`** — *Solid.* Not in break mode → `InvalidOperationException("not stopped at a breakpoint")`. Arm, then `Debugger.Go(false)` / `StepOver(false)` / `StepInto(false)` / `StepOut(false)` — the `false` is `WaitForBreakOrEnd`, and it must be false or the UI thread blocks for the length of the run. Await the source off the main thread, 60 s, `Timeout` otherwise.
- **`SetBreakpointAsync`** — *Check.* Add: `Debugger.Breakpoints.Add(File: path, Line: line, Column: 1, Condition: condition, ConditionType: dbgBreakpointConditionTypeWhenTrue)`. Remove: every `Breakpoint` whose `File` equals `path` (case-insensitive) and `FileLine == line` → `Delete()`. Returns the breakpoints remaining in that file. Works in design mode: breakpoints set before `debug_start` bind when the module loads.
- **`StackAsync(depth)`** — *Check.* `Debugger.CurrentThread.StackFrames`, first `depth`; each cast to `EnvDTE90a.StackFrame2` for `FileName` and `LineNumber` (the base `StackFrame` has neither — this cast is the whole reason the member is marked Check), `FunctionName` → `Name`, index → `FrameId` (1-based, as the collection is). A frame with no source → `Path = null`.
- **`VariablesAsync(frame)`** — *Solid.* Frame by id (or `CurrentStackFrame`): `Locals` → scope `Locals`, `Arguments` → scope `Arguments`. Each `Expression`: `Name`, `Value`, `Type`. Children (`Expression.DataMembers`) resolved **one level, and only when that scope has ten or fewer variables**, first twenty — the bound the interface documents. Reading `DataMembers` evaluates properties in the debuggee, which can be slow or have side effects; that is why the bound is enforced here and not left to the tool.
- **`EvaluateAsync`** — *Solid.* `Debugger.GetExpression(expression, UseAutoExpandRules: false, Timeout: 5000)`; `IsValidValue` false → `InvalidOperationException(expr.Value)` (Visual Studio puts the error text in `Value`). For a non-current frame, set `Debugger.CurrentStackFrame` first; this moves the user's Call Stack selection, which is what evaluating in a frame means in Visual Studio.
- **`OutputAsync(since)`** — *Check.* The Output window's **Debug** pane: `((DTE2)dte).ToolWindows.OutputWindow.OutputWindowPanes.Item("Debug")` → `TextDocument` → `StartPoint.CreateEditPoint().GetText(EndPoint)`, split into lines; return lines from `since`, new cursor = line count. If the pane was cleared (count < `since`), start again from 0. Reading the whole pane each call is O(pane); acceptable for a tool called a few times per run. *Check:* the pane's name is localised on a non-English Visual Studio; look it up by GUID (`VSConstants.OutputWindowPaneGuid.DebugPane_guid`) through `IVsOutputWindow` if `Item("Debug")` throws.
- **`StopAsync`** — *Solid.* Design mode already → return. `Debugger.Stop(false)`.

## 5. What is deliberately not in v1

- Definition, references and hover for C++ and anything else Roslyn does not own: answered `not available for this file type`.
- Selecting a launch profile programmatically (§4 `ConfigsAsync`).
- The debuggee's exit code.
- Attach-to-process, multiple simultaneous debug sessions, per-thread stacks.
- A tool window or any UI of the extension's own. It is a bridge; its only visible surface is the diff and its info bar.

## 6. How this gets verified

Nothing in this layer is claimed to work until the owner has run `WINDOWS-CHECKLIST.md`, which is generated from this document: one entry per *Check* and *Risk* above, each with what to do, what should happen, and where the evidence appears (the tool's answer, or **Help › View Activity Log**). The checklist's first entry is package load, because §1.1 predicts that is where a failure shows first.
