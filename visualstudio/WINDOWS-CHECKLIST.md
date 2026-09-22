# Windows checklist — proving the Visual Studio extension

Nothing in `src/BECode.VisualStudio` has ever run. It compiles against the real SDK, so names and signatures are settled; this list is how its *behaviour* gets proven. It is ordered by where a failure is most likely and most informative, so stop at the first step that fails and send back what **"If a step fails"** asks for — the rest can wait.

Do it once on Visual Studio 2022 (17.6 or later) and once on 2026. Tick as you go.

## If a step fails — what to send back

1. `%USERPROFILE%\.be-code\visualstudio.log` — the extension's own log (every entry is mirrored here precisely because the Activity Log may not be reachable from a background thread).
2. **Help › View Activity Log** in Visual Studio (or `%APPDATA%\Microsoft\VisualStudio\<version>\ActivityLog.xml`; start `devenv /log` if it is empty). Search for `BECode`.
3. The lock file, `%USERPROFILE%\.be-code\ide\<pid>.json`, **with the `token` value blanked**.
4. The output of `be-code --ide` and of `be-code doctor`, run in the project directory.
5. Visual Studio's exact version (**Help › About**) and whether the machine is x64 or Arm64.

## A. Build, install, load — the likeliest first failure

- [ ] **A1.** `cd visualstudio; .\build.cmd` (a launcher that runs `build.ps1` past PowerShell's default script block: `powershell -NoProfile -ExecutionPolicy Bypass -File ".\build.ps1"`) finishes and prints the path of `dist\be-code-visualstudio-0.1.0.vsix`. *(Not `be-code-vscode-…vsix` — that one is the VS Code extension, and Visual Studio will refuse it.)* It also lists what is inside: `BECode.VisualStudio.dll`, `BECode.Bridge.dll`, `System.Text.Json.dll`, `System.Threading.Channels.dll` and the rest of the small BCL set must be there; no `Microsoft.CodeAnalysis*`, no `Microsoft.VisualStudio*`.
  *If it says the extension-development workload is missing, install it from the Visual Studio Installer.*
- [ ] **A2.** The `.vsix` installs into Visual Studio 2022, and **Extensions › Manage Extensions › Installed** lists it as **BE-Code for Visual Studio**, version 0.1.0. *(On an Arm64 machine this is also the test that the Arm64 install targets are right.)*
- [ ] **A3.** The same `.vsix` installs into Visual Studio 2026.
- [ ] **A4.** Start Visual Studio **with no solution**. Within a few seconds `%USERPROFILE%\.be-code\visualstudio.log` gains the line `listening on port N, 0 workspace folder(s), VS <version>`, and `%USERPROFILE%\.be-code\ide\<pid>.json` exists with `"ideName":"visualstudio"` and `"workspaceFolders":[]`.
  *No log line and no lock file means the package loaded but could not resolve `BECode.Bridge.dll` or one of its dependencies inside `devenv.exe` — the single most likely failure. The Activity Log will name the assembly (`FileNotFoundException` / `FileLoadException`). Send it back.*
- [ ] **A5.** Visual Studio starts no slower than before, and nothing is pinned to the top of its window.

## B. Attaching — the second likeliest

- [ ] **B1.** With Visual Studio still open from A4, **open a solution**. Within a second or two the log gains a `republished lock` line and the lock file's `workspaceFolders` now lists the solution directory and each project's directory.
- [ ] **B2.** Open a terminal **outside** Visual Studio (Windows Terminal), `cd` into the solution, run `be-code` — no flags. Its startup says `Visual Studio connected: 16 tools`; `/tools` lists sixteen `ide_*` tools (no `ide_review_diff`, no `ide_review_cancel`).
  *If it does not attach, run `be-code --ide` once: if THAT attaches, the lock's folders do not cover your directory — send the lock file and the output of `cd` (PowerShell: `(Get-Location).Path`).*
- [ ] **B3.** `cd` to a directory that is **not** under the solution and run `be-code`: it must **not** attach.
- [ ] **B4.** Open a second Visual Studio on a different solution. Each `be-code`, started under its own solution, attaches to its own Visual Studio (ask it `what file am I looking at?` in each).
- [ ] **B5.** **File › Open › Folder** on a plain folder, close Visual Studio, reopen it so the folder is restored at startup: the lock lists that folder, and `be-code` inside it attaches.
- [ ] **B6.** Close Visual Studio: its lock file disappears.

## C. The editor's view

- [ ] **C1.** Open a C# file, caret on **line 10**, nothing selected. Ask: `what am I looking at?` → the file, relative to the solution, and **line 10** — not 9, not 11.
- [ ] **C2.** Select lines 5–8 (drag upward, from 8 to 5). Ask again → lines **5–8** and the selected text.
- [ ] **C3.** Ask `open <some other file> at line 40` → it comes to the front at line 40. Note whether focus stayed where you were typing (it matters only in Visual Studio's own terminal).
- [ ] **C4.** With 30 or more documents open, `what am I looking at?` still answers without Visual Studio visibly pausing.
- [ ] **C5.** Caret on a method name in C#: `where is this defined?`, `who calls this?`, `what is this?` → a location; a count that matches **Find All References**; a signature followed, after a blank line, by its doc summary, with no markup.
- [ ] **C6.** The same three on a `.json` or `.cpp` file → `not available for this file type`. On `string` or another framework type, `where is this defined?` → nothing found, not an error.

## D. Diagnostics — off by one here would be the most visible bug

- [ ] **D1.** Set the Error List to **Entire Solution** and **Build + IntelliSense**. Put a deliberate error on a known line and column. Ask `what errors are there?` → the same file, the **same line and column** the Error List shows, the error code, the message.
- [ ] **D2.** **Build** the solution with that error in place and ask `what errors are in <that file>?` → the error is listed. *(A build error may carry a bare file name rather than a full path; an empty answer here is that bug.)*
- [ ] **D3.** On a large solution, after a full rebuild with thousands of warnings, `what warnings are there?` answers; if the list was cut, it says so (`Error List truncated at 5000 …`). Note any freeze.

## E. Reviewing a change

- [ ] **E1.** Ask BE-Code for a small edit to one file. A diff tab opens — *Current* against *Proposed* — with a bar naming the file and offering **Accept**, **Accept all this session**, **Reject**. Note where the bar appears: on the diff tab (expected) or across the top of Visual Studio (the fallback).
- [ ] **E2.** **Accept** → the file changes on disk, the tab closes, the bar goes.
- [ ] **E3.** **Reject** → the file is untouched and BE-Code says the change was declined.
- [ ] **E4.** Close the diff tab with its ✕ instead of answering → treated as cancelled; no bar is left behind.
- [ ] **E5.** **Accept all this session**, then ask for a second edit → it lands without a diff. Restart `be-code` → the next edit asks again.
- [ ] **E6.** With the diff open, answer **in the terminal** instead (from a separate terminal, review resolves to `both`). The diff tab closes by itself, **no bar is left pinned to Visual Studio**, and the very next `ide_*` question still answers at once. *(This is the withdrawal that used to deadlock.)*
- [ ] **E7.** An edit that creates a new file, and one that empties a file, both show a sensible diff.

## F. Debugging

- [ ] **F1.** `what can I debug here?` → every project in the solution, **including ones inside solution folders**, the startup project first; launch profiles shown as `project › profile`.
- [ ] **F2.** `set a breakpoint at <file>:<line>` → it appears in the margin. `start debugging` → `stopped (breakpoint)` at that file and line, within 60 s. *(This changes nothing if the startup project is already right; naming another project **changes the solution's startup project** — confirm you are content with that.)*
- [ ] **F3.** `show the call stack` → frames with files and lines; `show local variables` → locals with values, and for a small scope one level of members; `evaluate <expression>` → its value.
- [ ] **F4.** `step over`, `step into`, `step out`, `continue` each report where execution stopped, or that the program exited.
- [ ] **F5.** `show the debug output` → the Output window's Debug pane; asking again returns only what is new.
- [ ] **F6.** Break the build, then `start debugging` → `build failed (n projects)` once the build has finished, not a 60-second wait, and not a false "build failed" while a slow build is still running. Fix it and start again → it reaches the breakpoint.
- [ ] **F7.** `start debugging <a launch profile's name>` → a clear message to select that profile in the toolbar. `start debugging` with a VS Code-style `program`/`type` → a clear refusal.
- [ ] **F8.** `stop debugging` → back to design mode. Visual Studio never froze during F2–F7.

## G. Long-run hygiene

- [ ] **G1.** After an hour of use, Visual Studio is as responsive as without the extension; `visualstudio.log` holds no repeating error.
- [ ] **G2.** Close the solution and open another without restarting Visual Studio → the lock follows (B1 again).
- [ ] **G3.** *Non-English Visual Studio only:* F5 still finds the Debug pane.

## What is not expected to work in this version

Definition, references and hover outside C# and VB; choosing a launch profile from BE-Code; the debuggee's exit code; attach-to-process. See `README.md`.
