# BE-Code Changelog

## v0.5.0 — 2026-09-11 — live sessions & handoff

- **Sessions now outlive their terminal.** Starting `be-code` on a terminal
  starts a detached **session host** process and attaches to it; the host owns
  the agent, tools, MCP servers and editor bridge, and your terminal is a thin
  pipe. Close the terminal (or lose the SSH link) and the session keeps
  running.
- `be-code attach <code|last>` attaches another terminal — locally, from the
  VS Code terminal, or over SSH from a phone — to a running session;
  `--view` attaches read-only. Any number of terminals can watch one session;
  the newest to attach holds input, the others are live viewers.
- Chords in an attached terminal: `Ctrl+] d` detach, `Ctrl+] t` take input
  back, `Ctrl+] Ctrl+]` send a literal `Ctrl+]`. From inside the session,
  `/detach` detaches this terminal and `/clients` lists every attached
  terminal with its size; the bottom line shows `⧉ <n>` and who holds input.
- `be-code sessions` gained a `LIVE` column (and lists a live session that has
  no saved turns yet); `be-code sessions kill <code>` ends one from outside —
  it asks the host to quit, waits, and only then terminates it.
- One session renders one screen, so the shared size is the smallest attached
  terminal's. Below 70 columns or 20 rows the TUI switches to a **compact
  layout** (no header, one-line status, bare `>` prompt, popups without
  descriptions), which makes a phone SSH app a usable second head; `layout`:
  `auto` (default) | `compact` | `full`.
- The resume line written on exit (`resume: be-code --resume <code>`) now
  reaches every attached terminal, not just the process that happened to own
  the session, and a fresh served session's resume code is the same code used
  to attach to it.
- New config: `host_sessions` (true; `--no-host` for one run) and
  `live_idle_limit` (0 = never — minutes a served session may sit with no
  attached terminals and no run before exiting). Plain mode, non-TTY runs and
  headless `be-code run` are never hosted.
- Live records live in `~/.be-code/live/<code>.json` (0600, socket auth token)
  beside the host's unix socket and its startup log `<code>.log`. Attaching is
  local-only by design; remote access is SSH, not a network port.


## v0.4.5 — 2026-09-11 — terminal window colours, python verification fix

- The ten named themes also recolour the terminal window itself (OSC 11 for
  the background, OSC 10 for the foreground on light themes) on terminals
  that support it, and restore the terminal's own colours on exit or when
  switching to dark/light/mono. `theme_terminal_colors: false` disables it;
  plain mode never sends it.
- Python verification uses the workspace virtualenv's interpreter
  (`.venv/bin/python`, `venv/…`, Windows `Scripts\python.exe`) when present,
  so the project's own pytest is found. A missing pytest module or "no tests
  collected" (exit 5) is now reported as `[SKIP]` with the reason instead of
  a failure the model was asked to repair.


## v0.4.4 — 2026-09-11 — editable message queue, colour themes

- Ten colour themes join dark, light and mono: dracula, nord, gruvbox,
  monokai, one-dark, solarized-dark, solarized-light, tokyo-night,
  catppuccin (Mocha) and github-light. `/theme` opens a picker (also under
  /menu › Settings › Theme); `/theme <name>` sets one directly. The choice
  applies immediately and is saved to config. Hex palettes render as true
  colour where supported and degrade to 256 colours otherwise.

- Change your mind about queued messages: while the agent works, press Up on
  an empty input (or Ctrl+Q) to open the queue popup. Enter pulls the
  highlighted message into the input for editing (it is paused, out of the
  queue, until you press Enter again), `d` or Delete drops it, Esc closes.
  Delivery is held while the popup is open so the list cannot shift; the
  bottom line shows `N queued · ↑ edit`. Plain mode: `/queue`, `/queue edit N`
  (prefills the input line), `/queue drop N`, all usable mid-run. A message
  the agent already took reports "already delivered".

## v0.4.3 — 2026-09-11 — VS Code editor bridge

- BE-Code now connects to the companion VS Code extension when launched from
  its integrated terminal (or with `--ide`): editor tools attach as `ide_*`
  (diagnostics, symbols, definitions/references/hover, debugger), file writes
  can be reviewed as a diff in the editor before landing on disk, and a
  one-line note about the active file/selection is folded into each prompt.
  Controlled by `ide.enabled` / `ide.auto_context` in config and the
  `--ide` / `--no-ide` flags; `be-code doctor` reports bridge status.
- New `vscode/` companion extension providing the other half of the bridge: 16
  model-visible tools over an MCP-over-TCP server (`context`, `open`,
  `definition`, `references`, `hover`, `diagnostics`, ten `debug_*` tools —
  configs, start, breakpoint, continue, step, stack, variables, evaluate,
  output, stop), plus an internal `review_diff` tool that BE-Code calls itself
  for in-editor accept/reject of pending writes (hidden from `tools/list`, so
  the model never sees or calls it). The extension
  is discovered via a lock file it writes to `~/.be-code/ide/<pid>.json`
  (port, auth token, workspace folders), pruned of dead processes on lookup.
- `build.mk` gained a `vscode` target (`npm install`, `npm test`, `npm run
  package`, output into `dist/`) and `release` now depends on it, so
  `dist/be-code-<version>.vsix` (the extension's own package version) is
  produced alongside the cross-compiled binaries.

## v0.4.2 — 2026-09-11 — TUI redesign, deeper compression, web search

- TUI redesign: branded header (boxed "BE-Code Redux", logo, attribution,
  dashed rule) on terminals with 30+ rows; a "/" command palette that pops
  up above the input the moment "/" is typed, filters as you type, runs on
  Enter or fills the input for commands that take arguments, Esc keeps the
  typed text; a full-screen `/menu` grouped into Sessions, Models, Tools,
  Context and Settings with a status block (provider, model, profile,
  window, context, session total); the input row gets a `(>):` prompt and
  the context wheel (○ ◔ ◑ ◕ ● by fill when idle, ◴ ◵ ◶ ◷ rotating once
  per 3 seconds while the model works, percentage beside it); the status
  bar and help line collapse into one bottom line (`/menu /help · model ·
  state`).
- Text selection and copy/paste in the TUI: drag over the transcript to
  select (reverse-video highlight, survives streaming output), Ctrl+C copies
  a selection instead of arming quit, Esc clears, right-click opens a
  copy/paste popup (copy selection, last reply, last tool output, select
  all, paste into input), and `/copy [selection|reply|tool|all]` does the
  same from the keyboard and palette. Copy goes out as OSC 52 plus the
  first system tool found (wl-copy, xclip, xsel, pbcopy, clip); paste reads
  through the system tools. Shift-drag still uses the terminal's own
  selection.
- Header attribution reads "2026 BE AI Research · https://github.com/BE-AI-Research - v1.0";
  the trailing public version (`tui.PublicVersion`) tracks the user-facing
  release independently of the build version.
- Deeper context compression, for longer runs between compactions:
  compression now aims for half the limit (hysteresis) instead of stopping
  just under it; big tool-call arguments (file bodies passed to
  write_file/edit_file, which stayed in the prompt forever) are shrunk to a
  marker that keeps the path; model compaction keeps only the newest
  exchange and collapses its tool result, so the post-compaction prompt is
  essentially system prompt + summary.
- Web search (opt-in): `web_search` via Google Programmable Search Engine
  (`web_search.cx` + API key from `GOOGLE_PSE_API_KEY`) and `web_fetch`
  (readable page text). Both are available in plan mode; `doctor` reports
  the setup state.

## v0.4.1 — 2026-09-11 — interrupt buffer, reasoning headroom, message queue

- Mid-task message queue: while the agent is working you can keep typing.
  Enter queues the message (shown as "queued" in the transcript) and it is
  delivered to the model before its next call, tagged as a message that
  arrived mid-task; anything still queued when the run ends starts the next
  turn. Esc/Ctrl-C cancels the run and discards the queue. Works in the TUI
  and in plain mode (which now reads input concurrently with the run).
- Reasoning models get a generation reserve of a third of the window
  (4k–16k) instead of a quarter capped at 4k: Qwen3-class models spend
  thousands of tokens thinking before the first answer token, and with only
  4k free they hit the window mid-thought (empty content,
  `finish_reason=length`). The reserve is re-derived on `/model` switches.
  When a cutoff still happens, BE-Code collapses tool outputs, compacts, and
  retries once; the final error names the real cause (reasoning exhausted
  the window) and the two knobs that fix it.
- Backend resilience ("interrupt buffer") for shared Ollama servers:
  transient failures (connection refused/reset, 5xx, 429, "loading") are
  retried in place with backoff while the tool-loop state is kept, so a
  model swap or server restart no longer ends the task; before every model
  call the agent checks whether the model is still resident and with which
  window, clamps or restores the budget on change and says so; a silent
  backend (reloading, queued behind another client) is reported after 20s
  instead of looking like a hang; the model's keep-alive is refreshed after
  every request (`keep_alive`, default 30m) so idle expiry does not evict it
  between prompts; progress is autosaved when a call fails.
- Installers stamp the build version from `build.mk`, so `be-code --version`
  distinguishes an installed build from a stale one ("dev" = unstamped).
  `install.sh`/`uninstall.sh` are executable again.

## v0.4.0 — 2026-09-10 — context fidelity & quick resume

Root cause of the "stall after ~500k cumulative tokens": the Ollama backend
served the model with an 8192-token window while `context_tokens` was 32768,
and the chars/4 estimate undercounted real tokens by ~2x. Prompts were
silently truncated server-side (system prompt and task first), compaction
never fired, and an empty reply was accepted as a final answer.

- Backend window detection: for Ollama providers BE-Code reads the live
  `context_length` (/api/ps, else Modelfile num_ctx, else loads the model to
  find out) and clamps its budget to it, with a warning that names the fix
  (`OLLAMA_CONTEXT_LENGTH` or `PARAMETER num_ctx`). `doctor` shows the window.
- Token estimate recalibrates from the server's reported `prompt_tokens`
  every request (`stream_options.include_usage` is now requested); the
  default ratio is a conservative 3 chars/token instead of 4.
- The budget now counts the tools schema and reserves generation headroom
  (max_tokens, else a quarter of the window capped at 4096).
- Compaction runs inside the tool loop, not only between user requests, and
  the summary request carries the original task, the previous summary and the
  NEWEST transcript (it used to keep the oldest 24KB and drop recent work).
  The summary prompt now preserves user-stated requirements verbatim.
- Trim collapses compat-mode `<tool_result>` messages, not just native ones.
- Empty replies are no longer accepted as answers: one nudge, then a clear
  error. `finish_reason=length` with no output errors out immediately with
  the likely cause. Hidden reasoning from thinking models is surfaced as a
  "thinking" indicator in both UIs instead of dead air.
- Ollama auxiliary calls (compaction summaries, exit briefings) go through the
  native `/api/chat` with `think: false`. On a 27B thinking model the exit
  briefing dropped from 5 minutes (mostly hidden reasoning) to ~15 seconds.
  Requests with tools keep the OpenAI-compatible streaming path.
- Shell allowlist hardening: compound commands (`;`, `&&`, `||`, `|`, `&`,
  newlines, redirects, backticks, `$()`) are never auto-approved by an allow
  glob, so `go build; rm -rf .` prompts instead of riding on `go build*`.
  Deny globs now match every segment of a compound command.
- Plan mode keeps its planning prompt: the per-turn git refresh used to
  replace it with the normal coding prompt (telling a read-only agent to
  write files).
- Shell timeouts and `process stop` now kill the whole process group
  (Setpgid + WaitDelay), so a dev server or `sleep &` grandchild can no
  longer hang the tool past its deadline or survive `stop`.
- Verification runs only when the request could have changed files (a file
  or shell tool ran, or the checkpointer recorded edits); questions no longer
  trigger minutes of `go test`.
- Repair rounds (verify and reviewer) share the request's checkpoint turn, so
  `/undo` reverts the whole task and the reviewer sees every changed file.
- `/provider` and the model picker go through `SetModel`, refreshing the
  profile, compat mode and tool catalog.
- `write_file` without a `content` argument is an error instead of silently
  writing an empty file.
- A model profile's temperature applies when config is at its default; an
  explicit `temperature` always wins. `doctor` prints the profile notes.
- Repo map is rebuilt before the next request after the agent writes files.
- `@` mention completion no longer lists directories outside the workspace.
- Servers that answer a stream request with one JSON object are accepted;
  an empty body is an error instead of a silent empty reply.
- Verification checks gained `Fallback` (used for the node test runner) so
  check strings no longer need `2>/dev/null ||`, which broke on PowerShell;
  the bench edit-precision judge is now Go code instead of `grep`.
- MCP servers are closed on exit; timed-out calls no longer leak a pending
  entry; the handshake reports the real build version.
- `--version` (stamped by `build.mk`), `sessions delete <code|id>`, `vllm`
  in the default provider table, `/exit` and `/q` in autocomplete, Ctrl-C
  cancels `/plan` in plain mode, `post_write` hooks honor cancellation.
- Removed dead code: `RunWithVerification`, `DefaultShellDeny`, `tui.Quiet`,
  `verify.Check.RequiresFile`.
- Compaction is now a last resort: when the history goes over budget the
  cheap pass (collapse old tool output bodies, keep every turn) runs first,
  and the model-written summary only if that is not enough. Notices show
  the estimated tokens, the limit and the calibrated chars/token.
- Per-call tool output is capped relative to the usable window (¾ of the
  limit in bytes, 4KB–24KB) so a few file reads cannot fill a small window.
- Status bar: `ctx %` is relative to the compaction limit (100% = about to
  compact) and the cumulative counter is labelled `total`.
- Quick resume: every session has a 6-character code shown by `sessions`,
  `/sessions` and printed on exit as `resume: be-code --resume <code>`.
  `--resume` and `/resume` accept the code, the ID, or `last`.
- Handoff briefing: on exit the model writes a briefing (task, user-stated
  requirements, decisions, files changed, state, next steps) into the session;
  resuming injects it into the system prompt so the next session keeps the
  requirements and decisions. Headless `run` writes a heuristic briefing
  without an extra model call. `/handoff` shows the loaded briefing.

## v0.3.2 — 2026-09-02 — pre-flight hardening

- Fixed a TUI data race: the status bar rendered live agent state during runs;
  usage numbers are now snapshotted on the agent goroutine at quiescent points
  and the UI renders only the cached copy.
- Checkpoint storage is now cleaned up on session exit, with a 7-day sweep of
  leftovers from crashed sessions (previously ~/.be-code/checkpoints grew forever).

## v0.3.1 — 2026-09-02 — installers

- install.sh / uninstall.sh (Linux/macOS): source build with prebuilt fallback,
  user/system/custom-prefix targets, shell completions (bash/zsh/fish), PATH check,
  optional first-run wizard; uninstall keeps ~/.be-code unless --purge.
- install.ps1 / uninstall.ps1 (Windows): %LOCALAPPDATA%\Programs\be-code install with
  user-PATH management; -Purge for data removal.

## v0.3.0 — 2026-09-02 — pro-grade pass

- Checkpoints & multi-level /undo: files snapshotted before every agent edit.
- Repo map: symbol-level workspace outline in the system prompt (/map); @file mentions
  pin files into context with Tab completion.
- Model profiles: family detection (qwen3, deepseek-r1, gemma, llama, codellama, phi, …)
  auto-sets tool-call mode; <think> reasoning blocks filtered from streams & transcripts.
- Model-driven context compaction (/compact + automatic) with trim fallback.
- Plan mode: /plan runs a read-only planning phase → approval modal → execution.
- Git awareness: branch/status in prompt each turn; /commit with model-written message;
  /init generates BECODE.md.
- Custom slash commands (.becode/commands/*.md, $ARGS) and hooks (post_write, pre_shell).
- MCP client (stdio JSON-RPC): config-declared servers' tools become agent tools.
- Reviewer routing: optional second model reviews changes post-verification, one repair round.
- Shell allow/deny glob lists (deny never runs; allow skips prompts) + `process` tool for
  background processes (start/list/logs/stop, bounded log rings).
- be-code bench: embedded offline eval suite scoring models on real tasks (--json).
- run --json machine-readable results; /stats + status-bar token counts.
- Markdown + chroma syntax highlighting in the TUI transcript; dark/light/mono themes.

## v0.2.0 — 2026-09-02 — TUI + ease-of-use

- Full-screen Bubble Tea TUI (default on terminals): streaming transcript, styled tool
  activity, status bar (provider · model · ctx %), multi-line input, slash autocomplete,
  shared ↑↓ input history, filterable pickers for /model, /provider, /sessions.
- Plain REPL upgraded to readline: line editing, persistent history, Tab completion;
  `--plain` flag / `"ui":"plain"` / auto-selected off-TTY.
- Diff-preview approvals for all agent file writes (pure-Go unified diff engine,
  internal/diff), y/n/a in both UIs; denials inform the model.
- Session save/resume: autosave each turn to ~/.be-code/sessions; `be-code sessions`,
  `--resume <id|last>`, `/resume`, `/sessions` picker, `/clear` starts fresh.
- First-run setup wizard: concurrent backend probe (Ollama, llama.cpp, LM Studio, vLLM,
  BE AI Engine), model pick, config written; `be-code setup` re-runs it.
- New commands: `sessions`, `setup`; new flags: `--plain`, `--resume`.
- Config additions: `approve_file_writes`, `ui`.

## v0.1.0 — 2026-09-02 — core

- Agentic tool loop (read/write/edit/list/search/shell) with native + embedded
  (compat) tool calling for local models; forgiving argument parsing.
- Verification cycle: project detection (go/node/python/rust/make), build/lint/test
  runners, bounded auto-repair feedback loop.
- Providers: OpenAI-compatible SSE client (Ollama, llama.cpp, vLLM, LM Studio,
  BE AI Engine) + Ollama native management (pull/ps/warm).
- Context budgeting with two-pass trimming; workspace path confinement; shell approval.
- REPL + headless `run` mode; `models`, `pull`, `doctor`, `verify`, `config` commands.
