# Live sessions — manual checklist

The session host, the attach client and the shared/compact rendering cannot be
fully covered by unit tests (they need real terminals of different sizes). Walk
this once per release, or after touching `internal/live`, `internal/tui/served.go`,
`internal/tui/compact.go` or `cmd/live.go`.

1. In VS Code's terminal: `be-code`. Expect the TUI as before; `/clients` shows one client.
2. Second terminal: `be-code sessions` → LIVE column shows the code; `be-code attach <code>`.
   Expect the same screen, "attached: … now holding input" in both, `⧉ 2` in the bottom line.
3. Type in the second terminal: it drives the session; the first is a viewer.
   `Ctrl+] t` in the first takes input back.
4. Resize the smaller terminal: both views relayout to the smaller size.
5. `Ctrl+] d` in the second: "detached … still running". Close the VS Code window
   entirely; from another terminal: `be-code attach <code>` — the session is still there.
6. `ssh localhost be-code attach <code>` — same as 2 over SSH.
7. From a phone SSH app: attach; expect the compact layout (no header, short bottom
   line, `>` prompt).
8. `/quit` from any client: every client prints the resume line and returns to its
   shell; `be-code sessions` no longer lists it live.
9. `be-code --no-host`: runs in-process; `/clients` says not served.

If a session never appears, `~/.be-code/live/<code>.log` has the host's own
stdout/stderr from startup; `~/.be-code/live/<code>.json` is its record (code, pid,
socket, workspace, model, auth token).
