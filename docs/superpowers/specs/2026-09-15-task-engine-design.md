# Task Handling Engine — design

**Date:** 2026-09-15
**Version target:** 0.10.0
**Status:** approved in design review; ready for an implementation plan

## Goal

Stop the primary model re-reading files it has already read. As a project
grows, a long task compacts its history, the model loses the tool output it
had, and it reads the same files again to continue. The engine makes the
harness remember what was read, what mattered in it, what was looked up and
what the task is, and puts that back in front of the model after every
compaction and on resume. It also gives the model git-backed lookups that
return the function, the hunk or the delta it needs instead of whole files.

The engine follows BE-Code's governing premise: the model is small and
fallible, so the harness captures and re-injects on its own. Nothing here
depends on the model remembering to call a "recall" tool.

## Non-goals

- No MCP façade. The engine is an internal package with built-in tools; there
  is no outside consumer.
- No tree-sitter or language servers. Symbol outlines reuse the repo map's
  regex extractors.
- No changes to how compaction chooses *when* to run, only to what it keeps.
- No files written inside the user's project.

## 1. The store

One package, `internal/engine`. One store per workspace at
`~/.be-code/engine/<workspace-key>/`, where the key is the SHA-256 of the
workspace's cleaned absolute path, truncated to 16 hex characters. Moving a
project starts a fresh store. All writes are atomic (temp file plus rename),
the dotdir convention.

Four files:

| File | Scope | Content |
|---|---|---|
| `digests.json` | session | one record per file read: path, content hash (SHA-256 of bytes), size, mtime, symbol outline, line ranges seen, note, `turn` last touched, `edited` flag |
| `ledger.json` | session | task line, steps `[{text, status}]` with status `todo`/`doing`/`done`/`skip`, decisions `[]string`, facts `[]string`, `baseline` (HEAD hash plus the `git status --porcelain` text, capped at 8 KiB, recorded when a `RunFull` starts) |
| `lookups.json` | session | last 20 lookups: query, tool, `[{file, line, text}]` hits, hashes of the files hit |
| `notes.md` | durable | short facts the model chose to keep across sessions |

"Session scope" means the files are cleared when a new session starts in that
workspace (`be-code` without `--resume`, or `--new`) and kept on resume. The
store records the session id it belongs to; a mismatch on a non-resume start
clears the three session files.

Bounds: at most 200 digests and 256 KiB of `digests.json`; eviction is
least-recently-touched first. `notes.md` is capped at 4 KiB, trimmed at a line
boundary, oldest lines first. `lookups.json` keeps the newest 20.

A digest whose file hash no longer matches the file on disk is *stale*, never
deleted: the model is told the file changed rather than silently trusting old
knowledge. The check runs when the Working memory block is rendered, against
`mtime` and size first and the hash only when those changed.

The store is guarded by one mutex; renderers take a snapshot under it. The
TUI's `/task` reads from the UI goroutine while `dispatch` writes from the
agent goroutine.

## 2. Auto-capture

`Agent.dispatch` hands every tool result to `engine.Observe(call, result)`
after the result exists and before it is returned to the model. Observe never
returns an error to the caller: a failure is one `notice` and the result goes
back unchanged.

- `read_file` (any `offset`/`limit`): hash the file; extract the outline once
  per hash with the repo map's extractors for that extension (unknown
  extensions get no outline); record the range seen (default range is the
  whole file when no offset/limit was given, else the lines actually
  returned). If the file is unchanged and every line of the requested range
  was already seen, the result is returned in full with a footer line
  appended: `already read at turn N (unchanged); outline and notes are in your
  context`. The footer is a nudge, never a refusal.
- `search`, `lookup`, `history`: index the hits as `file:line — text` under
  the query. A repeated identical query whose hit files are unchanged is
  answered from the cache with the footer `(cached; files unchanged)` and no
  tree walk or git call.
- `write_file`, `edit_file`: refresh that path's digest from the new content
  (hash, outline) and set `edited`; ranges seen are reset to the whole file.
- `show` and `changes` are not digested (they describe other revisions).

The per-file note is the one field the harness cannot infer. It is filled
from two things the model already does: a `task note` with a `file` argument,
and the compaction summary, which is asked to end with a `files:` block of
`path — note` lines that the engine parses back into notes (unknown paths
are ignored).

The store is written to disk at turn end and at `Compact`, not per call;
in-memory state is authoritative during a run.

## 3. Re-injection

`composeSystem` gains one block, `Working memory`, rendered from a store
snapshot on every model call, placed after the repository map and before the
handoff. It is capped by `engine.budget` bytes (default 6144), trimmed at line
boundaries, and filled in priority order:

1. **Notes** — `notes.md`, when present.
2. **Task** — the ledger's task line; the `doing` step; done steps on one
   line; then `decisions:` and `facts:` lists.
3. **Files read** — one row per digest, most recently touched first:
   `path (lines 40–120, 200–260) — note`, with `[changed since read]` when
   stale and `[you edited this]` when `edited`. The outline is included only
   when the repository map does not already carry that file's symbols (the map
   was truncated before it), so the two blocks never repeat each other.
4. **Recent lookups** — the newest queries, one line each:
   `lookup "ApplyWindow": handoff.go:175, loop.go:311`.

When the block is empty (fresh store, no reads yet) it is omitted entirely.

`Compact` changes in three ways:

- The summary request includes the Working memory block as given context and
  the instruction "Do not restate anything already in Working memory."
- In the transcript it summarises, a `read_file` result that has a digest is
  rendered as `(read <path> lines a–b; digested)` instead of a 600-character
  stub. Other tool results keep the stub.
- The summary is asked to end with a `files:` block (Section 2), which the
  engine consumes and strips before the summary is stored.

The trim fallback is unchanged. Because the block lives in the system prompt,
it survives both compaction paths.

`Agent.Resume` rebuilds the block from the on-disk store beside the handoff
briefing, so a resumed session starts with what it had read and looked up,
not only what it concluded.

## 4. The `task` tool and the ledger

One flat tool, `task`, with tolerant parsing in the style of `consult`:

| `action` | arguments | effect |
|---|---|---|
| `plan` | `text` (the task in one line), `steps` (array of strings or one newline-separated string) | replaces the plan; all steps `todo` |
| `step` | `step` (1-based), `status` (`doing`/`done`/`skip`; aliases `progress`→`doing`, `finished`/`complete`→`done`) | marks a step |
| `note` (alias `decision`, tagged as a decision) | `text`, optional `file`, optional `keep` (aliases `remember`, `durable`) | appends a fact or decision; with `file`, also sets that digest's note; with `keep`, also appends to `notes.md` |

Errors are plain text: `task needs an action (plan, step, note)`,
`step 4 does not exist (3 steps)`, `note needs text`. There is no `recall`
action: the ledger is always in the Working memory block.

Harness assistance:

- At the start of `RunFull`, an empty ledger gets the user's request (first
  200 characters, one line) as its task line, and the baseline is recorded.
- When plan mode hands an approved plan to `RunFull`, its numbered or bulleted
  lines (up to 20) seed the steps.
- `finishSession` reports any step still `doing` in the handoff briefing under
  `Stopped at:`.
- The system prompt gains a guidance paragraph on context use and progress
  notes, present whenever the `task` tool is registered:

  > Context is limited and does not survive compaction; your notes do. Working memory below lists what you have already read: do not read those files again unless they are marked changed. Read only the lines you need (read_file with offset and limit) instead of whole files. Before a change that takes several steps, record a plan with the task tool, mark each step as you finish it, and record decisions and facts as you learn them. When a file matters for later, note what matters in it (task note with file) so you need not read it again. If a git tool answers `not a git repository`, do not retry it: work from read_file ranges and keep your task notes and durable notes (task note with keep) up to date instead, because they are then your only memory across compaction.

  and, for each git tool actually registered (all four with `engine.tools:
  full`, only `lookup` with `minimal`), one sentence on when to use it:

  > To find where something is defined or used, call lookup (git grep over tracked files; symbol=true returns the whole enclosing function) before search or read_file.
  >
  > Before changing code you do not understand, call history on that file (symbol, lines, query or blame) to learn why it is the way it is.
  >
  > To compare a file with an earlier revision, call show with rev instead of reading and guessing.
  >
  > Before verifying, reviewing or summarising your work, call changes to see exactly what you altered instead of re-reading whole files.

User commands, in `ui.SlashCommandTable`, both UIs, busy-safe, view-local in
the TUI:

- `/task` prints the ledger; `/task clear` clears the session-scoped store for
  the workspace (`ledger`, `digests`, `lookups`; never `notes.md`).
- `/notes` prints `notes.md` with line numbers; `/notes add <text>` appends a
  line; `/notes drop N` removes line N; `/notes clear` empties it. (An
  `$EDITOR` round trip would contend with plain mode's readline goroutine for
  stdin, and a popup editor is more machinery than a 4 KiB file deserves.)

## 5. Git-backed lookup tools

Four read-only tools in `internal/tools`, using `gitctx`'s `git` helper,
confined to `Registry.Root`, never prompting. Every call has a 20-second
timeout; the error text is returned as a normal tool error. Outside a git
repository, `lookup` falls back to the existing walk-based search and the
other three return `not a git repository`. Output is capped at
`Registry.MaxOutput` like every tool. All four join plan mode's read-only
subset.

- `lookup` — `query` (literal; `regex: true` for a pattern), optional `path`
  (prefix filter), `symbol: true` for function context (`git grep -n -I -W`,
  which returns the whole enclosing function once per hit). Output
  `file:line: text`. Hits are indexed into the lookup cache.
- `history` — `path` plus exactly one of: `symbol` (`git log -L
  :symbol:path`), `lines` (`a,b` → `git log -L a,b:path`), `query` (`git log
  -S query -- path`, or repository-wide without `path`), `blame` with `lines`
  (`git blame -L a,b`). Entries are condensed to short hash, date, author,
  subject, and for `-L` the hunk; newest 10 by default (`limit`).
- `show` — `rev` (default `HEAD`), `path`, optional `offset`/`limit` as in
  `read_file`. Reads `rev:path` from the object store.
- `changes` — optional `since` (a revision) or, by default, the ledger's
  baseline; optional `path`. Returns `git diff --stat` and, with `path`, that
  file's unified diff. A dirty baseline (uncommitted changes when the task
  began) is compared as working tree vs. HEAD plus the recorded porcelain, so
  the model sees only what *this* task changed.

Tool count: the four plus `task` add five descriptions to every prompt.
`engine.tools` (`full` default; `minimal` registers only `task` and `lookup`)
lets a tight compat-mode setup trim it.

## 6. Configuration

```json
"engine": {
  "enabled": true,
  "budget": 6144,
  "notes_cap": 4096,
  "tools": "full"
}
```

`enabled: false` registers no tools, captures nothing and renders no block:
0.9.0 behaviour. `Load()` fills zero budgets with the defaults. Documented in
the README config reference.

## 7. Error handling

The engine is advisory everywhere:

- A store that cannot be read or written: one notice
  (`engine: <err>; continuing without working memory`), and the session runs
  as in 0.9.0 — no block, no footers; the tools still work but nothing is
  cached.
- A corrupt JSON file is renamed `<name>.broken-<yyyymmdd-hhmmss>` and started
  fresh; one notice.
- Git failures (no commits, shallow clone, path outside root, timeout) are
  plain tool errors, never panics.
- Observe is wrapped in `recover`; a panic is logged and the result returned
  unchanged.
- Rendering never blocks the agent: it takes the store mutex for a snapshot
  only.

## 8. Testing

- `engine`: digest capture on read, ranged read, write and edit; staleness on
  change; the `already read` and `cached` footers; eviction at both caps;
  session-scope clearing on a non-resume start and preservation on resume;
  `notes.md` cap and trim; corrupt-file recovery.
- Block rendering: priority order under budget, omission when empty, outline
  suppression when the repo map has the file, stale and edited markers.
- `Compact`: digested reads rendered as `(read … digested)`; the `files:`
  block parsed and stripped; "do not restate" present in the request.
- `task`: every action and alias, every error string; the `RunFull` seeding;
  plan-mode step seeding; `Stopped at:` in the handoff.
- Git tools against a temporary repository (as `gitctx`'s tests build one):
  each mode, the 20 s timeout, no-repo fallbacks, root confinement.
- UIs: `/task`, `/task clear`, `/notes`, `/notes clear` in plain mode and the
  TUI; busy-safe table entries.
- E2E: the scripted primary reads a file three times under a small budget,
  the older reads are trimmed from the transcript, the next request's system
  message still carries the digest row, and the third read carried the
  `already read` footer.

## 9. Documentation

README section "Working memory" after "Context: repo map, @mentions,
compaction" (what is kept, the `task` tool, the four git tools, `/task`,
`/notes`, the `engine.*` keys), CHANGELOG `v0.10.0 — working memory`,
`build.mk` 0.10.0, root `CLAUDE.md` subsection "Task engine" under
Architecture (store, Observe, the block, `Compact` changes, the tools).
