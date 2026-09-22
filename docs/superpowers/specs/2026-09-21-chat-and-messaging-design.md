# Chat and messaging — design

Status: approved in conversation 2026-09-21; this is the written form. Target: BE-Code 0.15.0.

## 1. What this is

Two things people who share a BE-Code session, or a machine, have asked for, decided together in one design because they share an identity record and a TUI:

- **Chat, per session.** A room every terminal attached to one live session can see and post to, routed by that session's host. `@agent` in the room hands the line to the session's model, which replies in the transcript and in the room.
- **DMs and an inbox, machine-wide.** A person on one session can message a person on another session on the same machine; messages wait in an inbox for someone who is not attached anywhere. No daemon: a shared mailbox on disk, which every session host watches.

The owner's sketch (`/dm`, `/inbox`, `/chat` as three TUI layouts, "all messaging is routed via the host", identity by user ID plus IP/PID with MAC as a fallback, "@mention the AI allows it to join a chat") is the source; the decisions below are the rulings taken in conversation.

### Decisions, as made

| Question | Decision |
|---|---|
| Scope | Chat is one session's; the inbox spans the machine. |
| User IDs | `chat.name` in the device's own config wins; else an ID already bound to the terminal's IP is offered; else the terminal is asked once and the answer is bound to that IP. |
| Device identity | IP and PID are the identity. MAC is looked up (best effort, ARP) **only when a terminal arrives from an IP that is not known**, to recognise a device whose IP changed. |
| Delivery | A shared mailbox on disk under `~/.be-code/inbox/`; each session host watches it. No `msgd`, no router election. |
| `@agent` | The mentioning line plus the last `chat.mention_context` (10) room lines go to the model as one request through the existing queue; the reply lands in the transcript and is copied to the room. `@agent` in a DM is plain text. |
| Modes | `/chat`, `/inbox`, `/dm` are view modes of the same session on the calling terminal; the session keeps running underneath; `Esc` or `/back` returns. Other terminals are unaffected. |
| Plain mode, headless | Not supported; the commands say `chat and DMs need the TUI`. |

## 2. Architecture

```
terminal A ─┐                                   ┌─ terminal C
terminal B ─┼─ session host 1 ── Room (memory) ─┤
            │        │                          └─ (chat: A, B, C only)
            │        └── watches ~/.be-code/inbox/ ── writes DM files
            │                          ▲
terminal D ─── session host 2 ─────────┘   (DM from D to A: host 2 writes,
                                             host 1 notices, pings A)
                    ~/.be-code/users.json  (both hosts read/write)
```

- **Room**: a struct on `tui.Session`, guarded by the session's `mu`, broadcast to every `View` through the existing mailbox. Saved with the session; restored on resume; cleared by `/clear`.
- **Mailbox**: `internal/inbox` — a package with no UI, no host: paths, atomic writes, a watcher, a reader. The session host owns one `inbox.Watcher`; the TUI owns the views.
- **Identity**: `internal/inbox/users.go` — `users.json` read-modify-write under a file lock (`flock` on Unix, `LockFileEx` on Windows — one helper, `internal/inbox/lock_*.go`), because two hosts may bind an ID at once.

Nothing here touches the agent except the `@agent` path, which uses `Agent.Enqueue`/`Run` as any user text does. The model never sees the room otherwise and never sees a DM.

## 3. Identity

### 3.1 What the host knows about a terminal

The client adds to its `Hello` frame: `ip` (the first field of `SSH_CONNECTION`, else `127.0.0.1`), `login` (`$USER`, else the OS user name), `pid` (its own), and `user` (`chat.name` from its config, may be empty). `live.ClientInfo` carries the same four fields, so `/clients` and the room can show them. A `Hello` without them (an older client) is `127.0.0.1`/unknown login, and the host asks for a name as below.

### 3.2 `~/.be-code/users.json`

```json
{
  "users": {
    "alice": {
      "devices": [
        {"ip": "192.168.1.38", "login": "sbrown", "mac": "aa:bb:cc:dd:ee:ff",
         "first_seen": "2026-09-21T10:00:00Z", "last_seen": "2026-09-21T14:32:00Z"}
      ]
    }
  }
}
```

Mode 0600; written atomically under the lock. `agent` is reserved and never appears. IDs are `[a-z0-9_.-]{1,32}`, lower-cased on input.

### 3.3 Resolution, on attach (`inbox.Resolve(hello) (id string, how string, ask bool)`)

1. `hello.user` set → that ID. If it is bound to another IP only, this IP is added to it (an ID is the person's; the IP list is where they have been). `how = "config"`.
2. Else, IDs whose `devices` include this IP: one → use it (`how = "ip"`); several → `ask = true` with those as choices (a terminal behind a shared IP picks which it is).
3. Else, MAC lookup for this IP (§3.4); an ID with a device of that MAC → use it and add the IP (`how = "mac"`).
4. Else `ask = true` with no choices: the terminal is prompted `Your name for chat and DMs:`; the answer is validated, bound to this IP/login, `how = "asked"`.

`ask` is raised on the terminal the first time it opens `/chat`, `/inbox` or `/dm`, not on attach, so a terminal that never uses them is never asked. Until answered, that terminal has no ID and the three commands show the prompt. "Only gives access to user IDs from that IP" is exactly step 2: a remote login is offered the IDs seen from its IP and nothing else; it can still type a new one.

### 3.4 MAC, best effort

Consulted only in step 3, never otherwise. Linux: `/proc/net/arp`; macOS/Windows: `arp -a` (through `procattr.Hide`). Only a device on the same subnet appears; a miss is a miss, not an error. The MAC is recorded on the device entry when known so the next IP change can use it. No `ping` to populate the table: an IP that has just connected over SSH is in it.

### 3.5 `/whoami`

Prints `you are alice (from config)` / `(bound to 192.168.1.38)` / `(recognised by MAC)` / `(asked this session)`, and `no name yet; /chat, /inbox or /dm will ask`.

## 4. The room (chat, per session)

### 4.1 Data

```go
type ChatLine struct {
    TS   time.Time `json:"ts"`
    User string    `json:"user"` // an ID, or "agent", or "" for a system line
    Text string    `json:"text"`
    Kind string    `json:"kind,omitempty"` // "", "join", "leave", "mention", "reply"
}
```

`Session.room []ChatLine`, capped at 2000 lines (oldest dropped). `store.Session` gains `Chat []ChatLine \`json:"chat,omitempty"\``, saved by the existing autosave and restored by `Resume`. `/clear` empties it.

### 4.2 Behaviour

- A terminal that opens `/chat` for the first time in its attach posts a `join` line; detaching posts `leave`. Both dim, both cheap to ignore.
- Posting: `Session.Post(user, text)` appends under `mu` and broadcasts `chatMsg{line}`; every view appends it to its own copy of the room. Views in `modeChat` re-render; others bump their "new since seen" counter, shown as `chat (2 new)` on the bottom line.
- Lines beginning `/` are commands, not posts.
- `@agent` handling: §6.

## 5. The mailbox (DMs and inbox, machine-wide)

### 5.1 Layout

```
~/.be-code/inbox/
  alice/
    1758473520123456789-bob.json     {"from":"bob","to":"alice","ts":"…","text":"…"}
    read.json                        {"last_read":{"bob":"…"}}
  bob/
    …
```

One file per message, named `<unix-nanos>-<from>.json`; written to `<dir>/.tmp-<rand>` then renamed; directories 0700, files 0600. A thread with `bob` from `alice`'s point of view is `alice/*-bob.json` ∪ `bob/*-alice.json`, sorted by name. `read.json` holds one timestamp **per correspondent** (`{"last_read": {"bob": ts}}`): a thread's messages at or before its mark are read. *(Amended during implementation: one mark for the whole inbox let opening the newest thread mark an older, unopened thread read too.)* Sending marks nothing; opening a thread in `/dm` sets that thread's mark to the newest line shown.

### 5.2 `internal/inbox`

- `Dir() string`; `Send(from, to, text) error`; `Thread(me, other string) ([]Message, error)`; `Threads(me string) ([]ThreadSummary, error)` (one per correspondent, newest first, with unread count and the latest line's first 80 chars); `MarkRead(me string, upTo time.Time) error`.
- `Watch(ctx, onNew func(to string, m Message))` — fsnotify on the inbox root and each user directory (created lazily; a new user directory is picked up by watching the root); a 2 s poll of directory mtimes as the fallback where inotify is unavailable or fails. Debounced: many files landing at once produce one call each, in name order.
- `Send` to an unknown ID creates the directory: an inbox exists before its owner has ever attached, so a message to a name not yet claimed waits for whoever claims it. The `/dm` view says `(not online)` when no host has that ID attached — determined by the live registry: each live record gains `users: [ids]`, which the host rewrites when its roster changes.
- Everything under `~/.be-code`, so `uninstall --purge` removes it; `be-code sessions` never prunes it.

### 5.3 The host's side

`runSessionHost` starts one `inbox.Watch`. On a new message for an ID that an attached terminal owns, the session `transient`s `DM from bob` and broadcasts `inboxMsg{to, m}` so that terminal's view bumps its unread count; a terminal in `/dm` with that thread open appends the line. A message for an ID nobody here owns is ignored (another host owns it, or nobody does yet).

## 6. `@agent`

### 6.1 Match

A room line matches when it contains `@agent` as a whole word, case-insensitive: `(?i)(^|[^\w.@])@agent\b`. `@agents`, `x@agent.com` and `@@agent` do not match.

### 6.2 Request

```
Chat mention from alice (room, 14:32). Recent chat:
  14:29 bob: the picking tests are red again
  14:30 alice: I think it's the max_dist check
  14:32 alice: @agent can you look at tests/test_picking.py?

Reply to alice in the chat.
```

The last `chat.mention_context` lines including the mention, system lines omitted. Delivered as user text: `Agent.Enqueue` while a run is in progress (it lands after the current tool results, as queued messages do), else a new turn through the same path the input line uses (`Session.submit`). One mention at a time; later mentions queue in order and are marked `(queued)` in the room. The room's line gets `Kind: "mention"`.

### 6.3 Reply

The turn's final answer is posted to the room as `agent` with `Kind: "reply"`, first line as is, the rest indented two spaces, cut at 40 lines with `(full reply in the transcript)`. Tool calls and notices are not mirrored. While the turn runs the chat footer reads `agent is working on alice's question`. Approvals raised by the turn are the ordinary shared asks and appear on every terminal.

### 6.4 Not in DMs

`@agent` in a DM is text. The inbox spans sessions; there is no one model to route to, and the live session queue is the way to reach the model.

## 7. TUI

New modes on `View`: `modeChat`, `modeInbox`, `modeDM`. Each is one terminal's own; the session's run state is untouched, and the transcript keeps accumulating behind. `Esc` (with an empty input) or `/back` returns to the transcript. Slash commands work in all three (`/back`, `/dm bob`, `/inbox`, `/chat`, `/quit`, `/whoami`, `/clients`); `ui.SlashCommandTable` gains `/chat`, `/inbox`, `/dm [user]`, `/back`, `/whoami`, marked busy-safe.

- **`modeChat`**: viewport of the room (`14:32 alice: …`; `agent` lines in the assistant style; system lines dim); own textarea; footer `chat · N here · Esc back`. Enter posts. Below `compact()` size the footer is `chat · Esc`.
- **`modeInbox`**: one row per thread, newest first: `● bob  14:31  first line of the latest message…`, `●` for unread. Up/Down select, Enter opens the thread in `modeDM`, `d` marks the selected thread read, `Esc` back. Empty: `no messages yet · /dm <name> to send one`.
- **`modeDM`**: left column of correspondents (width 18, unread marked, selected highlighted), thread on the right, own textarea below, footer `dm bob · Esc back`. `/dm` alone opens the most recent thread; `/dm carol` opens or starts one. Below 70 columns the column is hidden and `Tab` cycles threads. `(not online)` after the name when no host has that ID attached. Opening a thread marks it read up to its newest line.
- **Bottom line** (every mode): `chat (2 new)` and/or `inbox (1)` when there is anything unseen; a new DM also shows the transient `DM from bob`.
- **Naming prompt**: a small modal (reusing the picker: choices if there are IDs for this IP, plus `type a name…`), raised the first time one of the three commands runs on a terminal with no ID. Validation errors re-prompt with the reason.
- **`/clients`** shows `alice` beside each terminal's label.

Mouse selection and copy work in the three modes as in the transcript (same `selection.go` path over the mode's viewport). Themes: two new palette roles, `ChatUser` (names) and `ChatSystem` (join/leave).

## 8. Config

```json
"chat": {"enabled": true, "mention_context": 10, "name": ""}
```

`enabled: false` hides the commands (`chat is disabled in config`) and stops the host's inbox watcher; `name` is this device's user ID (§3.3 step 1); `mention_context` is the number of room lines sent with a mention (0 = the mention alone).

## 9. Errors and edge cases

- Two hosts bind the same new ID to different IPs at once: the lock serialises them; the second sees the first's binding and (step 2) offers it — the same person on two machines, or two people who chose one name; the prompt says `alice is already used from 192.168.1.40; pick another or press Enter to share it`.
- The inbox directory is unwritable: `/dm` reports `could not send: <err>` and nothing else changes; the watcher logs once to the host log.
- fsnotify unavailable (some containers): the poll fallback, with a one-line host-log note.
- A terminal detaches mid-`/dm`: nothing to do; read state was written on open.
- Session resume on another machine (a copied session file): the room comes back; the inbox does not (it is this machine's).
- `@agent` while the model is unavailable: the mention is queued like any request and the ordinary backend-unavailable notice appears in the room as a system line.
- Room over 2000 lines: oldest dropped, a system line `(older chat trimmed)` once.

## 10. Testing

- `internal/inbox`: `Send`/`Thread`/`Threads`/`MarkRead` round trips in a temp home; atomicity (no `.tmp-*` left; a crash between write and rename leaves no half file visible); `Watch` delivers a file written by another process (a `go run` helper) within 3 s under both fsnotify and the forced poll; two concurrent `Resolve` binds under the lock; ID validation; the MAC lookup parses a fixture of `/proc/net/arp` and of `arp -a` output and returns "" on a miss.
- `internal/live`: the `Hello`/`ClientInfo` extension round-trips; an old `Hello` without the fields decodes.
- `internal/tui`: posting broadcasts to every view (`chatMsg`); the unread counters; the three modes render at 195×56 and at 60×20 without panics; `Esc` returns to the transcript with the run state untouched; a mention builds exactly the request text of §6.2; the reply is posted once, cut at 40 lines; `@agents` does not match; a DM containing `@agent` reaches the mailbox and never the agent; the naming modal binds an ID.
- `internal/store`: `Chat` round-trips through save/load and is absent from a session with no room.
- e2e: a scripted scenario where two attached clients (the existing pty harness) exchange a room line and a DM, and a mention produces a reply in both places, against the mock backend.
- Live checklist (`docs/live-checklist.md` gains a section): the VM's VS Code terminal and the tablet post in the room; a DM from the tablet to a second session on the VM arrives with the `DM from` transient; `@agent` mid-run lands after the tool results; the naming prompt on a fresh IP; `/whoami` after an IP change with the ARP fallback.

## 11. Out of scope (this version)

Cross-machine delivery; message editing or deletion; attachments; read receipts beyond one timestamp; notifications outside the TUI; any authentication — identity is advisory and anyone with a shell on the host can read `~/.be-code/inbox/`, which the README will say plainly.
