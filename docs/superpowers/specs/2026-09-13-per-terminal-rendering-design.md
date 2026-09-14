# Per-Terminal Rendering for Shared Sessions — Design

**Date:** 2026-09-13
**Status:** approved (design sections reviewed in conversation)
**Builds on:** shared sessions (0.6.0), shared review prompt (0.7.0)
**Target version:** 0.8.0

---

## Problem

A shared session renders one frame at the smallest attached terminal's size and
the host copies those bytes to every client. When a phone attaches over SSH, the
desktop that started the session is squeezed into the phone's format: the
compact layout, forty columns, a short transcript. Per-client input rows were
spliced in as overlays in 0.6.0, but the frame under them is still one frame.
Themes are global too: `/theme` from any terminal recolours all of them.

The user's requirement: "a second process that mirrors the host terminal, but
formatted for the device it's connected to, so that the host terminal is not
altered by the remote terminal … an instance of the host, mirrored. It should
have its own theme settings, but the rest of them will be connected to the main
host session."

Chosen approach (A): one session core and one Bubble Tea program per attached
terminal, all inside the host process. Each terminal is rendered at its own
size, with its own scroll position, input line and theme; the transcript,
agent, queue, roster, approvals and pickers are one shared thing. The mirror
lives inside the host process rather than as a separate OS process because
that keeps state sharing trivial and needs no new wire protocol; from the
outside it behaves as a mirror process would.

## Goals

- A terminal's size, layout, scroll position, selection, input line and theme
  never affect any other terminal.
- Everything that is true for the session — transcript, running state, queue,
  approvals, pickers, roster, stats — is one shared state, shown on every
  terminal in that terminal's format.
- A theme chosen on a device is remembered for that device.
- The client binary, the socket protocol's client side, `be-code attach`, the
  launcher and detach/switch behave as they do today.
- Plain mode is untouched. `--no-host` runs the same code with one view.

## Non-goals

- Rendering on the client (a state protocol over the socket). Rejected as
  approach B: needs a new versioned wire protocol and the same core split.
- Per-terminal transcripts or per-terminal agents. There is one conversation.
- Persisting scroll position, selection or drafts across detach.

---

## 1. Core and views

`internal/tui` splits into two types.

### `Session` (the core, one per host)

Owns everything that is true regardless of who is looking:

- the agent, config, provider, checkpointer, review coordinator, custom
  commands, the histories file;
- the **transcript** as a slice of entries:

  ```go
  type entryKind int // entryUser, entryAssistant, entryTool, entryDim, entryOK, entryWarn, entryErr, entryNotice
  type entry struct {
      Kind   entryKind
      Text   string // raw text; Markdown source for entryAssistant
      Sender string // label of the terminal that sent an entryUser, "" otherwise
  }
  ```

  plus the streaming buffer for the reply in progress and `lastReply`;
- `running`, the cancel function, the inbox/queue (`Agent.EnqueueFrom`,
  `Items`), stats, the idle timer, the transient notice (text and deadline);
- the roster (`[]live.ClientInfo`) and the live-registry hooks (`liveCodes`,
  `liveRecords`, `loadSession`, `switchClient`, `detachClient`);
- the shared ask (section 3);
- the agent goroutine and the approval seam (`Registry.Approve`,
  `Agent.Events`, `OnStatus`, `OnNotice`), exactly the callbacks the current
  `Model` installs in `New`.

It has no width, no styles, no viewport. Every change is published to all
attached views through one fan-out, `Session.broadcast(msg tea.Msg)`, which
calls each registered view's send function. Messages are the ones the current
model already uses, made explicit: `entryMsg{entry}`, `streamMsg{delta}`,
`streamEndMsg`, `runStateMsg{running bool}`, `statusMsg`, `noticeMsg`,
`clientsMsg`, `askMsg`, `askResolvedMsg`, `quitMsg`.

All mutation of core state happens on the core's own goroutine or under one
mutex (`Session.mu`); views never write core fields directly. A view asks the
core to do things through methods (`Session.Submit(text, from)`,
`Session.Enqueue`, `Session.Cancel`, `Session.Answer(gen, decision, from)`,
`Session.SlashShared(cmd, args, from)`).

### `View` (one per attached terminal; a `tea.Model`)

Owns what is local to a screen:

- client id and label; width and height; the layout decision (compact below
  70 columns or 20 rows, `config.Layout` override) from its **own** size;
- the viewport and scroll position; mouse selection; the rendered transcript
  buffer at its width (section 5);
- its textarea, history cursor over the shared history store, the `/` palette,
  `/menu`, the right-click context menu and the queue popup;
- its `styles` value (section 4) and `richText`;
- the mode (`modeInput`, `modeBusy`, `modeAsk`, `modePalette`, `modeMenu`,
  `modeContextMenu`, `modeQueue`), where `modeAsk` mirrors the core's shared
  ask and the rest are local.

`View.Update` handles its own client's keys and mouse (delivered as plain
`tea.KeyMsg`/`tea.MouseMsg`, since the program is per client) and the core's
broadcasts. `View.View` renders from its rendered buffer, its popup and the
core's status snapshot.

The current `Model` becomes `View` + `Session`. `Model.New` becomes
`NewSession(cfg, ag, prov)` and `session.NewView(id, label, theme)`.

---

## 2. Programs, input and output in the host

### Host changes (`internal/live`)

- `Host.ClientOutput(id int) io.Writer` — enqueues bytes on that one client's
  queue (`c.enqueue(FOutput, …)`). `Host.Output()` stays and keeps its
  broadcast semantics for the closing lines only.
- Per-client resize: the host stops computing a shared minimum. A client's
  `FResize` updates that client's `cols/rows` and the host calls
  `OnClientSize(id, cols, rows)`; `OnSize`, `Host.Size`, `AnyASCII` and the
  shared-size recompute are removed. `ClientInfo` keeps `Cols`, `Rows`, `UTF8`.
- Overlays are removed: `FOverlay`, `SetOverlay`, `ClearOverlays`, the
  `overlay` field and its re-append in `fanout.Write`.
- `OnInput` and the key pump are unchanged: every key or mouse event arrives
  tagged with its client id.
- `Host.Drop(id int, reason string)` — drops one client with a `bye` carrying
  `reason` (used by the runner for a view that panicked, section 6).
- `live.LabelKey(label string) string` — the label with its ` (pid N)` suffix
  stripped, the key `client_themes` is looked up by (section 4).

### Runner (`internal/tui/served.go`)

`(*Session).RunServed(h *live.Host)` replaces `(*Model).RunServed`:

- registers `OnClients`, `OnClientSize`, `OnInput` (through one `live.KeyPump`)
  and `OnQuit`, seeds the roster, then blocks until the session ends;
- on a **new** client id in the roster: builds a `View` for it (theme by label,
  section 4), starts
  `tea.NewProgram(view, tea.WithInput(nil), tea.WithOutput(h.ClientOutput(id)), tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithoutSignalHandler())`
  on its own goroutine, registers a **mailbox** for it (a buffered channel and a
  goroutine that calls `p.Send`, so a program mid-start or mid-teardown can
  never block the core's broadcast), sends it `tea.WindowSizeMsg` from the
  client's size, and sends it `attachedMsg` so it prints nothing extra —
  the core adds the `attached: <label>` entry once for everyone;
- routes `live.ClientKeyMsg{Client, Key}` to that client's mailbox as
  `tea.KeyMsg` and `live.ClientMouseMsg` as `tea.MouseMsg`; keys for an id
  with no program yet are buffered and replayed when it starts (the pump
  already buffers early input on the host side; the runner keeps that
  guarantee for the window between roster and program start);
- on a **dropped** id: sends that program `tea.Quit`, waits for its `Run` to
  return, unregisters the mailbox, and writes the terminal-colour reset to
  that client's writer if `theme_terminal_colors` is on. A `Switch` is a drop.
- session end (`/quit` from any view, `Host.RequestQuit`, the idle limit, the
  empty-after-switch rule): the core broadcasts `quitMsg`, the runner quits
  every program and waits for all of them, and only then returns so the host
  writes the closing lines. **Invariant: no program is running when the closing
  lines are written.** This replaces the `ClearOverlays` invariant.

`--no-host` (`cmd/root.go` in-process path): `(*Session).RunLocal()` builds one
`View` with id 0 and label `local`, and runs one program with real stdin/stdout
(`tea.WithAltScreen`, `tea.WithMouseCellMotion`, signal handling on). Plain
mode (`internal/ui/repl.go`) is unchanged.

The client (`internal/live/client.go`), `chord.go`, `cmd/live.go`
(`launchServed`, `attachLive`, `decideStart`, `--new`) and `be-code attach` are
unchanged, except that the client no longer receives or handles `FOverlay`.

---

## 3. Shared prompts and popups

### Shared asks (answered once for the session)

File-write and shell approvals, the plan approval, the model, provider and
session pickers, and the shared review prompt (`review.Terminal`).

The core holds at most one ask at a time:

```go
type ask struct {
    Gen     int
    Kind    askKind  // askApproval, askPlan, askPicker
    Title   string   // e.g. "file_write", "Implementation plan — approve to execute", "Resume session"
    Detail  string   // diff preview / plan text; "" for pickers
    Items   []pickItem // pickers
    reply   chan askAnswer
}
```

`Session.Ask(a) askAnswer` (called from the agent goroutine, the review
coordinator's terminal adapter, or a view's slash handler) stores the ask with
a fresh generation, broadcasts `askMsg{ask}`, and blocks on `reply`. Every view
enters `modeAsk` and renders the same modal at its own size (approval diff in
its viewport, picker list with its own cursor and filter — the cursor and
filter are view-local; the *answer* is shared).

`Session.Answer(gen, ans, from)` from a view: if `gen` is not the current
generation it is ignored (a stale key); otherwise the answer is delivered on
`reply`, the ask is cleared, the generation advances, and
`askResolvedMsg{gen, by: label}` is broadcast. Every other view closes its
modal and appends the dimmed `answered by <label>` line to its own rendered
buffer (not a transcript entry — it is a per-view notice). The answering view
just closes.

Withdrawal from the other side (the editor answered a shared review):
`Session.Cancel(gen)` clears the ask and broadcasts `askResolvedMsg{gen, by:
"VS Code"}`; the review coordinator's `Terminal.Cancel` calls it. The
`review.Terminal` adapter in `internal/tui/review.go` now calls `Session.Ask`
directly and no longer needs `approvalMsg` or `approvalCancelMsg`.

Esc on an ask is a *reject* from that terminal (as today), delivered through
`Answer`. Keys typed on a terminal while an ask is open but before its view has
rendered it are handled by the view's `modeAsk` handler once it is in; nothing
is routed to input.

### View-local popups

The `/` palette, `/menu`, the right-click context menu and the queue popup
belong to the view that opened them; the other views are unaffected and their
keyboards are never deadened. `handleGuestKey` and the owner fields
(`paletteOwner`, `menuOwner`, `pickerOwner`) are removed. The queue popup
shows only the opener's queued messages (`Items` filtered by sender), as today.

### Slash commands

Handled by the view that typed them. Commands that read or change shared
state go to the core: `/model`, `/models`, `/provider`, `/sessions`,
`/resume`, `/plan`, `/undo`, `/verify`, `/commit`, `/init`, `/compact`,
`/handoff`, `/map`, `/stats`, `/tools`, `/config`, `/review`, `/queue` (with
the caller's id), `/clear`, `/quit`. `/quit` ends the session for everyone.
View-local: `/theme`, `/copy`, `/clients`, `/detach`, `/help`, `/menu`.
`ui.BusySafeCommand` keeps its list and meaning; the "commands wait until the
agent is done" line is a per-view notice.

The transient yellow notice row (stall and compaction notices) is broadcast as
`noticeMsg` and drawn by every view for the same 20 s. `attached: <label>` and
`detached: <label>` are transcript entries added by the core.

---

## 4. Themes per device

### Config

```json
"theme": "dark",
"client_themes": { "ssh from 10.0.0.5": "nord", "vscode": "github-light" }
```

`config.Config.ClientThemes map[string]string` (default empty), documented in
the README config reference. The key is a client label with its ` (pid N)`
suffix stripped (`live.LabelKey(label)`); matching is exact on that key.

### Lookup on attach

`client_themes[LabelKey(label)]` → `theme` → `dark`. An unknown theme name at
any step falls through to the next and the view prints a dimmed warning line
to itself once: `theme "x" is not known; using <name>`.

### `/theme`

- `/theme` (no argument): prints, to the calling view only, the theme it is
  using and its origin — `nord (remembered for "ssh from 10.0.0.5")`,
  `dark (config default)` or `dark (built-in default)`.
- `/theme <name>` and the theme picker: apply to the calling view only
  (rebuild its rendered buffer, section 5), then save
  `client_themes[LabelKey(label)] = name` via `cfg.Save`. The picker marks the
  calling view's current theme.
- `/theme default <name>`: sets `theme` in config (the old behaviour), and
  applies it to the calling view. Other attached views are not changed.
- The theme picker lists the same rows as today, followed by a `default: <name>`
  row that opens nothing but shows what new devices will get.

### Styles

`theme.go`'s ten package-level `st*` variables become a `styles` struct:

```go
type styles struct{ Dim, Err, OK, Warn, Accent, Tool, User, Status, ModalTi, Border lipgloss.Style; name string }
func newStyles(theme string) (styles, bool) // false = unknown name (caller falls back)
```

Every render site takes `v.st.Dim.Render(...)` instead of `stDim.Render(...)`.
`internal/ui.RenderMarkdown` is unchanged. `pinColorProfile` stays global (it
concerns the process's colour capability, not a theme).

### Terminal colours

`theme_terminal_colors`: the OSC sequences for a view's theme are written to
that client's writer when its program starts, and the reset when it is
dropped. They never go to `h.Output()`.

---

## 5. Rendering the transcript per view

Each view keeps a rendered buffer: `[]string` of wrapped lines plus the
rendered form of the streaming reply. Rules:

- `entryMsg`: render that one entry with the view's styles at its width and
  append (`entryUser` → `v.st.User.Render(prefix) + text`, `entryAssistant` →
  `ui.RenderMarkdown(text, v.richText, v.width)`, `entryTool` → the tool style,
  `entryDim/OK/Warn/Err` → the matching style, `entryNotice` → the yellow row
  outside the buffer).
- `streamMsg`: append the delta to the view's streaming string and re-render
  only the streaming tail; `streamEndMsg` renders the finished reply as
  Markdown and moves it into the buffer, as `flushStreaming` does today.
- Resize or theme change: rebuild the whole buffer for **that view** from the
  core's entries (`Session.Entries()` returns a snapshot under the mutex).
- `ui.RenderMarkdown` renders without wrapping (it styles lines; it never
  breaks them), so it needs no width parameter: each view wraps its rendered
  buffer to its own width with the same lipgloss pass `refreshTranscript` uses
  today, which is what makes a phone wrap narrow while the desktop keeps wide
  tables.

Scroll position follows the existing rules per view: pinned to the bottom while
at the bottom, held once the reader scrolled up. Mouse selection
(`selection.go`) works on the view's wrapped lines; `highlighted()` re-renders
selected lines of that view only. `/copy` uses the caller's selection and
writes OSC 52 through the caller's client writer, and the system clipboard
tools as before.

The context wheel and the bottom line read a status snapshot
(`Session.Status()`: model, provider, running, spinner phase, tokens, roster
labels, review mode) and are drawn per view: the `⧉ N · labels` marker, the
spinner and the model name appear everywhere, and the compact layout is chosen
by each terminal's own size.

The header (`headerView`, session code, model) is per view too, shown only
where the height allows.

---

## 6. Lifecycle, failure and testing

### Lifecycle

- `OnClients` is the single source of attach and detach (section 2).
- Session end quits every program before the closing lines (section 2).
- `--no-host`: one view, real terminal.
- A view that panics in `Update` or `View` is recovered by that program's
  goroutine only: the panic is logged to the host log, the client is dropped
  with `bye{reason: "view error"}` (`Host.Drop(id, reason)`), and the session
  and the other views continue. Bubble Tea's own panic catcher is disabled
  (`tea.WithoutCatchPanics`) so the runner's recover sees it.
- A view whose mailbox is full (a program that stopped draining, which only
  happens during teardown) has broadcasts dropped, never blocked on.

### Removed

`internal/tui/inputs.go`; `overlayFor`, `blankInputRows`, `publishOverlay`,
`publishAllOverlays`, `clearAllOverlays`, `publishAfterKey`,
`publishVisibilityChange`, `overlayVisible`; `Host.SetOverlay`,
`Host.ClearOverlays`, the `FOverlay` frame and its client handling;
`Host.OnSize`, `Host.Size`, `Host.AnyASCII`, `recompute`/`recomputeAttach`'s
minimum; `approvalMsg` with its reply channel, `approvalCancelMsg`,
`handleGuestKey`, `paletteOwner`/`menuOwner`/`pickerOwner`; the global `st*`
styles.

### Testing

- `Session` unit tests, no Bubble Tea: entries and broadcast order; a shared
  ask — first answer wins, stale generation ignored, `askResolvedMsg` reaches
  the other views with the answering label; `Cancel` from the editor side;
  theme lookup by label (remembered, default, unknown → fallback with the
  warning); `Submit` from two senders tags entries with their labels.
- `View` tests drive `Update` directly, as the current model tests do, on two
  views of one session: different sizes give different wrapping and layout
  (one compact, one full) for the same entries; different themes give
  different styled output; a resize of one view leaves the other's buffer
  byte-identical; `/theme` on one saves `client_themes` for its label only;
  scroll-up on one view does not move the other; a picker opened by a shared
  ask shows on both, Enter on one closes both with `answered by` on the other.
- Served integration test: a real `live.Host` with three fake clients at
  40×15, 80×24 and 120×40; assert each client's byte stream contains frames
  laid out for its own size (the compact prompt on the small one, the header
  on the large one), that all three carry the same transcript text, that a
  resize frame from one client changes only that client's stream, and that
  a detach quits exactly one program.
- `docs/live-checklist.md`: items 3, 6 and 9 rewritten (each terminal shows
  its own input line natively; resizing the smallest terminal relayouts only
  itself; the phone gets compact while the desktop stays full), plus a new
  item: `/theme nord` on the phone recolours the phone only and survives
  reattach; `/theme default …` changes what a new device gets.

### Docs

README (shared sessions section: per-terminal rendering, `client_themes`,
`/theme default`), CHANGELOG 0.8.0, `build.mk` VERSION 0.8.0, root
`CLAUDE.md` "Shared (live) sessions" rewritten for core/view.

---

## Out of scope

- Rendering on the client; per-terminal transcripts; persisting scroll or
  drafts.
- Windows named pipes; the AF_UNIX choice stands.
- Changing plain mode.
