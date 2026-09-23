# BE-Code Redux (Be-Code)

**Offline-first agentic coding CLI for local LLMs.** Part of the BE-Continuum ecosystem.

[![version](https://img.shields.io/badge/version-1.1.1-blue)](CHANGELOG.md)
[![Go](https://img.shields.io/badge/Go-1.25%2B-00ADD8)](go.mod)
[![license](https://img.shields.io/badge/license-MIT-green)](LICENSE)
[![platforms](https://img.shields.io/badge/platforms-linux%20%C2%B7%20macOS%20%C2%B7%20windows-lightgrey)](#install--uninstall)

BE-Code combines three lineages: the BE-CLI Go architecture and conventions, the Claude Code
tool-loop model, and the Codex headless/pipeline style — rebuilt around one premise that neither
of those tools has: **the model is small, local, and fallible, so the harness must supply the
quality.** Every design decision follows from that.

It talks to whatever inference you already run — Ollama, llama.cpp, LM Studio, vLLM, a BE AI
Engine fabric — and nothing else: no telemetry, no account, no network call but the endpoints
you configured (and web search, which is opt-in and off by default). A session survives the
terminal that started it, several terminals can watch and drive the same run, and the record of
what the model has read and decided lives in your project as Markdown you can edit by hand.

## Contents

**Start here** — [Requirements](#requirements) · [Install](#install--uninstall) ·
[Quick start](#quick-start) · [The two interfaces](#the-two-interfaces) · [Backends](#backends)

**Day to day** — [Approvals](#approvals-and-diff-previews) · [The screen](#the-screen) ·
[Themes](#themes) · [Select and copy](#select-and-copy) ·
[Talking to the agent mid-run](#talking-to-the-agent-while-it-works) ·
[Checkpoints & undo](#checkpoints--undo) · [Plan mode, git, hooks](#plan-mode-git-custom-commands-hooks)

**More than one model** — [Co-working models](#co-working-models) · [Sub-agents](#sub-agents) ·
[Reviewer routing](#reviewer-routing-multi-model) · [Model profiles](#model-profiles) ·
[Context window](#context-window) · [Benchmarking](#benchmarking-your-fleet)

**Memory** — [Sessions](#sessions) · [Shared sessions](#shared-sessions) ·
[Chat and DMs](#chat-and-dms) · [Repo map, @mentions, compaction](#context-repo-map-mentions-compaction) ·
[Working memory](#working-memory) · [Task record](#task-record) · [Project memory](#project-memory)

**Integrations** — [VS Code](#vs-code) · [Visual Studio](#visual-studio) ·
[MCP servers](#mcp-servers) · [Web search](#web-search-optional-google-programmable-search) ·
[Scripting](#scripting)

**Reference** — [Commands](#command-reference) · [Config](#config-reference) · [Safety model](#safety-model) ·
[Shell safety](#shell-safety--background-processes) · [Layout](#layout) ·
[Development](#development) · [Documentation](#documentation) · [Status](#status)

## How it wins with weaker models

1. **Verification is the quality multiplier.** After the agent believes it is done, BE-Code
   detects the project type (Go, Node, Python, Rust, Makefile) and runs the real toolchain —
   vet/build/test. Failures are condensed and fed back as bounded repair turns
   (`max_repairs`). The model doesn't have to be right the first time; it has to converge.
2. **Small-model-friendly tool calling.** Seven flat tools, forgiving argument parsing
   (double-encoded JSON, alternate key names), and a *compat mode* that embeds tool calls in
   plain text (`<tool_call>{...}</tool_call>`) for models whose native tool-call support is
   broken. `auto` mode tries native and falls back per session.
3. **Aggressive context budgeting.** A `context_tokens` budget with compress-to-target trimming: old tool
   outputs collapse first, then old turns drop (the original task always survives). Small
   contexts degrade before they overflow; BE-Code trims before that point.
4. **One failure at a time.** Verification stops at the first failing check — small models
   repair best with a single root cause in front of them.
5. **A record the conversation cannot lose.** What the model has read, run and decided is
   written to a task tree — Markdown documents in your own project — as the work happens, and
   put back in front of the model every turn. When a compaction summary fails or comes back
   empty, the session continues from that record instead of from a trimmed transcript.
6. **The local prompt cache is part of the design.** A local server can reuse its cache only
   when a prompt strictly extends the last one, so BE-Code never changes or takes back what it
   has sent, and sends the next prompt ahead after anything that empties the cache. Measured on
   a LAN server: about 6 s a turn instead of 11–41 s.

## Requirements

- **A backend.** Any OpenAI-compatible server on your machine or LAN: Ollama (best supported —
  native `/api`, a window you set rather than discover), llama.cpp, LM Studio, vLLM, or a BE AI
  Engine fabric. No cloud account is needed anywhere.
- **A model that can call tools.** Anything in the 7B–32B class with tool calling works; models
  whose native tool calling is broken are carried by compat mode (see below). Qwen3-family
  models are what BE-Code is tuned and measured against.
- **To build from source: Go 1.25 or newer** (`go.mod` sets the floor). `install.sh` will accept
  an older toolchain, which then has to fetch 1.25 itself — on a machine with no network,
  install Go 1.25 or use a prebuilt binary instead.
- **To run a prebuilt binary: nothing.** One static Go binary, no runtime, no dependencies.
- **Platforms:** linux/amd64, linux/arm64, darwin/arm64, windows/amd64. `git` is optional
  (its absence only costs the git-backed lookups and `/commit`); `make`/`sh` are only needed
  for the build.

## Install / uninstall

One line, no checkout — it reads the current version from the repository, downloads that
release's binary for your platform, installs shell completions and offers the setup wizard:

```bash
curl -fsSL https://raw.githubusercontent.com/BE-AI-Research/be-code-redux/main/install.sh | sh
```

`--system` and `PREFIX=` work the same way (`| sh -s -- --system`). `BE_CODE_VERSION=1.1.0`
installs a specific release, `BE_CODE_REF=<branch|tag>` reads the version from somewhere other
than `main`, and `BE_CODE_REPO=` points the whole thing at a fork or a mirror. When a release
has no binary for your platform the installer fetches the source and builds it, which needs Go
1.25+. Uninstall the same way:

```bash
curl -fsSL https://raw.githubusercontent.com/BE-AI-Research/be-code-redux/main/uninstall.sh | sh
```

Or download a release binary yourself, make it executable and put it on your PATH:

```bash
chmod +x be-code-linux-amd64 && mv be-code-linux-amd64 ~/.local/bin/be-code
be-code --version && be-code setup     # the setup wizard writes your first config
```

From a checkout (builds or picks up `dist/`, installs shell completions, offers the wizard):

```bash
./install.sh              # user install to ~/.local/bin (builds from source when
                          # a Go toolchain is present, else uses a dist/ or bin/ prebuilt
                          # that reports this tree's build.mk VERSION — a stale one is
                          # refused, never installed under the new version's name);
                          # installs shell completions, offers the setup wizard
./install.sh --system     # system-wide to /usr/local/bin
PREFIX=/opt/be ./install.sh   # custom prefix
./uninstall.sh            # remove binary + completions; KEEPS ~/.be-code
./uninstall.sh --purge    # also delete ~/.be-code (config, sessions, history)
```

Windows: `.\install.cmd` / `.\uninstall.cmd [-Purge]` (installs to
`%LOCALAPPDATA%\Programs\be-code` and manages the user PATH; no administrator rights
needed). The `.cmd` files are one-line launchers for `install.ps1` / `uninstall.ps1`:
Windows refuses to run unsigned PowerShell scripts by default ("running scripts is
disabled on this system"), and blocks ones unpacked from a downloaded zip even where
local scripts are allowed, so the launchers run them as
`powershell -NoProfile -ExecutionPolicy Bypass -File "<script>"` — a bypass for that one
process, which changes nothing about the machine's policy. Run that command yourself if
you prefer; arguments (`-NoSetup`, `-Purge`) pass straight through. Both installers are
safe to re-run — they upgrade in place.

## Quick start

```bash
make -f build.mk build    # there is no Makefile; every target needs -f build.mk
./be-code                 # full-screen TUI in the current directory
./be-code --plain         # inline REPL (best over SSH / BE-CLI web terminals)
./be-code doctor          # check configured backends + workspace toolchain
./be-code init            # measure the workspace and write BECODE.md project notes
./be-code run "add unit tests for pkg/utils"   # headless, verifies before exiting
```

Building the release artifacts yourself: `make -f build.mk verify` runs vet, build and the
whole test suite, and `make -f build.mk release` cross-compiles all four platforms into `dist/`
and packages the VS Code extension beside them.

**First launch** runs a setup wizard: it probes local backends concurrently
(Ollama :11434, llama.cpp :8080, LM Studio :1234, vLLM :8000, BE AI Engine :9800),
lists the models on whichever answer, and writes your config from two keypresses.
Re-run any time with `be-code setup`.

## The two interfaces

**TUI (default on a terminal)** — alt-screen app: streaming transcript, styled tool
activity (● running / ✓ done / ✗ error), status bar with provider · model · context-use %,
multi-line input (Enter sends, Ctrl+J newline), Tab slash-command completion, ↑↓ input
history (shared with plain mode), arrow-key pickers for `/model`, `/provider`, and
`/sessions` (type to filter), and scrollable approval modals.

**Plain REPL (`--plain`, or `"ui": "plain"`)** — same core over simple line output:
readline editing, persistent history, Tab completion. Survives dumb terminals, `screen`,
and BE-CLI web sessions; also auto-selected when stdin/stdout isn't a TTY.

## Approvals and diff previews

Before the agent touches a file you see a colored unified diff (new files show as
all-adds) — `y` approve, `n` deny (the model is told and adapts), `a` stop asking this
session. Shell commands get the same gate; compound commands (chains, pipes, redirects,
substitutions) are never auto-approved by an allow glob and always prompt. `-y` auto-approves both for headless runs;
non-interactive runs without `-y` deny writes safely.

## The screen

On terminals with 30 or more rows a branded header sits at the top; below that the
transcript, then the input row with a `(>):` prompt and the **context wheel** at its
right: ○ ◔ ◑ ◕ ● shows how full the context is, and while the model works the wheel
rotates (◴ ◵ ◶ ◷) with the percentage beside it. One bottom line shows `/menu /help`,
the model, and the state. Type `/` on an empty input to open the **command palette**:
keep typing to filter, ↑↓ to pick, Enter runs it (or fills the input for commands that
take an argument), Tab fills, Esc keeps what you typed. `/menu` opens a full-screen
grouped menu with a status block (provider, model, profile, window, context usage,
session total).

## Themes

Thirteen palettes: `dark` (default), `light`, `mono`, `dracula`, `nord`, `gruvbox`,
`monokai`, `one-dark`, `solarized-dark`, `solarized-light`, `tokyo-night`, `catppuccin`
and `github-light`. `/theme` opens this terminal's theme picker, whose title reports the current theme and where it came from;
`/theme nord` applies one for this terminal only, at once, remembered for this device
in `client_themes`; `/theme default nord` instead sets `theme` — what a new device
starts from. Pick from a list via `/menu` → Settings → Theme. The ten named themes
also recolour the terminal window itself (background, and foreground for light
themes) on terminals that support it, restored when BE-Code exits; set
`theme_terminal_colors` to false to keep your terminal's own background. Plain mode
uses your terminal's own colours, so there `/theme` only records the choice for the TUI.

## Co-working models

A co-working model is a second model — usually stronger, local or online —
the primary can consult mid-task without handing over the work. Configure
one or more under `coworkers`:

```json
"coworkers": [
  {"name": "big", "provider": "ollama", "model": "qwen3:32b", "skills": "architecture, tricky bugs"}
],
"cowork": {"auto": true, "max_consults_per_run": 3, "consult_turns": 12, "consult_timeout": 300}
```

`provider` names an entry under `providers{}`, same as `default_provider`. An
invalid co-worker (empty name/model, unknown provider, duplicate name) is
dropped with a `warn:` line at startup rather than failing the run.

The primary calls the `consult` tool itself when it is stuck. With
`cowork.auto` on, the harness also consults the first configured co-worker on
its own: once when verification is still failing after the last repair round
(the advice earns one extra repair round), and once when the same tool has
failed three times in a row (the advice is delivered as a note before the
next model call). Either way, a consultation is a read-only scratch agent —
`read_file`, `list_dir`, `search` only, no writes or shell — running on the
co-worker's own provider/model with its own turn budget
(`cowork.consult_turns`), seeded with the question, the named files (at most
8 of them, 8 KiB each and 32 KiB in all — the rest are listed by name for it
to read itself), and the primary's recent context. At most `cowork.max_consults_per_run`
consultations run per request, one at a time, and each is bounded by
`cowork.consult_timeout` seconds, after which it is abandoned with
`co-worker <name> timed out after <n>s` and the run carries on. A co-worker
that fails, declines or is unreachable never interrupts the run — it is
silent or leaves a `co-worker unavailable: …` notice.

Answers show up on every attached terminal as `<name>? question` then
`<name>> answer`, in the theme's co-worker colour, followed by a short line
of how many files it read and how long it took; the status line shows
`consulting <name> · N files read` while one is in flight. Plain mode prints
the same lines.

A co-worker marked `"online": true` — or whose provider's address is off this machine and
this network, which is treated as online with a startup warning whatever the entry says —
asks for consent once per session before
any code is sent to it — the TUI's `Co-working model — approval required`
modal (`a` allows it for the rest of the session), or the same prompt in
plain mode; `-y` allows it, and a headless run without `-y` declines with
`consultation declined`. Local co-workers, and a person's own `/consult`,
never ask.

`/coworkers` lists the configured co-workers and how many times each has been
consulted this session. `/consult [name] <question>` asks one directly —
busy-safe; asked mid-run it runs on the session's own root context rather
than the turn's, so the run is left alone and Esc (or `/quit`) cancels both
the run and the consultation. A `/consult` never counts against
`cowork.max_consults_per_run`.

## Sub-agents

`"sub_agent": true` on a `coworkers` entry lets that model own a step of the
task tree instead of only being consulted: assigned a step, it gets a scoped,
write-capable scratch agent of its own — `ask_main` to park on a question for
the primary, and its own `task` tool pinned to the subtree — seeded with the
step's text, the project notes and the primary's recent context, the same as
a consultation.

Assign a step with `/task assign <id> <name>` (`/task assign <id> main` gives
it back), or write the owner straight into the task document:

```
- [ ] 3.2. port internal/scan  @big  scope: internal/scan, internal/scan_test.go
- [ ] 3.3. write the migration note  @claude!  scope: docs/scanner.md  after: 3.2
```

`@name` assigns; `@name!` is pinned by the operator, and the model will not
reassign it. A step with an owner and no scope is not ready — give it one
with `/task scope <id> <path>[, <path>…]`. A sub-agent stuck on a decision
only the primary can make asks with `ask_main`, which parks the step until
`/task reply <id> <text>` answers it (or the primary does, when it is free);
a write outside the assigned scope is refused with a note, not silently
dropped.

`/agents` lists every sub-agent card and what it is doing — idle, waiting on
a scope or a dependency, working, or asking; `/agents stop <name>` cancels
whatever it is running and closes the step `blocked` with the reason
`stopped by operator`, handing it back so the main model knows. It is
`/task assign <id> main` that takes a step away without closing it, putting
it back to `todo`. An interrupted step (`/quit`, or a crash) is picked back
up on the next session — told which files its earlier attempt had already
written, from the interruption note on a clean exit, and recovered from the
step's own evidence after a crash, which never got to write one — or handed
back blocked and noticed when nothing configured can pick it up, exactly as
before.

That resume no longer starts working the moment the session opens. Once
whatever cannot come back has blocked and handed back, if anything else is
still assigned and ready, BE-Code asks once, through the same modal a tool
approval uses, naming every pending step, its owner, where it runs and its
scope:

```
resume sub-agent work from the previous session?
  3.2 port internal/scan — big (ollama-lan/qwen3:32b), scope: internal/scan
  4.1 write the migration note — claude (online), scope: docs
nothing has been sent to any model yet
```

Yes dispatches them; no leaves them assigned and `todo` and prints `sub-agent
work left dormant; /agents start runs it` — that verb dispatches whatever is
waiting without asking again. The decline holds back *everything* assigned,
not just the steps that happened to be ready when it was asked, so a second
step sequenced behind one of them does not quietly start once the first
closes. A step assigned or reassigned mid-session, by you or by the main
model, is never held back this way — as long as it is a genuine change; the
model restating a plan it already made does not undo your decline — because
only the one question at startup ever asks. `-y` (headless) skips the
question and dispatches; a non-interactive run without `-y` declines the
same way (under `run --json`, that decline's notice does not appear in the
JSON output — only the approver's own denial line on stderr does).

Approvals follow the session's own mode: a write inside a sub-agent's scope
raises the same modal or diff a primary write does, labelled `sub-agent
<name> (<id>):` so it is never mistaken for the primary's own edit. A
sub-agent marked `"online": true` asks consent once per session before any
code reaches it, the same as an ordinary co-worker, except the detail names
the step's scope rather than only the model.

A sub-agent runs on its own per-server lane, the primary's requests always
first: on its own server it works alongside the primary freely, but a
sub-agent sharing the primary's server interleaves with it, and each
hand-over between them costs the primary a cold prompt read — a local
server's prompt cache survives only a strictly extending prompt, and a
hand-over is never that. A second server does not.

`sub_agents.max_concurrent`, `sub_agents.max_turns`, `sub_agents.ask_timeout`
and `coworkers[].max_scope` are in the config reference below. The full
owner/scope syntax is in [`docs/task-format.md`](docs/task-format.md).

## Select and copy

Drag with the mouse over the transcript to select text (Shift-drag uses your
terminal's own selection instead). Ctrl+C copies the selection, Esc clears it, and a
right-click opens a popup with Copy selection, Copy last reply, Copy last tool output,
Select all and Paste into input. From the keyboard, `/copy [selection|reply|tool|all]`
does the same. Copy uses OSC 52 (works in VS Code's terminal, most terminals and over
SSH) and, when present, wl-copy/xclip/xsel/pbcopy/clip; paste into the input reads the
system clipboard through those tools, while normal terminal paste keeps working.

## Talking to the agent while it works

You do not have to wait for a run to finish. Type while the agent is working and
press Enter: the message is shown as *queued* and delivered to the model before
its next call, after the tool results it was waiting on, tagged as a message that
arrived mid-task. Use it to add a requirement, redirect, or ask a question.
Anything still queued when the run ends starts the next turn. Esc (TUI) or Ctrl-C
(plain mode) cancels the run and discards the queue. Slash commands wait until the
agent is idle, except `/queue`.

Changed your mind? Press Up on an empty input (or Ctrl+Q) while the agent works to
open the queue: ↑↓ to pick, Enter to pull a message into the input for editing (it is
paused until you press Enter again), `d` to drop it, Esc to close. Delivery pauses
while the popup is open. In plain mode use `/queue`, `/queue edit N` and
`/queue drop N`.

## Web search (optional, Google Programmable Search)

BE-Code is offline-first; web access is the one opt-in exception. With it, the agent
gets `web_search` (numbered results with title, URL, snippet) and `web_fetch` (page
text with HTML stripped), which help a local model with library and API facts it
does not know.

1. Create a search engine at https://programmablesearchengine.google.com (search the
   whole web or a list of doc sites) and copy its **Search engine ID** (`cx`).
2. In Google Cloud, enable the **Custom Search JSON API** and create an API key.
3. Export the key and set the engine ID in `~/.be-code/config.json`:

```bash
export GOOGLE_PSE_API_KEY=...        # the key never goes in the config file
```

```json
"web_search": { "provider": "google", "cx": "a1b2c3d4e5f6g7h8i", "api_key_env": "GOOGLE_PSE_API_KEY", "max_results": 5, "allow_fetch": true }
```

`be-code doctor` shows whether search is on and the key is present. The free tier is
100 queries/day. Set `allow_fetch` to false to allow searching without page fetches.

## Sessions

Every conversation auto-saves to `~/.be-code/sessions` after each turn and gets a
6-character resume code. On exit BE-Code prints `resume: be-code --resume <code>`
after writing a **handoff briefing** with the model (task, requirements you stated,
decisions, files changed, current state, next steps). Resuming injects that briefing
into the system prompt so the next session keeps your requirements and decisions
even after history is trimmed; `/handoff` shows it. `be-code sessions` lists codes;
resume with `--resume <code>`, `--resume <id>`, `--resume last`, `/resume <code>`, or
the `/sessions` picker; `be-code sessions delete <code>` removes one. A session
being served right now is marked `live`; resuming it by any route joins the
running session rather than loading a second copy of its file (see "Shared
sessions"). Headless `run` prints the resume line to stderr and writes a
quick heuristic briefing (no extra model call). `/clear` starts a fresh session.

Resuming a session replays its saved transcript into the terminal first — what
you typed, the replies, the tool calls and their first result line, and any
compaction summary as a dim block — then a `— resumed here —` divider, so you
pick up where you left off instead of facing a blank prompt. `resume_replay:
false` turns that off; `resume_replay_turns: N` keeps only the last N requests.

## Shared sessions

Starting `be-code` on a terminal does not run the session in that terminal: it
starts a **detached host process** and attaches to it. The host owns the agent,
the tools, the MCP servers and the editor bridge; your terminal is a thin pipe
that paints what the host renders and forwards your keystrokes. So the session
outlives the terminal — close the VS Code window, lose the SSH link, shut the
laptop lid on a train — and you pick it up exactly where it was, from anywhere
you can reach the machine:

```bash
be-code                       # joins this workspace's live session, or starts one
be-code --new                 # start a fresh session even though one is live here
be-code sessions              # the LIVE column marks sessions being served right now
be-code --resume A1B2C3       # live? joins it. not live? resumes the saved file
be-code attach A1B2C3         # attach another terminal to the same session
be-code attach last           # attach to the most recently started live session
ssh workstation be-code attach A1B2C3    # ...from your phone, over SSH
be-code attach A1B2C3 --view  # watch only: this terminal never sends input
be-code sessions kill A1B2C3  # end a live session from outside it
```

**One live instance per session code.** A session that is running somewhere is
never loaded a second time — every way in joins it instead of forking it, and no
path prompts you first:

- `be-code` in a workspace that already has a live session joins the newest one,
  printing `joining live session <code> (be-code --new starts a fresh one)`.
  `--new` is the only way to get a second session on the same files.
- `be-code --resume <code>` prints `joining live session <code>` and attaches,
  whether you named it by code, by id or as `last`.
- The session picker (`/menu` → "Resume a saved session", `/sessions`, `/resume
  <code>`) marks a running session `LIVE`; picking that row moves *your* terminal
  into it, leaving the other terminals on your old session where they were. If
  the session you left was fresh and nobody else is attached to it, it exits
  rather than linger as an empty host.
- Without a host (`--no-host`, `host_sessions: false`) and in plain mode there
  is nothing to join in-process, so `/resume` of a running code says
  `<code> is live elsewhere; join it with: be-code attach <code>` and loads
  nothing.

The session file is guarded as well: it carries the pid of the host that owns
it, and a second program that finds a *live* owner stops autosaving rather than
overwrite that host's turns (`session file is owned by live host <pid>; autosave
disabled for this session`). On exit that run is written out under a fresh code
instead — `saved as a new session: be-code --resume <code>`.

**Every terminal renders itself.** The host runs one program per attached
terminal, each at its own size and layout (a phone gets the compact layout
while the desktop keeps its header and full layout), with its own scroll
position, selection and input line: your half-written message stays on your
screen and nobody else's, and the palette you opened with `/`, the `/menu` you
opened, the right-click menu, your command history and your queued-message
popup all belong to the terminal that opened them. Press Enter and the message
goes into the one shared transcript, prefixed with the terminal that sent it
(`local (pid 4321)> …`) whenever more than one terminal is attached — with a
single terminal the prefix is the usual `you> `. The transcript, the agent, the
message queue and the roster live on one shared session underneath; each
terminal's program just renders its own view of it, at its own size and in its
own theme (see "Themes" for `/theme`'s per-device semantics).

**Shared prompts, answered once.** Approvals, the plan prompt and the
model/provider/session pickers appear on every attached terminal at once.
Whichever terminal answers first decides for the whole session — the verdict
lands on the shared transcript, so everyone sees what was decided — and the
rest close their own copy with the dimmed note `answered by <label>` (or
`answered in VS Code` when the editor answered it instead). Esc from any of
them closes it the same way. A terminal whose renderer fails is disconnected
(`view error`) without taking the session or any other terminal down with it.

The bottom line shows `⧉ 2` (`# 2` on non-UTF-8 terminals) followed by the
attached terminals' labels, and `/clients` lists them with their sizes. Chords
are typed in the attached terminal rather than sent to the session:

| Chord | Does |
| --- | --- |
| `Ctrl+] d` | detach this terminal; the session keeps running |
| `Ctrl+] Ctrl+]` | the same detach, without reaching for `d` |
| `Ctrl+]` then anything else | sends the literal `Ctrl+]` on to the session (so does `Ctrl+]` on its own, a second later) |

`/detach` does the same as `Ctrl+] d` for the terminal that types it, and
`/quit` ends the session for everybody: every attached terminal prints the
`resume:` line and drops back to its shell.

**Each terminal keeps its own size.** Attach a phone alongside a desktop and
only the phone relayouts to its own size — the desktop's view is untouched.
Below 70 columns or 20 rows a terminal's own program switches to its **compact
layout**: no header, a one-line status, a bare `>` prompt, popups without
descriptions; a resize above that threshold brings the header and full layout
straight back. `layout: compact`/`full` forces it either way, per terminal. A
phone SSH app is therefore a usable second head on a session without shrinking
anyone else's view.

Knobs: `--no-host` (or `host_sessions: false`) keeps the session in the launching
process the old way — nothing to attach to, and `/clients` says so. Plain mode
(`--plain`, `ui: plain`), non-TTY runs and headless `be-code run` are never
hosted. `live_idle_limit` (minutes) exits a served session that has been sitting
with no attached terminals and no run in progress, so a forgotten host does not
hold a model resident forever.

Records live in `~/.be-code/live/<code>.json` (0600, with the socket's auth token)
next to the host's own socket and its startup log `<code>.log` — the place to look
if a session never comes up. Attaching is local-only by design: the socket is a
unix socket in your own dotdir, so remote access means SSH, not a network port.

### Chat and DMs

Every terminal attached to a session shares a chat room: `/chat` opens it on your terminal
(the session keeps running underneath; `Esc` or `/back` returns), Enter posts, and `@agent`
in a line hands it — with the last `chat.mention_context` lines of the room — to the
session's model, whose answer appears in the transcript and in the room. DMs span every
session on the machine: `/dm <name>` opens a thread, `/inbox` lists them newest first
(`●` unread), and a message to someone not attached anywhere waits for them. Your name
comes from `chat.name` in your own config, else the names already seen from your address,
else you are asked once (`/whoami` says which). Messages live under `~/.be-code/inbox/`;
identity is advisory, not security — anyone with a shell on the host can read them. Plain
mode and headless runs have neither.

## Checkpoints & undo

Every agent turn snapshots files before they're touched. `/undo` rolls back the last
turn's edits (multi-level, LIFO — created files are removed, modified files restored).
Shell side effects aren't snapshotted; that's what git awareness is for.

## Context: repo map, @mentions, compaction

A symbol-level repository outline (`/map` to view) is injected into the system prompt so
small models spend turns editing, not exploring. `@path/to/file` in any message pins that
file's content into context (Tab completes @paths in the TUI). When the conversation
exceeds its budget, BE-Code has the model summarize older turns (`/compact` to force it)
instead of dropping them. If that summary call fails or comes back empty, the session
continues from the task record under `Working memory:` — which the harness wrote as the
work happened, without a model call — and says so: `compaction: the model returned no
summary; continuing from the task record`. Plain trimming is the fallback only when
there is no record to continue from, and a compaction you cancel still stops rather
than rewriting the transcript you were keeping.

## Working memory

BE-Code keeps a per-workspace record of what the model has already read, looked up
and decided — a task tree whose documents live in your project (see **Task record**
below) with its disposable half under `~/.be-code/engine/<key>/` — and puts it back
in the system prompt as a `Working memory:` block (after the repository map) on every
request, including after compaction and on resume, so a long task stops re-reading the
same files and a compaction summary no longer has to restate what it already knows.
The block is composed fresh every turn: Task Reports for finished work oldest first,
then the active branch down to the step marked `doing`, then that step's tool calls
and output *verbatim*, then the durable notes. The active branch and its verbatim step
are never what gets cut when `engine.budget` bites — finished reports condense first,
oldest first, down to a `task show <id>` pointer.

- **Reads.** Every `read_file` is recorded against the step that made it: an outline
  of the symbols it touched, the line ranges seen, a content hash, and (once a
  compaction summary or a `task note
  file:` mentions it) a short note of what mattered. Asking for lines already covered
  by an unchanged file is still answered in full, with the footer `already read at
  turn N (unchanged); outline and notes are in your context` — the tool result is
  never withheld, only flagged as redundant.
- **Lookups.** A repeated `search` or `lookup` whose hit files are unchanged is
  answered from a cache instead of re-running, with the footer `(cached; files
  unchanged)`.
- **The `task` tool.** Five verbs over one task tree, whose nodes are addressed by
  dotted paths like `2.1.3`: `action: plan` records a task and its steps in one call;
  `add` creates a node (`parent:` its id, omitted for a new top-level task) and
  returns its id; `status` marks a node `doing`, `done`, `blocked` or `dropped` (with
  `reason:` for the last two); `note` records a fact or decision against a node
  (`file:` ties it to a file so it need not be re-read; `keep: true` also remembers it
  in the durable notes, which survive across sessions); `show` prints a node, a branch
  or the whole tree. What the model does while a node is `doing` is recorded against
  that node, and work done before it named one is adopted by the next node it marks
  `doing`. The tree and the notes are always shown under `Working memory:`. `/task`
  prints the tree, `/task show <id>` one branch, `/task open` the path to the
  documents, and `/task clear` closes every still-open node as `dropped` with the
  reason `cleared` — it never deletes a document; `/notes` prints the durable notes,
  `/notes add <text>` appends one, `/notes drop N` removes one, `/notes clear` empties
  them. Both are busy-safe in either UI, and in a shared session every attached
  terminal reads and writes the same store.
- **Git-backed lookups**, read-only and confined to the workspace, never prompting:
  `lookup` (git grep over tracked files, plus untracked ones; `symbol: true` returns
  the whole enclosing function instead of just the matching line); `history` (a
  function's own log, a line range's log, a pickaxe search, or blame); `show` (a file
  as it stood at any revision); `changes` (everything altered since the task began,
  including files that were already dirty when it started). Outside a git repository
  `lookup` falls back to the ordinary walk-based search and the rest say so plainly.

`engine.tools: minimal` registers only `task` and `lookup` (dropping `history`, `show`
and `changes`) for backends where a smaller embedded tool catalog matters more than
the extra lookups. A headless `be-code run` and a live host on the same workspace share
the store directory without a lock: the host's in-memory state is authoritative and is
rewritten at its next flush, and `notes.md` is never cleared by that.

## Task record

The tree is not a database: it is Markdown in your project, one document per
top-level task, under `<project>/.be-code/tasks/`.

```markdown
# 001 — fix the parser

- [x] 1. fix the parser
  - [x] 1.1. find the bug
    - files: lexer.go (lines 1–120)
    - cmds: go test ./... — failed
    - error: FAIL: TestLex
  - [>] 1.2. fix and verify
  - [-] 1.3. rewrite the scanner — dropped: not needed after all
```

Five marks, one per step: `[ ]` todo, `[>]` doing, `[x]` done, `[!]` blocked,
`[-]` dropped — the last two carrying why, after an em dash. Ids are dotted paths
that follow a step's position (`2.1.3`), evidence lines sit one level deeper than
the step that earned them, and everything the engine does not recognise — your own
prose, a checklist you keep in the same file — is preserved exactly and comes back
untouched. Edit these files by hand whenever you like: a hand-edited status is your
intent and is never overridden (two `[>]` marks get a warning naming the documents,
and neither document is changed). A document the engine cannot parse is renamed
`NNN-<slug>.broken-<stamp>.md` rather than half-read, and nothing here is ever
deleted — not by a retitled task, and not by `/task clear`, which closes open steps
as `dropped` with the reason `cleared` and leaves the documents alone.

`/task` prints the tree, `/task show <id>` one branch, `/task open` the folder. In a
git repository the first document written adds `.be-code/` to your `.gitignore`, so
the record is **untracked by default**; delete that one line to commit it. The full
specification is [`docs/task-format.md`](docs/task-format.md), and a short version is
written into `.be-code/tasks/README.md` for whoever opens the folder next.

## Context window

On an Ollama backend BE-Code talks to `/api/chat` natively, so the context window is
something you set rather than something you discover:

```jsonc
"providers": {
  "lan": { "type": "ollama", "base_url": "http://192.168.1.150:11434",
           "context_window": 32768, "keep_alive": "30m",
           "options": { "temperature": 0.6 } }
},
"models": {
  "hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL": { "context_window": 32768 }
},
"reload_on_mismatch": "ask"
```

Resolution for one model is: the `models` entry, else the provider block, else a
probe of the backend. **A configured `context_window` means no probe** — no
`/api/ps` read, no Modelfile parse, and above all no loading the model to find out,
which on a large local model is minutes of startup for a number you already know.
`context_tokens` is then derived from the window (minus generation headroom) unless
you set it, in which case your number still wins and BE-Code says at startup how
much of the window that leaves unused.

**Changing the window of a model that is already loaded makes Ollama reload it, which
evicts every other user of that server.** So it is a change to a shared service, not
a setting of ours: `reload_on_mismatch: "ask"` (the default) puts it through the same
approval prompt as a shell command — `y` reloads, `n` keeps the loaded window and
clamps the budget to it, `a` saves `reload_on_mismatch: always` to your config.
`"always"` is that standing consent for this machine's server; `"never"` runs inside
whatever window the server already has, every time. Each model is asked about once a
session, whichever way it was answered, and a non-interactive or headless run never
prompts — it keeps the loaded window, clamps, and prints why. A question nobody
answered before the session gave up on it is not remembered as a refusal; the next
resolution asks again. When another client reloads the model underneath a
running session, BE-Code adapts to *their* window rather than reloading it back: a
reload war between two clients on one server is the worst outcome available.

Switching model (`/model`, or a pick from `/models`) resolves the new model's
parameters off the UI thread, so the command returns at once and the window arrives
as a notice; window, budget and reserve follow the model, and if the new window is
smaller than the conversation already occupies BE-Code compacts once on the spot
rather than letting the next request truncate. Nothing outside the loader loads a
model or sends `num_ctx`. A server too old to serve `/api/chat` (a 404 whose body is
not Ollama's own JSON error, or a 405 or 501) falls back to the OpenAI-compatible
endpoint for the rest of the session, with one notice — that path cannot carry a
window at all.

## Model profiles

The model name selects a family profile (qwen3, deepseek-r1, gemma, llama, codellama,
phi, …) that sets the tool-calling mode automatically — e.g. Gemma-family models get
embedded tool calls, reasoning models (qwen3, deepseek-r1, gpt-oss) get their
`<think>` blocks stripped from streams and transcripts. `/model` switches re-apply
profiles live; the status bar shows the active family.

## Plan mode, git, custom commands, hooks

`/plan <task>` runs a read-only planning phase (inspect-only tools), shows the numbered
plan for approval, then executes it. `/commit` writes a model-generated commit message
and commits; git branch/status is refreshed into the prompt each turn when the workspace
is a repo. `/init` measures the repository and writes `BECODE.md` (see "Project memory").
Custom slash commands are markdown prompt templates in `.becode/commands/*.md` (workspace) or
`~/.be-code/commands/*.md` (global), with `$ARGS` substitution. Hooks
(`hooks.post_write`, `hooks.pre_shell` in config) run your commands around tool actions
— e.g. `"post_write": ["gofmt -w $FILE"]`.

## MCP servers

Attach any Model Context Protocol tool server (stdio transport) via `mcp_servers` in the
config; its tools appear to the agent as `mcp_<server>_<tool>`:

```json
"mcp_servers": { "mcpql": { "command": "be-mcpql", "args": ["--serve"] } }
```

## VS Code

Build the extension with `make -f build.mk vscode` (or as part of `make -f build.mk
release`), which type-checks, runs its tests and packages
`dist/be-code-vscode-<version>.vsix`. Install it from
the Extensions view → `...` → **Install from VSIX...**. To run it from source instead —
for development, or to try changes before packaging — `cd vscode && npm install && npm
run build`, then use VS Code's "Run Extension" launch configuration to open an Extension
Development Host with it loaded.

Either way, run `be-code` from the editor's integrated terminal. When BE-Code detects
it's running inside VS Code (or `--ide` is passed) it connects to the extension's editor
bridge and gains `ide_*` tools — diagnostics, symbols/definitions/references/hover, and
the debugger — plus a one-line note about the file and selection you're looking at, and
in-editor diff review before a write lands on disk. Run `BE-Code: Open terminal` from the
command palette to get a terminal already wired up. Shell command approvals still happen
in the terminal, not the editor.

Controlled by `ide.enabled` and `ide.auto_context` in config (both on by default) and the
flags `--ide` (connect even outside an editor terminal, and even when `ide.enabled` is
false) and `--no-ide` (never connect, which wins over everything else). Headless `be-code
run` never touches the editor unless `--ide` is passed explicitly, so scripted runs stay
reproducible; even then it only attaches the `ide_*` tools — no editor diff review and no
per-turn `[editor: …]` context note.
Where a change is reviewed is `ide.review`: `auto` (the default) shows the diff in the
editor while VS Code's own terminal is the only one attached, and raises the terminal
approval prompt *as well* once another terminal joins the session, so whoever is at a
phone or an SSH session can answer too — the first answer from either place wins and the
other is withdrawn (the modal closes with `answered in VS Code`, the editor diff closes
itself). `editor` always reviews in the editor (falling back to the terminal only when the
bridge cannot), `tui` only ever asks in the terminal, and `both` always does both. Sharing
needs a live editor on the other side: with no bridge attached, every mode falls back to the
ordinary write approval, which still honours `-y` and `approve_file_writes`. `/review`
prints the current mode, `/review <mode>` changes it for the session.

`be-code doctor` reports whether an editor bridge is listening. See `vscode/README.md` for
the extension's own commands/settings and `docs/vscode-live-checklist.md` for a manual
end-to-end checklist.

## Visual Studio

`visualstudio/` is the same bridge for Visual Studio 2022 (17.6 or later) and 2026: one
`.vsix`, built on Windows with `visualstudio\build.ps1`, giving BE-Code the same `ide_*`
tools — context, the Error List, definition/references/hover for C# and VB, the debugger —
and a diff of each proposed write in Visual Studio's own difference viewer with Accept,
Accept all and Reject. Visual Studio's terminals do not announce themselves the way VS
Code's do, so there is nothing to pass: run `be-code` in any terminal under the solution
and it attaches to the running Visual Studio whose solution covers that directory.
`--ide`, `--no-ide`, `ide.enabled` and `ide.review` mean what they mean for VS Code; from
a separate terminal `ide.review: auto` resolves to `both`, so the terminal prompt and the
editor diff are both live and the first answer wins.

**As of 0.12.0 the Visual Studio layer compiles against the real SDK but has never been
run**; `visualstudio/README.md` says which parts are proven and what this version does not
do, and `visualstudio/WINDOWS-CHECKLIST.md` is how it gets proven.

## Shell safety & background processes

Commands are matched against `shell_deny` (never runs, never prompts) and `shell_allow`
(runs without a prompt) glob lists; everything else asks. Defaults allow common
build/test commands and deny the destructive classics. The `process` tool manages
background processes (dev servers, watchers): start/list/logs/stop, with bounded log
rings, cleaned up at session end.

## Reviewer routing (multi-model)

Set `reviewer.model` (and optionally `reviewer.provider`) plus `"review_on_done": true`
and a second — typically larger — model reviews the changed files after verification
passes, with one automatic repair round for the issues it raises. Small model drafts,
big model reviews: a natural fit for a BE AI Engine fleet.

## Benchmarking your fleet

`be-code bench` runs an embedded offline eval suite (compile fixes, function
implementation, logic bugs, precision edits) through the real agent against your live
backend and scores each model: `be-code bench --models qwen3:8b,llama3.1:8b --json`.

## Scripting

`be-code run --json "task"` emits a machine-readable result (answer, verification and
review status, changed files, token/latency stats) for driving BE-Code from other
Continuum components or CI.

## Backends

`~/.be-code/config.json` ships with the local stack pre-wired:

| provider       | endpoint                      | notes                          |
|----------------|-------------------------------|--------------------------------|
| `ollama`       | `http://localhost:11434/v1`   | default; native pull/ps/warm   |
| `llamacpp`     | `http://localhost:8080/v1`    | llama.cpp server               |
| `lmstudio`     | `http://localhost:1234/v1`    | LM Studio                      |
| `be-ai-engine` | `http://localhost:9800/v1`    | BE AI Engine fabric (set `BE_AI_ENGINE_KEY`; point `base_url` at your fabric head node) |

Anything OpenAI-compatible works (vLLM included) — add an entry to `providers`.

```bash
./be-code -p be-ai-engine -m qwen3:8b        # pick provider/model per run
./be-code pull qwen3:8b                       # Ollama backends only
./be-code verify                              # run project checks, exit non-zero on failure
echo "fix the failing test" | ./be-code run -y   # pipeline mode (auto-approve shell)
```

## Project memory

Drop a `BECODE.md` in the workspace root (falls back to `CLAUDE.md`) — build commands,
conventions, gotchas. It is injected into the system prompt (up to 8 KiB) as *facts about
your project for orientation*: notes describe the repository, they are never read as
instructions or tasks.

**`/init` writes it for you, from measured facts.** `/init` (both UIs) and headless
`be-code init [-y] [-C dir]` first *measure* the workspace — languages by extension, the
project kind and its exact check commands, key files (README, Makefile, `go.mod`,
`package.json`, …), top- and second-level directories with file counts, entry points,
test directories, formatter/linter configs, git branch/remote/recent commits, the README's
first lines, any `AGENTS.md`, `CLAUDE.md` or `GEMINI.md` at the root (instructions written
for other coding agents: up to 12 KB of each, handed over as facts about what the authors
want an agent to know, never as instructions) and the repo map — then ask the model, with no tools, to write the overview
*from those facts only*: what the project is, how to build, test and run it using the
measured commands exactly, the layout, the conventions and the gotchas. The scan is
bounded (20,000 files, 2 s; a partial scan says so).

The reply is validated before anything is written: it must not describe BE-Code or its
tools, it must cite at least two measured files, directories or commands, every command
it shows in backticks or a fenced block must be one that was actually measured, and it
must be at most 150 lines. "Measured" covers what a project really documents, not just the
verification checks: every `make` target in `Makefile`/`build.mk` (as `make <target>` and
`make -f <file> <target>`), every `package.json` script (`npm run <script>`, plus
`npm test`/`npm install` and `npx <bin>`), `go run .`, `go run ./cmd/<x>`,
`go test ./<pkg>` and `go vet ./...` in a Go module, `cargo build|test|run|check`, and
`pytest` in a Python project with tests — each accepted with flags after it
(`go test ./... -race`) but not with a different target (`go generate ./...`). A rejected draft gets one retry with the reasons; if that
fails too, the measured fact sheet itself is written, headed by a comment saying the
model's overview was rejected and why.

Then it is a normal write: you see the diff preview and approve it, a restore point is
recorded, the file is written, and the new notes go into the system prompt at once. In a
git repository the restore point is a branch `be-code/pre-init/<yyyymmdd-hhmmss>` holding
the whole working tree as it was — tracked and untracked files alike, honouring
`.gitignore` — created without touching your HEAD, index or checkout. The lines after the
write name it and the command that rolls everything back:

```
restore point: be-code/pre-init/20260913-141200
  roll back with: git restore --source=be-code/pre-init/20260913-141200 --staged --worktree -- .
```

Every init adds a new branch and none is ever moved, so the earliest one is the permanent
initial restore point (prune them with `git branch -D` when you no longer need them). If
the snapshot cannot be taken, init stops before writing rather than writing without one.
Outside a git repository any previous `BECODE.md` is kept as `BECODE.md.bak` instead. That approval is always the terminal's own prompt — `/init` asks here even when
`ide.review` is `editor`, because the document it wrote is what you are being shown. Re-run `/init` whenever the project has moved on. A session started in a recognized
project that has no notes file says so once — `no BECODE.md; /init maps this project` —
and does nothing else. `be-code init` on a non-interactive stdin without `-y` denies the
write instead of writing unattended.

## Safety model

- All file tools are confined to the workspace root (path-traversal hardened, per BE-CLI lessons).
- Every shell command requires interactive approval (`y`/`N`/`a`lways) unless `-y` /
  `auto_approve_shell` is set.
- No telemetry, no network calls except to your configured inference endpoints. Fully offline.

## Layout

```
cmd/                 cobra commands (root, run, init, bench, models, pull, doctor, verify, config, sessions, attach, setup)
internal/provider/   OpenAI-compatible client (SSE + tool calls), Ollama native /api/chat
internal/loader/     the only code that loads a model or sends num_ctx; reload consent
internal/agent/      loop, prompts, embedded-call parsing, budgeting/compaction, plan mode,
                     think-filtering, @mentions, stats, co-working, sub-agent runner, autosave
internal/tools/      file/search/shell/process tools, write approvals, allow/deny, hooks,
                     scoped registries for sub-agents, MCP adapter
internal/engine/     the task tree: documents in your project, working-memory block, evidence
internal/subagent/   sub-agent records, scope arithmetic, readiness rule, per-server lanes
internal/verify/     project detection, check runners, repair summaries
internal/diff/       pure-Go unified diff for change previews
internal/checkpoint/ turn-level snapshots for /undo
internal/repomap/    symbol-level workspace outline
internal/gitctx/     git awareness (+/commit)
internal/profiles/   model-family tuning table
internal/mcp/        stdio MCP client (JSON-RPC 2.0)
internal/ide/        editor bridge discovery and MCP-over-TCP client (VS Code, Visual Studio)
internal/review/     where a file change is reviewed (editor, terminal or both)
internal/live/       live-session host, attach client, records (~/.be-code/live)
internal/inbox/      machine-wide DMs (~/.be-code/inbox), no UI, no host
internal/discover/   workspace measurement for /init (languages, commands, layout, git)
internal/commands/   custom slash commands (.becode/commands)
internal/bench/      embedded offline eval suite
internal/store/      session persistence (~/.be-code/sessions)
internal/setup/      backend probe + first-run wizard
internal/config/     ~/.be-code/config.json
internal/procattr/   hidden child processes (Windows needs this; a test enforces it)
internal/ui/         plain REPL (readline), markdown/chroma rendering, shared helpers
internal/tui/        full-screen Bubble Tea UI (transcript, modals, pickers, themes)
vscode/              the VS Code extension (TypeScript, its own project)
visualstudio/        the Visual Studio bridge and package (C#, its own solution)
```

## Command reference

**Commands** (`be-code <command>`; with none, the interactive session starts):

| Command | Does |
| --- | --- |
| `run [prompt]` | headless run, verification included; `--json` for a machine-readable result; reads stdin when no prompt is given |
| `init` | measure the workspace and write `BECODE.md` project notes |
| `setup` | the first-run wizard: probe backends, list models, write the config |
| `doctor` | probe every configured backend, the workspace toolchain and the editor bridge |
| `verify` | run the workspace's own build/lint/test checks, non-zero exit on failure |
| `models` / `pull <model>` | list the backend's models / pull one (Ollama backends) |
| `sessions` | list saved sessions (`LIVE` marks one being served); `sessions delete <code>`, `sessions kill <code>` |
| `attach <code\|last>` | attach another terminal to a live session; `--view` for watch-only |
| `bench` | run the embedded offline eval suite against your models |
| `config` | print the effective configuration |

**Flags** (persistent, so they work on any command): `-p/--provider`, `-m/--model`,
`-C/--dir <workspace>`, `-y/--yes` (auto-approve for headless use), `--resume <code|id|last>`,
`--new`, `--plain`, `--no-host`, `--ide` / `--no-ide`.

**Slash commands** (both UIs; `/` on an empty input opens the palette, `/menu` the grouped menu):

| | |
| --- | --- |
| **Session** | `/sessions` `/resume <code>` `/handoff` `/clear` `/quit` `/detach` `/clients` `/stats` `/config` |
| **Models** | `/model <name>` `/models` `/provider <name>` `/coworkers` `/consult [name] <q>` `/agents [stop <name>\|start]` |
| **Work** | `/plan <task>` `/verify` `/commit` `/undo` `/compact` `/init` `/map` `/tools` `/queue [edit N\|drop N]` |
| **Record** | `/task [show <id>\|open\|clear\|assign <id> <owner>\|scope <id> <paths>\|reply <id> <text>]` `/notes [add <text>\|drop N\|clear]` |
| **People** | `/chat` `/inbox` `/dm [name]` `/whoami` `/back` |
| **This terminal** | `/theme [<name>\|default <name>]` `/review [auto\|editor\|tui\|both]` `/copy [reply\|tool\|all]` `/help` `/menu` |

Your own commands are markdown prompt templates in `.becode/commands/*.md` (workspace) or
`~/.be-code/commands/*.md` (global), with `$ARGS` substitution — they appear in the palette
beside the built-ins.

## Config reference

Everything lives in `~/.be-code/config.json`, written by `be-code setup` and safe to edit by
hand; `/config` prints what the running session actually resolved.

- `default_provider`, `model`, `providers{}` — backend selection. A provider
  block also carries the parameters every model on that endpoint runs with:
  `providers.<name>.context_window` (the `num_ctx` BE-Code sends on Ollama's
  native path), `providers.<name>.keep_alive` (overrides the top-level
  `keep_alive` for this endpoint) and `providers.<name>.options` ({}), a map
  passed through to Ollama's options block untouched, so config can reach keys
  the harness knows nothing about (`top_k`, `top_p`, `repeat_penalty`...).
  Ignored for `type: openai`, which has no such knob.
- `models{}` ({}) — the same three keys per model, keyed by the model name
  exactly as the backend spells it (tag included), e.g.
  `"models": {"qwen3:8b": {"context_window": 32768, "keep_alive": "30m",
  "options": {"top_k": 40}}}`. Parameters belong to the model, so these win
  over the provider block; the `options` maps are merged key by key rather
  than replaced. Resolution order for one model is: this map, else the
  provider block, else a probe of the backend. **A configured
  `context_window` means no probe at all** — no `/api/ps` read, no Modelfile
  parse, and above all no loading the model to find out, which on a large
  local model is minutes of startup for a number you already know.
- An absent `num_ctx` is not neutral: on Ollama it means the server's default
  window applies, which reloads a model loaded at any other size. So BE-Code
  sends the window the server already holds wherever it knows it, including
  for the reviewer and co-worker models, and drops runner-level keys
  (`num_ctx`, `num_batch`, `num_gpu`, `use_mmap`, …) from `options` with a
  notice unless `reload_on_mismatch` is `always`. Use `context_window`, not
  `options.num_ctx`, to set a window.
- `reload_on_mismatch` (`ask`) — `ask` | `always` | `never`. Sending a
  `num_ctx` that differs from how a model is currently loaded makes Ollama
  **reload it, evicting whatever else on that machine was using it**. That is
  a change to a shared service, so `ask` (the default) puts it through the
  same approval prompt as a shell command, under the action `model_reload`;
  `a` at that prompt sets this key to `always`. `never` runs inside whatever
  window the server already has. A headless or non-interactive run never
  prompts: it keeps the loaded window, clamps its budget and says why.
  When another client reloads the model underneath a running session,
  BE-Code adapts to *their* window rather than reloading it back — a reload
  war between two clients on a shared box is the worst outcome available.
- `temperature` (0.2), `max_tokens`, `context_tokens` (unset) — generation/budget.
  `context_tokens` is the total prompt budget (system prompt, tools schema and
  history). **Leave it out** and it is simply the window the session gets — the
  configured `context_window` when there is one, otherwise what the backend
  reports (Ollama: `/api/ps`, Modelfile `num_ctx`). Set, it is a cap and still
  wins, so existing config files keep behaving — but a cap below the window
  means the rest of the window goes unused, and BE-Code now says so at startup
  instead of shrinking the budget in silence. Generation headroom is reserved
  from it: `max_tokens` if set, else a quarter of the window (1k–4k) for plain
  models or a third (4k–16k) for reasoning models, which think before they
  answer. The
  chars-per-token estimate recalibrates from server-reported usage each request.
- `keep_alive` ("30m") — how long Ollama keeps the model resident after each request
  (refreshed after every prompt; "0" disables). Sharing the server with other
  clients can still evict the model; BE-Code then reports the eviction, retries
  failed calls in place with the task state intact, and re-checks the context
  window before every call.
- `max_turns` (24) — tool-loop iterations per request
- `max_repairs` (3) — verification repair attempts
- `compat_tool_calls` — `auto` | `always` | `never` (profiles refine `auto` per family)
- `verify_on_done` (true), `auto_approve_shell` (false), `approve_file_writes` (true)
- `ui` — `tui` | `plain`; `theme` — `dark` | `light` | `mono`
- `client_themes` ({}): theme per device, keyed by the attached terminal's label
  without its pid (`"ssh from 10.0.0.5": "nord"`). Written by `/theme <name>` in a
  shared session; `/theme default <name>` sets `theme` instead.
- `layout` — `auto` (default) | `compact` | `full`; `auto` switches the TUI to a
  reduced layout (no header, short prompt, one-line status, popups without
  descriptions) below 70 columns or 20 rows. Each attached terminal is judged by
  its own size, so one can be compact while another keeps the full layout;
  `compact`/`full` force it on or off
- `shell_allow` / `shell_deny` — command glob lists; `hooks` — post_write / pre_shell
- `repo_map` (true) + `repo_map_budget`; `compact_with_model` (true)
  Since 0.11.1 this is a ceiling: the map is built to at most a fifth of the usable
  context and rebuilt when the window changes, with a notice when the budget was cut.
- `mcp_servers` — stdio MCP tool servers; `reviewer` + `review_on_done` — second-model review
- `ide.enabled` (true), `ide.auto_context` (true), `ide.review` (`auto`) — the editor
  bridge; see "VS Code". With `ide.enabled` on, BE-Code auto-attaches in `TERM_PROGRAM=vscode`
  terminals as before, and also (without needing `--ide`) to a running Visual Studio whose
  advertised workspace covers the current one — a covering VS Code lock never auto-attaches
  outside its own terminal.
- `chat.enabled` (true) — `/chat`, `/inbox` and `/dm`; `chat.mention_context` (10) — room lines
  sent with an `@agent` mention; `chat.name` — this device's user ID for chat and DMs (unset:
  the host offers the IDs seen from your IP, or asks once).
- `live_idle_limit` (0) — minutes a served session may sit with no attached clients
  and no run in progress before it exits (0 = never)
- `stall_notice_seconds` (45) — seconds of backend silence before the yellow "waiting for
  backend" notice appears above the input line (a second notice follows at four times
  this); notices of this kind show for 20 s and are not kept in the transcript.
- `host_sessions` (true) — run each interactive TUI session in a detached host
  process this terminal attaches to, so it survives the terminal and other
  terminals can attach (`--no-host` for one run); see "Shared sessions"
- `coworkers` (`[]`) — `{name, provider, model, skills, online}` entries naming
  other models the primary can consult; `cowork.auto` (true), `cowork.
  max_consults_per_run` (3), `cowork.consult_turns` (12) and
  `cowork.consult_timeout` (300, seconds one consultation may take) tune when
  and how much; see "Co-working models"
- `coworkers[].sub_agent` (false) — the co-worker may own a step of the task tree
  (see "Sub-agents"); `coworkers[].max_scope` (`[]`, whole workspace) — the widest
  set of workspace paths it may ever be given as a scope. Entries that escape the
  workspace are dropped with a warning, and if that leaves none at all the co-worker
  loses `sub_agent` too: an empty `max_scope` means the whole workspace, so a
  `max_scope` of `["/docs"]` must not quietly become one
- `sub_agents.max_concurrent` (2) — sub-agents running at once across every server;
  `sub_agents.max_turns` (40) — turns one sub-agent gets on its step;
  `sub_agents.ask_timeout` (600, seconds) — how long an `ask_main` waits for an
  answer before the step is marked blocked
- `engine.enabled` (true) — the working-memory store, its `task` tool and the git
  lookups; `engine.budget` (6144) — byte cap on the `Working memory:` system-prompt
  block; `engine.notes_cap` (4096) — byte cap on the durable `notes.md`;
  `engine.item_cap` (4096) — byte cap on one recorded tool result;
  `engine.node_cap` (32768) — byte cap on one task node's verbatim buffer, oldest
  results dropped (and counted) past it;
  `engine.step_nudge` (20) — once the current step has taken more tool calls than this,
  Working memory tells the model to finish it, split it or note why (negative turns it off);
  `engine.tools` — `full` (default) | `minimal` (`task` and `lookup` only); see
  "Working memory"
- `reasoning_effort` (`medium`) — the thinking budget asked of a reasoning model: `low`, `medium` or `high` (empty leaves the backend's default, which for Qwen3.x GGUF templates is the highest). The tool loop adapts it per call: one level down once the prompt fills more than half the window, and `low` for the rest of a request after reasoning has exhausted the window. On a 32k window with a 27B thinking model, `low` is the setting that keeps long runs moving.
- `prompt_layout` (`cached`) — `cached` never changes or takes back anything it has sent: the system prompt is stable for the length of a request, the git summary and Working memory are attached to your message when a request begins and stay in the history, and a fresh snapshot is attached only after a compaction or trim. A local server's prompt cache then covers everything but the new text (measured: about 6 s a turn against 11–41 s). `classic` re-sends Working memory in the system prompt every turn, as every version before 0.14 did.
- `prompt_prefill` (true) — after anything that empties the server's prompt cache (a model load or reload, a resume, `/compact`), the prompt the next turn will send is sent ahead with generation off, in the background, and cancelled the moment you press Enter. Native Ollama only. `/stats` is the session's metrics screen: context in use against the usable limit and the fixed prompt, the window and reserve, compactions, requests and tokens (hidden reasoning included), the server's prompt-reading time and cache misses, tool calls by tool, repair rounds, attached terminals and the task tree's counts.
- `time_awareness` (true) — gives the model a clock: every tool result ends with `[14:32:07 · took 3.2s · step 3.2 open 14m · context 61%]` and each of your messages with when it was sent. Appended text only, never the system prompt, so the server's prompt cache is untouched; about fifteen tokens a tool call.
- `resume_replay` (true) replays the saved transcript when a session is resumed; `resume_replay_turns` (0 = all) caps it to the last N requests.

## Development

```bash
make -f build.mk verify      # vet + build + the whole test suite — run before claiming done
go test ./...                # the unit suites on their own (about a second)
go test ./... -race          # what CI-quality work runs
sh test/e2e/run_e2e.sh       # end-to-end against a scripted mock backend on :18111
make -f build.mk release     # the four platform binaries plus the VS Code extension
make -f build.mk vscode      # the extension alone (npm install, tests, package)
make -f build.mk visualstudio-test   # the C# bridge and its tests (needs the .NET SDK)
```

`go vet` is the only linter. The `verify` package's tests invoke the real Go toolchain in
temp directories and the `mcp` tests spawn a subprocess, so both need `go` and `sh` on PATH;
`verify` itself never needs the .NET SDK. `go run . doctor` probes your configured backends
and the workspace toolchain, which is usually the fastest way to find out why a run failed.

## Documentation

| | |
| --- | --- |
| [`CHANGELOG.md`](CHANGELOG.md) | every release, what changed and why |
| [`docs/task-format.md`](docs/task-format.md) | the task-document format in full: marks, ids, evidence lines, owner and scope syntax |
| [`docs/BE-SPEC-CODE-2026-001.md`](docs/BE-SPEC-CODE-2026-001.md) | the design specification |
| [`docs/superpowers/specs/`](docs/superpowers/specs) | per-feature design documents (sub-agents, the Visual Studio bridge, …) |
| [`vscode/README.md`](vscode/README.md) | the VS Code extension's own commands and settings |
| [`visualstudio/README.md`](visualstudio/README.md) | the Visual Studio bridge: what is proven and what is not |
| [`docs/live-checklist.md`](docs/live-checklist.md), [`docs/vscode-live-checklist.md`](docs/vscode-live-checklist.md) | manual end-to-end checklists against a real backend |

## Status

**v1.1.1 (current)** — a sub-agent no longer resumes behind your back: if a previous session
left work assigned, BE-Code asks once at startup, names every pending step with its owner and
scope, and dispatches nothing until you answer. `/agents start` runs what a decline left
dormant.

Recent releases:

- **v1.1.0** — sub-agents: a co-worker marked `sub_agent: true` can own a step of the task tree
  — a scoped, write-capable scratch agent, `ask_main`, per-server lanes with the primary first,
  and approvals on the main model's own schema.
- **v1.0.1** — co-working hardened: `online` corroborated against the provider's address, the
  advice sanitizer extended to Qwen's XML tool calls, a co-worker panic contained, and a
  co-worker budgeted against its own window. **v1.0.0** — 0.15.0 renumbered, nothing changed.
- **v0.15.0** — a per-session chat room with `@agent`, and machine-wide DMs.
- **v0.14.0** — a prompt layout the server's prefix cache survives, background prompt
  processing after a model load, and the server's prompt-reading time in `/stats`.
- **v0.13.0** — pacing guidance, a nudge for a step open too long, a repeat detector, and a
  clock for the model.
- **v0.12.x** — the Visual Studio 2022/2026 extension beside the VS Code one, an editor review
  that can be withdrawn without wedging the bridge, and the model-reload fixes a shared LAN
  server taught us.
- **v0.11.x** — context handling: a task record that survives compaction, native Ollama with a
  configurable window, a model loader gated on consent, a compaction target that can be reached.
- **v0.3.0** — the pro-grade pass: checkpoints/undo, repo map and `@mentions`, model profiles and
  think-filtering, model compaction, plan mode, git awareness, custom commands and hooks, the MCP
  client, reviewer routing, shell allow/deny, background processes, the bench suite, JSON output,
  themes. Before it: v0.2.0 (dual UI, wizard, diff approvals, sessions), v0.1.0 (the core loop
  and verification).

Built for four platforms from one Go module. 25 of the 28 Go packages carry tests, the two
editor extensions have their own suites, and a scripted-model end-to-end run (repair loop, MCP
attach, undo, JSON mode, bench harness, task record) drives the real binary.

## License

MIT — see [`LICENSE`](LICENSE). Copyright (c) 2026 BE AI Research.
