# Shared Sessions Design

**Date:** 2026-09-12
**Status:** approved (design sections reviewed in conversation)
**Builds on:** `2026-09-11-session-handoff-design.md` (live sessions, v0.5.0)
**Target version:** 0.6.0

## Problem

Live sessions (v0.5.0) mirror one running TUI to every attached terminal, but
two things stop them being *shared work sessions*:

1. **A session code can fork.** Resuming a session that is already live
   somewhere else loads the saved file into a second program. The user hit
   this: a fresh `be-code` on the tablet, then "Resume a saved session" in the
   session manager picked the desktop's live session. Both hosts then owned
   one session file, each blind to the other's turns and overwriting each
   other's autosaves. The shell path (`--resume CODE`) refuses when the code
   is live; the in-TUI picker, `/resume`, and plain mode do not.
2. **One keyboard.** The newest attacher holds input; everyone else watches or
   takes over with `Ctrl+] t`. Two people cannot each compose a message.

## Goals

- One live instance per session code, anywhere. Every entry point that loads
  a session joins the live one instead of forking it.
- Every attached terminal has its own input line. The transcript, wheel,
  header, bottom line and every modal remain one shared rendering.
- Everything the input line does today (slash palette, autocompletion,
  history, Up to edit a queued message, Enter queues while busy) works per
  terminal.
- No new on-disk formats; records, sockets, sessions are unchanged apart from
  a pid stamp on the session file.

## Non-goals

- Per-client rendering of the transcript (own scroll position, own size, own
  selection). The shared minimum size and shared modals stay.
- A network transport. Remote terminals reach the host machine over SSH and
  run `be-code attach` there, as in v0.5.0.
- Presence beyond labels (no cursors of other users, no typing indicators).

## Section 1: One live instance per session code

The live record directory (`~/.be-code/live/`) is the authority on where a
session is running. Every path that loads a session consults it first.

| Entry point | Code is live | Code is not live |
|---|---|---|
| `be-code --resume CODE` (launcher, hosted) | attaches to the live host; no new host is spawned; prints `joining live session CODE` | unchanged: spawns a host that resumes the file |
| `be-code attach CODE` | unchanged | unchanged: falls back to `--resume` |
| Session picker in a served TUI (`/menu → Resume a saved session`, `/resume CODE`) | row is marked `LIVE`; selecting it *switches* the selecting terminal (below) | unchanged: loads the file into the current program |
| Session picker / `/resume` in an in-process TUI (`--no-host`, `host_sessions: false`) | prints `CODE is live elsewhere; join it with: be-code attach CODE`; nothing loaded | unchanged |
| Plain mode `/resume` | same message as the in-process TUI | unchanged |
| Startup offer for a workspace with a live session | unchanged (offer once, newest record, respects `--resume`) | n/a |

**Switching.** When a served TUI's picker selects a live code, the program
asks the host to switch the selecting client: the host sends that client
`bye{reason: "switch:CODE"}` and drops it. The attach client recognises the
prefix, loads the record for `CODE`, and reconnects in the same terminal,
printing nothing; the screen is cleared and repainted by the new host as on
any attach. If the record for `CODE` is gone by then, the client prints
`CODE ended before you could join it` and returns to the shell.

After the switch, if the current host is a fresh session (no turns) with no
remaining clients, it quits itself so empty hosts do not accumulate. A host
with turns, or with other clients, keeps running.

**Save guard.** `store.Session` gains `HostPID int` (JSON `hostPid`,
omitempty). A hosted program stamps its pid on every save. Before saving, the
agent reloads the on-disk stamp; if it names a different, live process, the
save is skipped, autosave stays off for the rest of the run, and the
transcript shows one dimmed warning: `session file is owned by live host
<pid>; autosave disabled for this session`. This is the last line of defence,
not the primary mechanism. In-process runs stamp their own pid too; a stale
stamp (dead pid) is ignored.

## Section 2: Client-tagged input

The host no longer feeds raw bytes into a pipe. It creates the program with a
nil input (Bubble Tea then runs no input reader), parses each client's bytes
with `github.com/charmbracelet/x/input` (v0.3.7), converts every event to the
v1 message types (`tea.KeyMsg`, `tea.MouseMsg`), and sends them to the
program as:

```go
type ClientKeyMsg struct { Client int; Key tea.KeyMsg }
type ClientMouseMsg struct { Client int; Mouse tea.MouseMsg }
```

The conversion lives in `internal/live/keys.go` as a table from `x/input`
key codes and modifiers to `tea.KeyType`/runes/alt, plus mouse buttons and
actions. Unknown events are dropped. Paste events (bracketed paste) become the
same `KeyRunes` message with `Paste: true` that Bubble Tea's own reader
produces.

The holder concept is removed: every attached client's keys are delivered,
tagged. The `takeover` frame and the `Ctrl+] t` chord are removed; `Ctrl+] d`
still detaches; a `--view` client still sends nothing.

### Routing inside the TUI

`handleKey` and `handleMouse` take the client id (0 for the in-process TUI,
which has exactly one client and is otherwise unchanged).

- **Input mode:** the key goes to that client's own textarea. Enter submits
  that client's text: it starts a turn if the agent is idle, or queues it
  while busy, exactly as today. Because the frame is one shared rendering,
  the transcript's user line is prefixed `<label>:` for every sender
  (including your own) whenever more than one client is attached; with a
  single client the prefix is omitted, as today.
- **Queue:** `Agent.Enqueue` records the sender's client id with the message.
  Up on an empty input (or `Ctrl+Q`) opens the queue popup showing only the
  opener's messages; editing pulls the message into the opener's textarea.
- **Palette:** `/` typed by a client opens the palette filtered by that
  client's text; it is drawn in the shared frame and driven by the client that
  opened it (other clients' keys are ignored while it is open; Esc from the
  owner closes it; it also closes if the owner detaches).
- **Modal modes** (approval, picker, plan, menu, context menu): shared, driven
  by whichever client presses keys; Esc from anyone closes them.
- **Mouse:** wheel scrolling and selection stay shared; the last client to
  drag owns the selection; a per-client selection is out of scope.
- **Detach from the keyboard:** `/detach` detaches the client that typed it.
  `/clients` lists labels. The bottom line's clients marker becomes
  `⧉ N · <label>, <label>` (labels truncated to fit; in compact only `⧉ N`).

### Per-client textareas

The served model keeps `inputs map[int]*textarea.Model` keyed by client id,
created on attach (empty) and dropped on detach (unsent text discarded; queued
messages live in the agent and survive). Each textarea is sized to the shared
width minus the wheel column and has a fixed height: three rows in the full
layout, one in compact. History is per client. The in-process TUI keeps its
single textarea as client 0.

## Section 3: Per-client input rows on the wire

The shared frame is rendered once by Bubble Tea with the input rows left blank
(the `View` emits empty rows of the fixed input height in served mode; the
wheel column still renders at the right of those rows).

After handling a key for client *c*, the model renders *c*'s textarea and
publishes it through `Host.SetOverlay(c, rows string)`. The host stores the
overlay under a mutex and:

- writes it to client *c* immediately, as an `overlay` frame (the client
  writes it verbatim, like `output`), positioned by an absolute cursor move to
  the first input row, column 1, each row followed by a clear-to-end-of-line
  up to the wheel column;
- appends each client's current overlay to that client's copy of every shared
  frame the program flushes (Bubble Tea's standard renderer performs one
  `Write` per frame, verified at v1.3.10), so a full repaint never wipes a
  half-typed message.

The overlay ends by parking the cursor at the last row, last column (the real
cursor is hidden; the textarea's styled cursor marks the caret, as today). A
new client gets an empty overlay on attach; overlays for detached clients are
dropped.

## Section 4: Protocol, commands, docs

**Frames.** Removed: `takeover`. Added host→client: `overlay` (payload: bytes
to write). `bye` reasons gain `switch:CODE`. `clients` drops the `holder`
field. Everything else (hello, input, resize, detach, quit, output, size, bye)
is unchanged. Records, sockets and the session file (bar `hostPid`) are
unchanged.

**Host API.** `InputReader()` is removed; `OnInput(func(client int, b []byte))`
replaces it (the served TUI wires it to the parser). `DetachHolder()` becomes
`Detach(client int)`. `Switch(client int, code string)` sends the switch bye.
`SetOverlay(client int, s string)` as above.

**Commands.** `attach` and `sessions` unchanged. `/clients`, `/detach`,
`/resume` and the picker as in Sections 1 and 2.

**Config.** No new keys.

**Docs.** README "Live sessions and handoff" rewritten around shared sessions
(everyone has an input line, `<label>:` prefixes, switching from the picker,
shared modals, `Ctrl+] d` only). CHANGELOG `v0.6.0 — shared sessions`. Root
CLAUDE.md "Live sessions" updated (tagged input, overlay, join-not-fork, save
guard). `docs/live-checklist.md` gains: two terminals typing at once, resuming
a live code from a second terminal (`--resume` attaches), and the picker
switch.

## Section 5: Failure handling

- Malformed bytes from a client are dropped by the parser; other clients are
  unaffected.
- An overlay write failure detaches that client like any other write failure
  (bounded, never blocks the program).
- Switch to a vanished host: client prints `CODE ended before you could join
  it` and exits; the source host has already dropped the client and, if it was
  an empty session with no clients left, has exited.
- Save guard as in Section 1: never silently merge or lose work.
- The palette owner detaching closes the palette; a modal never gets stuck
  waiting for a client that left.

## Testing

- `internal/live/keys_test.go`: conversion table for every `tea.KeyType`,
  runes with and without alt, ctrl combinations, paste, and mouse buttons and
  actions.
- Host tests: tagged delivery from two clients interleaved; overlay goes to
  the addressed client only; overlay appended after a full frame for every
  client; `Switch` sends the right bye and drops the client; `Detach(client)`.
- Client tests: `overlay` frames are written verbatim; `switch:CODE` reconnects
  to the new record (two hosts in one test); vanished target prints the
  message and exits.
- TUI tests: two simulated clients typing interleaved text land in their own
  textareas; Enter from one queues only that text with the sender recorded;
  the queue popup shows only the opener's messages; the palette is owned by
  the client that typed `/` and closes when it detaches; `View` leaves the
  input rows blank in served mode and renders a single textarea in-process;
  picker marks live rows and selecting one emits the switch.
- Launcher tests: `--resume` of a live code attaches instead of spawning; the
  in-process paths print the "live elsewhere" message.
- Store/agent tests: the save guard skips a save when another live pid owns
  the file and ignores a dead stamp.
- Pty walk against the real model: two terminals typing at once (each sees its
  own line, both see both messages in the transcript), a `--resume CODE` from a
  third terminal joining the live session, and a picker switch from a fresh
  session into the live one.

## Resolved questions

- Transcript prefix: `<label>:` for every sender when more than one client is
  attached; none with a single client. (One rendering means no `you:`.)
- Input height in served mode: fixed three rows (one in compact); the textarea
  scrolls internally for longer messages.
- The `takeover` chord is removed rather than kept as a no-op.
