# In-app updater — design

**Status:** approved in brainstorming, 2026-10-05. Branch `feature/updater` off `main`.

## Purpose

Let a person learn that a newer BE-Code release exists and install it from inside the app, without
leaving the session to re-run the installer. Finding out is automatic and quiet; installing is
always the person's choice.

## Decisions taken in brainstorming

| Question | Decision |
| --- | --- |
| When is the network asked? | A quiet background check at session start that only sets the "update available" flag; `update_check: false` turns it off. |
| What does Update do? | Installs in place: download, verify against `SHA256SUMS`, swap the binary, "restart BE-Code to use vX.Y.Z". |

## §1 Check

- `internal/update` (new, stdlib only): `Latest(ctx, client) (Release, error)` reads
  `https://api.github.com/repos/BE-AI-Research/be-code-redux/releases/latest` (`tag_name`,
  `html_url`, asset names/URLs). The base URL is a package variable so tests point it at an
  `httptest` server.
- `Newer(current, latest string) bool`: semantic `major.minor.patch` comparison, a leading `v`
  ignored; a current version of `dev` (or anything unparsable) is never offered an update.
- At session start (TUI, hosted and plain; never headless `run`, never `init`) one goroutine runs
  the check under a 5 s timeout. Any failure (offline, rate limit, bad JSON) is silent. It never
  delays startup and never blocks a request.
- `update_check` (config, default `true`): `false` skips the start check. The menu item still
  works.

## §2 Notice

- The flag lives on the TUI `Session` (`updateAvailable string`, the version) and is broadcast to
  every attached view, so each terminal shows it.
- The TUI bottom line ends with ` · ⬆ v<X.Y.Z> available` (dim) while the flag is set; compact
  layouts drop it first when space runs out.
- Plain mode prints one dim line when the start check finds a newer version:
  `BE-Code v<X.Y.Z> is available — /update installs it`.

## §3 Update

- `/update` (slash command, both UIs, in `ui.SlashCommandTable`, busy-safe) and a **Check for
  updates** row in `/menu` run the check again (not using the cached flag), then:
  - none newer → `BE-Code v<cur> is the latest`;
  - newer → ask `update` (`Update BE-Code <cur> → <new>?`), an approval with **no "always"**
    (TUI `a` handler, REPL `noAlwaysAction`, `headlessApprover` refuses; `-y` never answers it);
  - yes → install (§4), then `updated to v<new>; restart BE-Code to use it` and the flag clears.
- The running session, and any live hosts, keep the old binary until restarted.

## §4 Install

- Asset name `be-code-<GOOS>-<GOARCH>` (`.exe` on Windows) — the names `make release` and
  `install.sh` already use. Missing asset → `no v<new> build for <os>/<arch>; see <html_url>`.
- Download the asset and `SHA256SUMS` (bounded size, 2 min timeout). No `SHA256SUMS`, no line for
  the asset, or a mismatch → nothing is replaced: `v<new> could not be verified; not installed`.
- Target is `os.Executable()` with symlinks resolved. Write to a temp file in the same directory,
  `chmod 0755`, then `os.Rename` over the target (atomic on Unix). On Windows the running `.exe`
  cannot be overwritten: rename it to `be-code.exe.old` first, then move the new one in; a
  leftover `.old` is removed on the next update.
- Directory not writable → nothing changed:
  `cannot write <dir>; update with: curl -fsSL https://raw.githubusercontent.com/BE-AI-Research/be-code-redux/main/install.sh | sh`.
- A `dev` build never installs (the menu says it was built from source).

## §5 Also

- `doctor` gains `update: v<cur> (latest v<new>)` or `update: v<cur> is the latest`, or
  `update check off` / `could not reach GitHub` — one check, 5 s.
- README (command reference, config reference, a short "Updating" note under Install) and
  CHANGELOG.

## §6 Testing

All against `httptest` servers; no real network; never touches the real `~/.be-code` or the real
binary (the target path is injectable).
- `Newer` cases (`1.2.0` < `1.2.1` < `1.10.0`, `v` prefix, `dev`, garbage).
- `Latest` parses a release; non-200, bad JSON and timeout are errors.
- Install: good checksum replaces the target; mismatch, missing `SHA256SUMS`, missing asset leave
  it untouched; unwritable dir gives the curl message; temp file cleaned up on failure.
- TUI: flag shows in the bottom line on every view; `/update` asks with no "always"; `a` ignored.
- REPL prompt `[y/N]`; headless approver refuses `update`; `update_check: false` makes no request.

## §7 Out of scope

Automatic installs; release channels or pre-releases; downgrades; updating the VS Code extension;
restarting the session for the person.
