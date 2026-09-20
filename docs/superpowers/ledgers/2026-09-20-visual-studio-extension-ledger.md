# Visual Studio extension — what was decided along the way

Plan: `docs/superpowers/plans/2026-09-20-visual-studio-extension.md`. Specs: `…-visual-studio-extension-design.md` and `…-visual-studio-host-design.md`. Branch `feature/visual-studio-extension`, from `main` at 0.11.1, to 0.12.0.

This is the record of the rulings made while the plan ran — decisions taken without stopping to ask, each with why and what it costs if wrong — so the owner can see them in one place and overturn any of them.

## What went wrong, and what changed because of it

The spec and plan were written in a hurry and thin. One line of the wire contract — "requests on one connection are handled in order" — was copied from what `vscode/src/server.ts` did, without checking it against the Go client, which multiplexes calls on one connection and sends `review_cancel` on the same connection as the `review_diff` it withdraws. The server was built to that line. Every review round after it found another consequence: the deadlock, then teardown bugs in the concurrency added to fix it, then a defect in the cap added to bound that. The owner's description — "brute forcing results, not using the correct methods we established" — was accurate; the systematic-debugging rule for exactly this pattern (each fix reveals a new problem elsewhere: stop, question the design) was not applied soon enough.

What changed: the server was frozen and its lifecycle written down as invariants; fix rounds were checked by the controller reading the diff and running the tests instead of by further large reviews; small fixes were written directly; and the Visual Studio layer — the one part tests cannot rescue — got a design first, reviewed by the owner, before anyone implemented it. Writing that design led to a spike showing the layer **compiles on Linux** against the SDK's NuGet reference assemblies, which turned "written blind" into "type-checked and analyzer-checked, unrun".

The same deadlock was live in the shipped VS Code extension (1.1.0) and is fixed here (1.1.1).

## Rulings

| # | Ruling | Why | Cost if wrong |
|---|---|---|---|
| R-1 | Test project and fake host target `net8.0` with `RollForward=Major`. | The machine first had only the .NET 10 runtime; Visual Studio 2022's SDK cannot target `net10.0`. | None seen; 8.0 was installed later and roll-forward is inert there. |
| R-2 | `LineFramer` caps a line at 16 MiB and the connection closes beyond it. | The framer runs before authentication. | A legitimate single message over 16 MiB is refused; raise the constant. |
| R-3 | `BridgeServer.OnError` callback added. | Every swallowed failure was invisible. | One more public member. |
| R-4 | No `-32700` for unparseable frames; no fixed-time token compare. | The Go client ignores replies without an id; a 256-bit token on loopback. | None practical. |
| R-5 | The harness attaches unasked only to a Visual Studio lock that **covers** the workspace, through a new `ide.DiscoverCovering`; `--ide` keeps `Discover`'s newest-lock fallback. | `Discover` falls back to the newest lock of *any* workspace, so a Visual Studio open on another project would have been attached to. | One exported helper. |
| R-6 | `diagnostics` defaults to errors and warnings, not `all` — the plan's own text was wrong. | The shared manifest's description, which the model reads, says so, and VS Code does so. | None. |
| R-7 | **The wire contract's ordering line was a defect.** `initialize`, `tools/list`, `ping` and refusals stay ordered; every `tools/call` runs on its own and replies by id. Spec §3 amended with a dated note. | `review_cancel` could never be processed while `review_diff` was pending. | Two tool calls on one connection can overlap; the agent awaits each call, so in practice only a cancel overlaps. |
| R-8 | The `IEditorHost` seam, settled before the Visual Studio layer was written: no `ReviewCancelAsync` (the token is the one cancel mechanism); `GetWorkspaceFoldersAsync`; absolute paths across the seam, the tools relativise and confine; `DiagnosticsAsync` without a severity and `ReferencesAsync` without a max (the tools filter, count and truncate); selection truncation in the tool. | Everything decidable without Visual Studio belongs in the tested half. | A host returns more data than strictly needed. |
| R-9 | Descriptions of exactly `debug_start` and `debug_configs` are overridden on the Visual Studio side. | The shared wording steers a model toward `program` + `type`, which Visual Studio refuses. | A second place a description lives; a parity test pins the exception list. Host-neutral wording in the manifest is the alternative. |
| R-10 | At most 64 concurrent tool calls per connection, as back-pressure. | R-7 raised one connection's blast radius from one call to unbounded (50 000 live calls by probe). | At the cap the whole ordered path waits, `tools/list` included. |
| R-11 | `ConnectionClosed` is delivered before in-flight calls have unwound; the tools tolerate a call finishing afterwards. | Prompt teardown; the alternative blocks cleanup behind a pending review. | The tools layer carries the burden (the accept-all resurrection race, found and fixed). |
| R-12 | The seam's conventions written into `IEditorHost.cs`: **1-based lines and columns in both directions**, the 60 s wait for the next debugger stop and its `StopKind` mapping, concurrent calls from pool threads, `OperationCanceledException` only for the caller's own token, enums for `action` and `step`, `VariablesAsync` without a scope. | The layer that implements it cannot be run here; an unstated convention is an off-by-one nobody would catch. | None. |
| — | `System.Text.Json` held at 6.0.x in `BECode.Bridge`; Visual Studio floor 17.6 (owner agreed). | Inside `devenv.exe` a reference newer than Visual Studio's own copy fails the package load. | Raising either means raising both. |
| R-13 | The VSIX **ships** the 6.0.x BCL set and carries `[ProvideBindingPath]` — reversing the host design's first answer. | Nothing put `BECode.Bridge.dll` on `devenv.exe`'s probing path, and betting Visual Studio carries every dependency was unverifiable. 6.0.x is older than anything 17.6+ redirects to, so Visual Studio's copy still wins where it has one. | A slightly larger package. |
| R-14 | `debug_configs` lists every loaded project that is not a solution folder; the startability heuristic is gone. | The heuristic read `OutputType` backwards and could not see projects in solution folders. Over-listing is harmless; under-listing hides a project. | `debug_start` on a class library fails with Visual Studio's own message. |
| R-15 | Every Activity Log entry is mirrored to `~/.be-code/visualstudio.log`. | The Activity Log may not be writable from a background thread, and it is the owner's only diagnostic surface. | One more file in the dotdir. |
| R-16 | `Lock.Covers` folds case on Windows. | Found by the whole-branch review: Visual Studio, a PowerShell `cd` and VS Code each report the same directory in a different case. The byte-exact comparison had always been there, hidden by `Discover`'s newest-lock fallback; attaching without `--ide` has no fallback and is silent when it misses. | None on Linux or macOS; on Windows two directories differing only in case are one directory anyway. |

## The whole-branch review

One review at the end, scoped to what a task review cannot see. It found the Windows case mismatch above (the likeliest cause of the checklist's attach step failing, and invisible from Linux); that an attached Visual Studio was announced as "VS Code connected" and a review it answered as "answered in VS Code"; that the system prompt still recommended the debugger's `program` form, which Visual Studio refuses and its own tool description warns against; that the C# server did not answer `ping` though the contract says it does; a manifest regeneration switch that any exported value could trip; stale status and version-floor text; and no notice for the MIT-licensed .NET assemblies the `.vsix` now ships. All fixed. It confirmed an ordinary session end writes nothing alarming to `visualstudio.log`, that a user who installs neither extension sees no change beyond `~/.be-code/ide` being created, and that `make release` needs no .NET SDK.

Decided by the owner: the repository had no LICENSE file, though `vscode/package.json` declared MIT — it now carries the MIT licence, the same text and copyright holder as `vscode/LICENSE`. Also decided by the owner: the Visual Studio extension is its own thing and keeps its own version (`0.1.0`), independent of BE-Code's and of the VS Code extension's — so both extensions are named for their editor instead (`BE-Code for VS Code`, `be-code-vscode-<version>.vsix`; `BE-Code for Visual Studio`, `be-code-visualstudio-<version>.vsix`), and a test keeps that version's three hand-written copies in agreement.

Accepted limits of 0.12.0 (owner agreed): definition, references and hover for C# and VB only; launch profiles listed but not selectable; diagnostics are what the Error List currently shows.

## Open — not blocking

- **The Visual Studio layer has never run.** `visualstudio/WINDOWS-CHECKLIST.md` is the proof still owed; its section A (package load, the shipped assemblies) and B (attach after a solution is opened in a Visual Studio that started with none) are where a failure is likeliest.
- `build.ps1` has never executed.
- Windows path behaviour of `Paths.RelPath` is tested as a pure function with an injected separator and comparison, not on a real Windows filesystem.
- `BridgeServer`: replies written inline during teardown still report `write failed` for a client that has simply left (forked replies are quiet); the file is about 1 100 lines and wants a `Connection` type extracted when next touched.
- The VS Code extension has no test that drives `review_diff` then `review_cancel` through its real server with the `vscode` mock; the mechanism is covered by a cancel-shaped test.
- From 0.11.x, unchanged by this work: the first request after `/model` carries no `num_ctx`; the flaky `internal/live` and `internal/tui` tests.
