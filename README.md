# BE-Code

**Offline-first agentic coding CLI for local LLMs.** Part of the BE-Continuum ecosystem.

BE-Code combines three lineages: the BE-CLI Go architecture and conventions, the Claude Code
tool-loop model, and the Codex headless/pipeline style — rebuilt around one premise that neither
of those tools has: **the model is small, local, and fallible, so the harness must supply the
quality.** Every design decision follows from that.

## How it wins with weaker models

1. **Verification is the quality multiplier.** After the agent believes it is done, BE-Code
   detects the project type (Go, Node, Python, Rust, Makefile) and runs the real toolchain —
   vet/build/test. Failures are condensed and fed back as bounded repair turns
   (`max_repairs`). The model doesn't have to be right the first time; it has to converge.
2. **Small-model-friendly tool calling.** Six flat tools, forgiving argument parsing
   (double-encoded JSON, alternate key names), and a *compat mode* that embeds tool calls in
   plain text (`<tool_call>{...}</tool_call>`) for models whose native tool-call support is
   broken. `auto` mode tries native and falls back per session.
3. **Aggressive context budgeting.** A `context_tokens` budget with compress-to-target trimming: old tool
   outputs collapse first, then old turns drop (the original task always survives). Small
   contexts degrade before they overflow; BE-Code trims before that point.
4. **One failure at a time.** Verification stops at the first failing check — small models
   repair best with a single root cause in front of them.

## Install / uninstall

```bash
./install.sh              # user install to ~/.local/bin (builds from source when
                          # Go >= 1.22 is present, else uses a dist/ or bin/ prebuilt
                          # that reports this tree's build.mk VERSION — a stale one is
                          # refused, never installed under the new version's name);
                          # installs shell completions, offers the setup wizard
./install.sh --system     # system-wide to /usr/local/bin
PREFIX=/opt/be ./install.sh   # custom prefix
./uninstall.sh            # remove binary + completions; KEEPS ~/.be-code
./uninstall.sh --purge    # also delete ~/.be-code (config, sessions, history)
```

Windows: `.\install.ps1` / `.\uninstall.ps1 [-Purge]` (installs to
`%LOCALAPPDATA%\Programs\be-code` and manages the user PATH). Both installers are
safe to re-run — they upgrade in place.

## Quick start

```bash
make build                # or: make -f build.mk build
./be-code                 # full-screen TUI in the current directory
./be-code --plain         # inline REPL (best over SSH / BE-CLI web terminals)
./be-code doctor          # check configured backends + workspace toolchain
./be-code init            # measure the workspace and write BECODE.md project notes
./be-code run "add unit tests for pkg/utils"   # headless, verifies before exiting
```

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

A co-worker marked `"online": true` asks for consent once per session before
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
and decided, under `~/.be-code/engine/<key>/`, and puts it back in the system prompt
as a `Working memory:` block (after the repository map) on every request, including
after compaction and on resume — so a long task stops re-reading the same files and a
compaction summary no longer has to restate what it already knows.

- **Reads.** Every `read_file` is digested: an outline of the symbols it touched, the
  line ranges seen, a content hash, and (once a compaction summary or a `task note
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
  prints the ledger, `/task
  clear` resets the session's working memory; `/notes` prints the durable notes,
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
`dist/be-code-<version>.vsix`. Install it from
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
first lines and the repo map — then ask the model, with no tools, to write the overview
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
internal/provider/   OpenAI-compatible client (SSE + tool calls), Ollama native mgmt
internal/agent/      loop, prompts, embedded-call parsing, budgeting/compaction, plan mode,
                     think-filtering, @mentions, stats, reviewer routing, autosave
internal/tools/      file/search/shell/process tools, write approvals, allow/deny, hooks, MCP adapter
internal/verify/     project detection, check runners, repair summaries
internal/diff/       pure-Go unified diff for change previews
internal/checkpoint/ turn-level snapshots for /undo
internal/repomap/    symbol-level workspace outline
internal/gitctx/     git awareness (+/commit)
internal/profiles/   model-family tuning table
internal/mcp/        stdio MCP client (JSON-RPC 2.0)
internal/live/       live-session host, attach client, records (~/.be-code/live)
internal/discover/   workspace measurement for /init (languages, commands, layout, git)
internal/review/     where a file change is reviewed (editor, terminal or both)
internal/commands/   custom slash commands (.becode/commands)
internal/bench/      embedded offline eval suite
internal/store/      session persistence (~/.be-code/sessions)
internal/setup/      backend probe + first-run wizard
internal/config/     ~/.be-code/config.json
internal/ui/         plain REPL (readline), markdown/chroma rendering, shared helpers
internal/tui/        full-screen Bubble Tea UI (transcript, modals, pickers, themes)
```

## Config reference (`~/.be-code/config.json`)

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
- `temperature` (0.2), `max_tokens`, `context_tokens` (16384) — generation/budget.
  `context_tokens` is the total prompt budget (system prompt, tools schema and
  history) and is clamped to the window the session actually gets — the
  configured `context_window` when there is one, otherwise what the backend
  reports (Ollama: `/api/ps`, Modelfile `num_ctx`). Left at 0 it is simply the
  window; set, it still wins, so existing configs keep behaving. Generation headroom is reserved
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
- `mcp_servers` — stdio MCP tool servers; `reviewer` + `review_on_done` — second-model review
- `ide.enabled` (true), `ide.auto_context` (true), `ide.review` (`auto`) — the VS Code
  editor bridge; see "VS Code"
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
- `engine.enabled` (true) — the working-memory store, its `task` tool and the git
  lookups; `engine.budget` (6144) — byte cap on the `Working memory:` system-prompt
  block; `engine.notes_cap` (4096) — byte cap on the durable `notes.md`;
  `engine.item_cap` (4096) — byte cap on one recorded tool result;
  `engine.node_cap` (32768) — byte cap on one task node's verbatim buffer, oldest
  results dropped (and counted) past it;
  `engine.tools` — `full` (default) | `minimal` (`task` and `lookup` only); see
- `reasoning_effort` (`medium`) — the thinking budget asked of a reasoning model: `low`, `medium` or `high` (empty leaves the backend's default, which for Qwen3.x GGUF templates is the highest). The tool loop adapts it per call: one level down once the prompt fills more than half the window, and `low` for the rest of a request after reasoning has exhausted the window. On a 32k window with a 27B thinking model, `low` is the setting that keeps long runs moving.
- `resume_replay` (true) replays the saved transcript when a session is resumed; `resume_replay_turns` (0 = all) caps it to the last N requests.
  "Working memory"

## Status

v0.3.0 — the pro-grade pass: checkpoints/undo, repo map + @mentions, model profiles +
think-filtering, model compaction, plan mode, git awareness + /commit + /init, custom
commands + hooks, MCP client, reviewer routing, shell allow/deny + background processes,
bench suite, JSON output, markdown/syntax highlighting, themes, usage stats. Earlier:
v0.2.0 (dual UI, wizard, diff approvals, sessions), v0.1.0 (core loop + verification).
4-platform builds; 15 tested packages plus scripted-model e2e (repair loop, MCP attach,
undo, JSON mode, bench harness) driven through the real binary.
