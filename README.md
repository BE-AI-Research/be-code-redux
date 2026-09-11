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
                          # Go >= 1.22 is present, else uses bin/ or dist/ prebuilt);
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
agent is idle.

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
the `/sessions` picker; `be-code sessions delete <code>` removes one. Headless `run` prints the resume line to stderr and writes a
quick heuristic briefing (no extra model call). `/clear` starts a fresh session.

## Checkpoints & undo

Every agent turn snapshots files before they're touched. `/undo` rolls back the last
turn's edits (multi-level, LIFO — created files are removed, modified files restored).
Shell side effects aren't snapshotted; that's what git awareness is for.

## Context: repo map, @mentions, compaction

A symbol-level repository outline (`/map` to view) is injected into the system prompt so
small models spend turns editing, not exploring. `@path/to/file` in any message pins that
file's content into context (Tab completes @paths in the TUI). When the conversation
exceeds its budget, BE-Code has the model summarize older turns (`/compact` to force it)
instead of dropping them; plain trimming remains the fallback.

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
is a repo. `/init` has the agent survey the repo and write `BECODE.md`. Custom slash
commands are markdown prompt templates in `.becode/commands/*.md` (workspace) or
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
conventions, gotchas. It is injected into the system prompt, exactly like Claude Code's
project memory.

## Safety model

- All file tools are confined to the workspace root (path-traversal hardened, per BE-CLI lessons).
- Every shell command requires interactive approval (`y`/`N`/`a`lways) unless `-y` /
  `auto_approve_shell` is set.
- No telemetry, no network calls except to your configured inference endpoints. Fully offline.

## Layout

```
cmd/                 cobra commands (root, run, bench, models, pull, doctor, verify, config, sessions, setup)
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
internal/commands/   custom slash commands (.becode/commands)
internal/bench/      embedded offline eval suite
internal/store/      session persistence (~/.be-code/sessions)
internal/setup/      backend probe + first-run wizard
internal/config/     ~/.be-code/config.json
internal/ui/         plain REPL (readline), markdown/chroma rendering, shared helpers
internal/tui/        full-screen Bubble Tea UI (transcript, modals, pickers, themes)
```

## Config reference (`~/.be-code/config.json`)

- `default_provider`, `model`, `providers{}` — backend selection
- `temperature` (0.2), `max_tokens`, `context_tokens` (16384) — generation/budget.
  `context_tokens` is the total prompt budget (system prompt, tools schema and
  history) and is clamped to the backend's real window when BE-Code can read
  it (Ollama: `/api/ps`, Modelfile `num_ctx`). Generation headroom is reserved
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
- `shell_allow` / `shell_deny` — command glob lists; `hooks` — post_write / pre_shell
- `repo_map` (true) + `repo_map_budget`; `compact_with_model` (true)
- `mcp_servers` — stdio MCP tool servers; `reviewer` + `review_on_done` — second-model review
- `ide.enabled` (true), `ide.auto_context` (true) — the VS Code editor bridge; see "VS Code"

## Status

v0.3.0 — the pro-grade pass: checkpoints/undo, repo map + @mentions, model profiles +
think-filtering, model compaction, plan mode, git awareness + /commit + /init, custom
commands + hooks, MCP client, reviewer routing, shell allow/deny + background processes,
bench suite, JSON output, markdown/syntax highlighting, themes, usage stats. Earlier:
v0.2.0 (dual UI, wizard, diff approvals, sessions), v0.1.0 (core loop + verification).
4-platform builds; 15 tested packages plus scripted-model e2e (repair loop, MCP attach,
undo, JSON mode, bench harness) driven through the real binary.
