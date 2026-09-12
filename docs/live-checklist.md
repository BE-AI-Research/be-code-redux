# Shared sessions — manual checklist

The session host, the attach client and the shared/compact rendering cannot be
fully covered by unit tests (they need real terminals of different sizes). Walk
this once per release, or after touching `internal/live`, `internal/tui/served.go`,
`internal/tui/inputs.go`, `internal/tui/compact.go` or `cmd/live.go`.

1. In VS Code's terminal: `be-code`. Expect `starting session <code>`, the TUI as
   before, and `/clients` showing one client.
2. Second terminal: `be-code sessions` → LIVE column shows the code; `be-code attach <code>`.
   Expect the same screen in both, `⧉ 2` plus both labels in the bottom line.
3. Type in both terminals at once, without pressing Enter: each terminal shows
   only its own draft in its input rows. Press Enter in each: both messages
   appear in the shared transcript, each prefixed with the label of the terminal
   that sent it, and the other terminal's unsent draft is still on its screen.
4. Third terminal: `be-code --resume <code>`. Expect `joining live session <code>`,
   a full frame, `⧉ 3`, and no prompt or refusal — it joins, it does not fork.
5. Fourth terminal: `be-code --new` in the same workspace. Expect a *fresh*
   session (a different code, `starting session <code>`) rather than a join.
   In it, `/menu` → "Resume a saved session" → the row marked `LIVE`: this
   terminal switches into the live session (its transcript is the shared one)
   while the other three stay where they were, and `be-code sessions` no longer
   lists the fresh code as live (the empty host exited).
6. Resize the smallest terminal: every view relayouts to the smaller size.
   In an attached terminal, drag-select transcript text and paste a multi-line
   snippet: selection highlights and the paste arrives as one message.
7. `Ctrl+] d` in one terminal: `detached from <code> (still running)`. Close the
   VS Code window entirely; from another terminal `be-code attach <code>` — the
   session is still there.
8. `ssh localhost be-code attach <code>` — same as 2 over SSH.
9. From a phone SSH app: attach; expect the compact layout (no header, short
   bottom line, `>` prompt) and an input line of its own.
10. `/quit` from any client: every client prints the resume line at column 0 and
    returns to its shell; `be-code sessions` no longer lists it live and no
    `be-code --session-host` process is left.
11. `be-code --no-host`: runs in-process; `/clients` says not served, and
    `/resume` of a code that is live elsewhere prints
    `<code> is live elsewhere; join it with: be-code attach <code>` and loads
    nothing.

If a session never appears, `~/.be-code/live/<code>.log` has the host's own
stdout/stderr from startup; `~/.be-code/live/<code>.json` is its record (code, pid,
socket, workspace, model, auth token).
