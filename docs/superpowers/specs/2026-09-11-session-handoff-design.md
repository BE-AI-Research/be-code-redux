# BE-Code session handoff — design

Date: 2026-09-11. Status: approved in conversation, pending written review.

## Goal

A running BE-Code TUI session can be attached to from any other terminal
that can reach the machine it runs on (another window, an IDE terminal, an
SSH login, a phone SSH app) using its resume code. The session keeps
running where it started, every attached terminal mirrors it, the most
recent attacher holds the keyboard, and the session survives all terminals
going away. BE-Code hosts this itself; no BE-CLI server is required.

## Approach

"Wish-style" in-process session server: the TUI process is the session. It
starts detached from the launching terminal and serves its Bubble Tea
program over a local socket. Clients are thin raw-mode pipes forwarding
keystrokes and window sizes; all clients receive identical rendered frames;
the program runs at the smallest attached terminal's size; a new attacher
triggers a full repaint (no PTY, no scrollback replay).

## Process model and lifetime

- `be-code` (TUI mode) becomes a launcher: it creates the session record and
  resume code, starts a detached `be-code --session-host <code>` (setsid on
  Unix; detached process group on Windows) with stdio to a log file, then
  attaches to it like any other client.
- The host listens on `~/.be-code/live/<code>.sock` (Windows:
  `\\.\pipe\be-code-<code>`) and writes `~/.be-code/live/<code>.json`
  {pid, socket, workspace, model, startedAt, token} (mode 0600).
- The host inherits the launcher's environment, so `TERM_PROGRAM=vscode`
  and IDE lock discovery keep working; the editor bridge attaches once, in
  the host, and persists across handoffs.
- The host exits when the TUI quits (same exit sequence: handoff briefing,
  resume line to every client), removing socket and record. `be-code
  sessions kill <code>` sends `quit`; an unresponsive host is signalled.
  Optional `live_idle_limit` (minutes, default off) ends a host with no
  clients and no running turn. Dead-pid records are pruned by
  `be-code sessions`.
- Headless `run` and `--plain` never host. `--no-host` runs the TUI
  in-process (no attach) for environments without sockets.

## Attach protocol and handoff

- One socket connection per client. Frame: 1-byte type, 4-byte big-endian
  length, payload. Client→host: `hello` {token, cols, rows, label, utf8},
  `input` (bytes), `resize` {cols, rows}, `detach`, `takeover`, `quit`.
  Host→client: `output` (bytes), `size` {cols, rows}, `clients` (list with
  holder marked), `bye` {reason}. Wrong token → `bye` + close.
- The most recently attached client holds input; others are viewers whose
  keystrokes are swallowed except the chords Ctrl+] `t` (takeover) and
  Ctrl+] `d` / Ctrl+] Ctrl+] (detach). Holder detach passes input to the
  most recently attached remaining client. A failed write marks a client
  detached.
- Attach: recompute shared size, send `size`, send the program a
  `WindowSizeMsg` → full repaint. Detach restores the terminal and prints
  `detached from <code> (still running); be-code attach <code> to return`.
- Ctrl+C is forwarded as input (its TUI meanings are unchanged).

## Shared sizing and rendering

- Program size = min cols × min rows over attached clients; recomputed on
  attach, detach, resize; each change sends one `WindowSizeMsg`.
- Output goes to a fan-out writer; clients clear their screen on attach and
  on every `size` change. Each client sets raw mode, mouse reporting and
  bracketed paste itself and restores them on detach.
- Host runs Bubble Tea with `WithInput`/`WithOutput` on internal pipes and
  an explicit size (as `wish` does). OSC 11/10 theme colours and OSC 52
  copies go through the fan-out writer; the reset goes to all clients on
  exit.
- Per-client bounded frame queue: a slow client drops older frames (each
  frame is a full repaint) and never stalls the others.

## Compact layout (small shared size, including phones)

- `layout: auto | compact | full` (default auto). Auto engages compact when
  the shared size is under 70 columns or 20 rows.
- Compact: header and attribution hidden; bottom line `/menu · <model
  short> · <state>` with queue count `⧉N` and IDE marker `⌘`; input prompt
  `>`; wheel glyph + percentage only; popups drop the description column
  under 60 columns and cap at 6 rows; `/menu` status condensed to two lines
  and label-only entries; approval/plan modals full-screen with wrapped
  diffs and a trimmed hint line; tool-call argument previews truncated to
  the width.
- ASCII fallbacks (`o`/`*`, `IDE`, `[2]`) when the client's `hello`
  reports no UTF-8.
- Trade-off: while a small client is attached, all views run at its size.
  Per-client rendering is a possible follow-up, out of scope here.

## Commands and the TUI's view of clients

- `be-code` in TUI mode: if a live session exists for this workspace, ask
  whether to attach or start new. `be-code attach <code|last>`; `--view`
  attaches as a viewer. `be-code sessions` gains a LIVE column
  (`live · 2 clients`, `live · idle`, `-`) and `sessions kill <code>`.
- TUI: `/clients` lists clients (holder marked); `/detach`; both under
  `/menu › Sessions`. Client changes arrive as a message: bottom line shows
  `⧉ N` when more than one client; dimmed transcript lines record
  `attached: pts/3 (ssh from 10.0.0.5), now holding input` / `detached:
  vscode`; a viewer's bottom line ends `viewing · Ctrl+] t to take over`,
  the holder's `Ctrl+] d to detach`.
- `be-code attach <code>` tries a live host first and falls back to
  `--resume <code>` when none is listening; the same code serves both.

## IDE and headless interactions

- The editor bridge lives in the host: `ide_*` tools, editor diff review and
  the context note keep working after the IDE terminal detaches. If VS Code
  closes, tools error clearly and approvals fall back to the TUI on the
  client holding input. `reviewing change in VS Code…` shows on all clients.
- Headless `run` and `--plain` are unchanged.

## Testing

- Protocol: framing round-trip, token rejection, holder election, shared
  size recomputation, bounded queue drops frames without blocking.
- Host with in-memory sockets: two fake clients receive identical frames; a
  `WindowSizeMsg` per size change; takeover and detach chords.
- TUI: compact layout at 60×18; clients message renders marker and lines.
- Live checklist: IDE terminal → `ssh localhost be-code attach <code>`;
  handoff line; edit a queued message from the second terminal; detach and
  reattach; phone SSH app for the compact layout.

## Out of scope

Per-client rendering; BE-CLI server backend; sharing across users (the
record is 0600, same-user only); web client.
