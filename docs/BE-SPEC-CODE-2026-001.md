# BE-SPEC-CODE-2026-001 — BE-Code v0.1

**Component:** BE-Code (BE-Continuum → BE AI Engine adjacency)
**Class:** Offline agentic coding CLI
**Status:** v0.1.0 implemented; spec reflects as-built
**Date:** 2026-09-02

## 1. Mission

Produce high-quality code with *local* models, fully offline. BE-Code synthesizes three
lineages — BE-CLI (Go architecture, dotdir conventions, path-security hardening), Claude Code
(agentic tool loop, project memory, interactive REPL), and Codex (headless prompt→patch
pipeline mode) — around the governing constraint that local models are small and fallible.
The harness, not the model, is responsible for final quality.

## 2. Architecture

```
┌─────────────────────────────────────────────────────────┐
│ cmd/ (cobra)   REPL │ run │ models │ pull │ doctor │ verify │
├─────────────────────────────────────────────────────────┤
│ internal/agent      loop · prompt · history (token budget) │
│   ├── internal/tools    read/write/edit/list/search/shell  │
│   └── internal/verify   detect · check · repair summaries  │
├─────────────────────────────────────────────────────────┤
│ internal/provider   OpenAI-compat (SSE+tools) · Ollama mgmt │
│        ollama │ llamacpp │ lmstudio │ vllm │ be-ai-engine   │
└─────────────────────────────────────────────────────────┘
```

### 2.1 Provider layer
One wire protocol — OpenAI `/v1/chat/completions` with SSE streaming — reaches every
supported backend, including the BE AI Engine fabric (any node exposing the compatible
endpoint). Tool-call fragments are merged by index; servers that omit indexes (llama.cpp)
are handled. Ollama backends additionally get native `/api` management: tags, pull with
progress, ps, and model warm/keep-alive.

### 2.2 Agent loop
Turn-bounded (`max_turns`) tool loop. Native tool calling is used when the backend supports
it; **compat mode** embeds the tool catalog in the system prompt and parses
`<tool_call>{...}</tool_call>` (and fenced-JSON variants, and `tool`/`parameters`/`input`
field aliases) from plain text. Mode `auto` starts native and downgrades per-session when
the backend rejects the tools field. Tool argument parsing tolerates double-encoded JSON
and alternate key names — misfires return corrective messages to the model, never crashes.

### 2.3 Context budgeting
`context_tokens` budget (chars/4 estimate). Trim order: (1) collapse tool outputs older than
the last 8 messages, (2) collapse all but the newest tool output, (3) drop oldest turns after
the first user message (the task statement is never dropped). Orphaned tool results are
rewritten as user-role notes so strict backends never see unpaired messages.

### 2.4 Verification cycle (the quality multiplier)
`Detect` maps workspace markers to check suites — go.mod → vet/build/test; package.json →
tsc(-noEmit)/npm test; pyproject → compileall/pytest; Cargo.toml → check/test; Makefile →
make test. `RunChecks` stops at the first failure. Failures are condensed (head+tail slice,
6 KB cap) into a repair prompt; the loop re-runs up to `max_repairs` times. Exit codes make
`be-code run`/`be-code verify` CI-safe.

### 2.5 Security posture
File tools are confined to the workspace root via cleaned-path `Rel` checks (BE-CLI Issue #4
lesson applied from day one). Shell execution is human-approved per command by default;
`-y` is an explicit operator decision. API keys resolve from environment variables only;
the config file never stores secrets. Zero network egress except configured inference
endpoints.

## 3. Interfaces

- Interactive: `be-code` REPL (slash commands: /models /model /verify /tools /config /clear).
- Headless: `be-code run "<prompt>"` or stdin pipe; streams progress to stdout, verification
  report to stderr, non-zero exit on unrepaired failure. This is the integration surface for
  other Continuum components (BE-PAP, schedulers, CI).
- Project memory: `BECODE.md` (fallback `CLAUDE.md`) injected into the system prompt, 8 KB cap.

## 4. Verification of BE-Code itself

Unit suites: provider SSE assembly (fragmented/indexless tool calls, HTTP error surfacing);
tools (path confinement, loose args, unique-match edits, approval denial); agent (embedded-call
parsing incl. innocent-JSON rejection, budget trimming, max-turn stop, native+compat loop with
scripted provider); verify (marker detection, first-failure stop, live go-toolchain
break/fix). End-to-end: real binary against a scripted OpenAI-compatible mock that writes
broken Go, is caught by verification, and repairs on the feedback prompt
(`test/e2e/`). Cross-compiles clean: linux/amd64, linux/arm64, windows/amd64, darwin/arm64.

## 5. v0.2 addendum — dual UI and ease-of-use layer (as-built)

**Dual interface, one core.** `internal/tui` is a full-screen Bubble Tea app (transcript
viewport, streaming deltas, spinner status bar with context-usage %, multi-line input,
slash autocomplete, ↑↓ history); `internal/ui` remains the inline REPL, upgraded to
readline (editing, persistent shared history at `~/.be-code/history`, Tab completion).
Selection: TUI on a TTY, plain via `--plain` / `"ui":"plain"` / non-TTY auto-detect. Both
UIs implement the same `tools.ApproveFunc` seam; the agent goroutine bridges into the TUI
event loop via message-passing with a reply channel.

**Diff-preview approvals.** `internal/diff` (pure-Go line LCS, prefix/suffix trim,
hunk builder, 4M-cell guard) renders unified previews; `write_file`/`edit_file` route
through approval before touching disk. Denials return corrective messages to the model.
`y/n/a` semantics in both UIs; headless denies without `-y`.

**Sessions.** `internal/store` persists conversations to `~/.be-code/sessions/*.json`
(atomic writes, BE-CLI dotdir convention); the agent autosaves after every completed
request. Resume via flag, command, or TUI picker.

**First-run wizard.** `internal/setup` probes the five standard local endpoints
concurrently (4 s budget), lists models on reachable backends, and writes the initial
config; all candidates stay in the config as named providers. Runs on plain stdio before
any UI starts, so it behaves identically everywhere; `be-code setup` re-runs it.

## 6. v0.3 addendum — pro-grade layer (as-built)

**Rollback.** `internal/checkpoint` snapshots each file before its first modification
per turn (agent-edit scope; shell effects are git's domain). `/undo` is LIFO across
turns; denied/empty turns are discarded. Undo also informs the model via a context note.

**Context system.** `internal/repomap` builds a regex-extracted symbol outline (Go, Py,
JS/TS, Rust, Java, C/C++; budget-capped, junk-dir-aware) injected into the system prompt.
`@path` mentions inline file content (16KB cap, traversal-guarded) with Tab completion.
Git branch/status refreshes into the prompt each turn (`internal/gitctx`). Over-budget
histories are compacted by the model itself (300-word structured summary + last 4
messages), with the v0.1 trim pass as fallback.

**Model adaptation.** `internal/profiles` maps model-name families to tool-call mode and
think-block handling; longest-match resolution ("deepseek-r1…qwen-distill" → deepseek-r1).
`agent.ThinkFilter` strips reasoning tags from live streams with partial-tag holdback.

**Workflow.** Plan mode (read-only tool subset → approval → execution with plan pinned);
`/commit` (model-written message over `git diff`); `/init` (agent-written BECODE.md);
custom commands (`.becode/commands/*.md`, workspace shadows global); hooks
(post_write/$FILE, pre_shell/$COMMAND).

**Extensibility.** `internal/mcp`: stdio MCP client (2024-11-05 handshake, newline JSON-RPC,
per-call timeouts, EOF fail-fast); server tools register as `mcp_<server>_<tool>`.
Reviewer routing: configured second model reviews changed files (from checkpoints) after
verification passes; APPROVED gate or one repair round.

**Execution safety.** Deny globs never run and never prompt; allow globs skip prompts;
`*` crosses spaces/slashes; defaults ship both lists. `process` tool manages background
processes with 64KB ring buffers, killed at session end.

**Measurement.** `internal/bench`: five embedded tasks (compile fix, function
implementation, logic bug, Python fix, edit precision) run through the real agent in
temp workspaces, judged by real toolchains, reported per model with score/turns/tokens
(`--json`). `run --json` exposes the full outcome envelope for Continuum integration.
Session `Stats` (requests, tool calls, tokens, elapsed) surface in /stats and the TUI
status bar.

## 7. Roadmap candidates (v0.4+)

Sandboxed shell profiles (landlock/seccomp); tree-sitter repo maps; retrieval over large
repos; parallel sub-agents; BE-CLI session integration (BE-Code as a managed BE-CLI
service); fleet-wide bench orchestration via BE AI Engine.
