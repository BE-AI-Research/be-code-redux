# BE-Code for Visual Studio

Gives BE-Code the editor's own view of your project in Visual Studio 2022 and 2026: the active file and selection, the Error List, go-to-definition, references and hover for C# and VB, the debugger, and a side-by-side diff of every change BE-Code proposes, with Accept, Accept all and Reject. It is the Visual Studio counterpart of the VS Code extension in `../vscode/`, and speaks the same protocol to the same harness.

It has no window of its own. Install it, open a solution or a folder, and run `be-code` in any terminal whose working directory is inside that project.

## Status — read this first

**The Visual Studio layer has never run.** Everything in it is compiled on Linux against the real Visual Studio SDK with warnings as errors and the SDK's analyzers on, so names, signatures and the common threading mistakes are settled. What it *does* inside Visual Studio is unproven until `WINDOWS-CHECKLIST.md` has been worked through on a real install.

| Part | State |
|---|---|
| Wire protocol, lock file, the eighteen tools, path confinement, review and accept-all logic (`src/BECode.Bridge`) | Built and tested on Linux: 275 tests |
| The real BE-Code client against the real bridge, every tool (`internal/ide/contract_test.go`) | Tested on Linux through a scripted host |
| The Visual Studio host (`src/BECode.VisualStudio`) | **Compiles; never run** |
| Packaging the `.vsix` (`build.ps1`) | **Never executed** |

## Requirements

- Visual Studio 2022 **17.6 or later**, or Visual Studio 2026. Community, Professional or Enterprise; x64 or Arm64.
- To build: the **Visual Studio extension development** workload.
- BE-Code 0.12.0 or later.

## Build and install

```powershell
cd visualstudio
.\build.ps1
```

It finds MSBuild with `vswhere`, restores, builds Release, and puts `be-code-visualstudio-<version>.vsix` in the repository's `dist\` folder, beside the VS Code extension's `be-code-vscode-<version>.vsix`. Both are `.vsix` files; the name is how to tell them apart, and each keeps its own version number — this extension's is independent of BE-Code's and of the VS Code extension's. Double-click the file, or for a particular instance:

```powershell
& "C:\Program Files\Microsoft Visual Studio\2022\Community\Common7\IDE\VSIXInstaller.exe" ..\dist\be-code-visualstudio-0.1.0.vsix
```

One `.vsix` serves both 2022 and 2026; install it into each. Restart Visual Studio afterwards.

## Using it

Open a solution or a folder. The extension loads in the background, starts a server on `127.0.0.1` and advertises it in `%USERPROFILE%\.be-code\ide\<pid>.json`. Then, in any terminal under that solution:

```
be-code
```

BE-Code finds the lock, connects, and its tool list gains sixteen `ide_*` tools. No `--ide` flag is needed: a running Visual Studio whose solution covers the working directory is attached to automatically (`ide.enabled`, on by default; `--no-ide` to stay out). Two Visual Studio instances on two solutions each advertise themselves, and each `be-code` attaches to the one that covers its directory.

Proposed file changes open in Visual Studio's difference viewer with an information bar: **Accept**, **Accept all this session**, **Reject**. Closing the tab without answering cancels that review. Visual Studio only decides; BE-Code writes the file. Where a change is reviewed is `ide.review` / `/review`, as with VS Code — from a separate terminal it resolves to `both`, so the terminal prompt and the diff are both live and the first answer wins.

## Tools

`context`, `open`, `definition`, `references`, `hover`, `diagnostics`, `debug_configs`, `debug_start`, `debug_breakpoint`, `debug_continue`, `debug_step`, `debug_stack`, `debug_variables`, `debug_evaluate`, `debug_output`, `debug_stop` — and two the harness drives itself and never shows the model, `review_diff` and `review_cancel`. Names and argument schemas come from one file shared with the VS Code extension, `../vscode/tools.manifest.json`.

## What it does not do yet

These are limits of this version, not bugs:

- **Definition, references and hover work for C# and VB only.** They go through Roslyn. For C++ and everything else the answer is `not available for this file type`.
- **Hover is the symbol's signature and its XML documentation summary**, not Visual Studio's Quick Info tooltip.
- **Launch profiles are listed, not selectable.** `debug_configs` shows a project's `launchSettings.json` profiles, but `debug_start` takes a startup project; pick the profile in Visual Studio's toolbar. Visual Studio offers no stable public way to switch it from an extension.
- **`debug_start` debugs the startup project.** The VS Code extension's ad-hoc `program` + `type` + `args` form is refused with a message saying so.
- **Diagnostics are what the Error List currently shows**, filter included. Set it to *Entire Solution* and *Build + IntelliSense* for the full picture.
- The debuggee's exit code is not reported; there is no attach-to-process.

## Privacy

The server listens on loopback only and requires the random token in the lock file. That file lives under your user profile and gets the profile's permissions — other ordinary users of the machine cannot read it; an administrator can. (On Linux and macOS, where the bridge library is also used, it is created `0600`.) Nothing leaves the machine; there is no telemetry.

## Layout

```
src/BECode.Bridge/           netstandard2.0 — the protocol and the tools; no Visual Studio reference
src/BECode.Bridge.FakeHost/  console host with scripted answers, for the Go contract test
src/BECode.VisualStudio/     net472 — the package and the Visual Studio implementation of IEditorHost
test/BECode.Bridge.Tests/    xunit
build.ps1                    Windows: produces the .vsix
WINDOWS-CHECKLIST.md         what to verify on a real install, and what to send back if a step fails
THIRD-PARTY-NOTICES.md       the .NET libraries shipped inside the .vsix
```

On Linux or macOS, `dotnet build` and `dotnet test` in this directory compile every project, the Visual Studio layer included, and run the tests. Only packaging needs Windows. From the repository root that is `make -f build.mk visualstudio-test`. It is deliberately **not** part of `make verify` or `release`, which must work without the .NET SDK — so after changing a tool in the VS Code extension and regenerating `vscode/tools.manifest.json`, run it: the C# side's parity tests are what notice a tool this side has no handler for. The design is in `../docs/superpowers/specs/2026-09-20-visual-studio-extension-design.md` and `…-visual-studio-host-design.md`.

`System.Text.Json` is held at 6.0.x in `BECode.Bridge` on purpose. Inside Visual Studio the library runs in `devenv.exe`, which loads its own copies of these assemblies under binding redirects an extension cannot change; a reference *newer* than the oldest supported Visual Studio carries fails the package load. Because 6.0.x is older than anything 17.6+ redirects to, the `.vsix` can safely ship its own copies too: where Visual Studio has one, Visual Studio's wins; where it has none, ours is found (`[ProvideBindingPath]`). Raising the version means raising the minimum Visual Studio with it.

If something does not work, the extension's own log is `%USERPROFILE%\.be-code\visualstudio.log`.

## Licence

MIT — see `../LICENSE`. The .NET libraries carried inside the `.vsix` are Microsoft's, also MIT: `THIRD-PARTY-NOTICES.md`.
