# The task document format

BE-Code's working memory is a task tree, and the tree lives in the user's own
project as Markdown: one document per top-level task under
`<project>/.be-code/tasks/`. This is the long form of the format. The short
form — enough to read and edit these files — is written into
`.be-code/tasks/README.md` the first time a document is saved there, and that
file points back here.

Two rules govern everything below. The document is the user's file, so a line
the engine does not understand is preserved verbatim and comes back untouched.
And a document the engine cannot understand *at all* is quarantined, never
half-parsed and rewritten.

## Where things live

```
<project>/.be-code/
  tasks/
    README.md                 the short form, written once, never rewritten
    001-fix-the-parser.md     one top-level task
    002-add-the-cache.md
~/.be-code/engine/<key>/
    state.json                the disposable half (see "The dotdir half")
    notes.md                  durable notes (task note keep:true)
```

`<key>` is a 16-hex-character hash of the absolute workspace path, so two
checkouts of the same project keep separate state. The first document written
into a project adds `.be-code/` to the project's `.gitignore`, so the record
stays out of `git status` uninvited; deleting that line is how a user commits
the record. Outside a git repository nothing creates a `.gitignore` — but if
one is already there, the line is still added.

## The document

```markdown
# 001 — fix the parser

- [x] 1. fix the parser
  - [x] 1.1. find the bug
    - files: lexer.go (lines 1–120)
    - cmds: go test ./... — failed
    - error: FAIL: TestLex
  - [>] 1.2. fix and verify
    - decision: keep the old token type; the callers depend on it
  - [-] 1.3. rewrite the scanner — dropped: not needed after all
```

**The heading** is `# NNN — <task text>`. It is regenerated on every save, and
`NNN` is taken from the file name so the two never disagree.

**A step** is `- [<mark>] <id>. <text>`, indented two spaces per level. Depth is
unbounded: a step that turns out to be bigger than it looked grows children in
place instead of renumbering its siblings.

**Ids** are dotted paths — `2.1.3` is the third child of the first child of the
second task — and they follow position, not anything typed by hand.

**The five marks**, one per step:

| mark  | status    | meaning                 |
|-------|-----------|-------------------------|
| `[ ]` | `todo`    | not started             |
| `[>]` | `doing`   | in progress             |
| `[x]` | `done`    | finished                |
| `[!]` | `blocked` | could not proceed       |
| `[-]` | `dropped` | decided not to          |

`blocked` and `dropped` stay distinct all the way into a report: "could not"
and "decided not to" read very differently six turns later. Each carries its
reason after an em dash — `— dropped: not needed after all`. `[X]` is read as
`done` as well, since that is what a person types.

## Evidence

Evidence lines sit one level deeper than the step that earned them, each
starting with one of six keys:

| key         | shape                                                | written by                          |
|-------------|------------------------------------------------------|-------------------------------------|
| `files:`    | `path (lines a–b, c–d) [edited] — note`               | `read_file`, `write_file`, `edit_file` |
| `cmds:`     | `<command> — ok` / `— failed`                         | `shell`, `process`                  |
| `lookups:`  | `<tool> <query> → a.go:12, b.go:3`                    | `search`, `lookup`, `history`       |
| `note:`     | free text, optionally `[file: path]`                  | `task note`                         |
| `decision:` | the same, flagged as a decision                       | `task note decision:true`           |
| `error:`    | `<command>: <first line of the failure>`              | any failing tool call               |

Every part of a `files:` line after the path is optional. Ranges are what the
model was actually shown: a `read_file` contributes the lines it echoed back, a
`write_file` the whole file (the model authored every line), and an `edit_file`
contributes none at all — it replaced a fragment of a file it may never have
read, and claiming the whole file would answer a later read with "already read"
for lines nobody has seen.

## How evidence reaches a step

Evidence attaches to whichever node is `doing` when the tool call returns.
Exactly one node is `doing` at a time; marking a node `doing` closes the
previous one and adopts any `unfiled` evidence.

When nothing is `doing`, the engine opens an `unfiled` node under the active
top-level task rather than guessing which step the work belonged to — a report
that is confidently wrong is worse than one that says unfiled. The next
`task status <id> doing` adopts it.

## Fidelity: the verbatim buffer and distillation

While a node is `doing` it keeps every tool call and its output **verbatim**,
capped per item (`engine.item_cap`, 4 KiB) and per node (`engine.node_cap`,
32 KiB), oldest dropped first so one large read cannot swallow the buffer. A
drop is counted in the node and rendered as `(N item(s) dropped)`, so the
record never silently claims to be complete. The caps bound the *buffer*, not
the record: a read larger than `item_cap` still records every line it returned.

When a node leaves `doing` it is **distilled in place** from that buffer into
the evidence lines above. Distillation reads the buffer, never the transcript,
so it does not depend on a transcript that compaction has already thrown away.
The buffer is not in the document — it is the one part of the record that lives
in the dotdir.

## Task Reports

When a top-level task reaches a terminal status (`done`, `blocked` or
`dropped`), its branch rolls up into a Task Report: the task line and its
status, one line per child with status and reason, the files touched, the
commands that succeeded, the decisions made, the errors hit, and what was left
blocked or dropped. It is built from the tree alone — never a model call — so
the same record renders the same bytes every time.

Reports are what the system prompt carries for finished work, oldest first,
above the active branch. Under `engine.budget` (6144 bytes) they render in
full; as the block approaches the cap they condense oldest first, through: full
→ headline with outcomes and decisions → one line → a pointer to open with
`task show <id>`. **The active branch and its verbatim step are never what gets
cut**; if the cap cannot be met with them intact, the block says
`(reports condensed)` and carries on.

## Editing by hand

Safe to change: a step's text, its status mark, the reason after the em dash,
and any `note:` or `decision:` line. A hand-edited status is read as the user's
intent and is never overridden.

Anything the engine does not recognise — prose, a heading of your own, an
ordinary checklist kept in the same file — is preserved exactly and written
back after the tree. A **top-level** checklist item is only read as a task when
it carries a number, so `- [ ] buy milk` at the bottom of a file stays the
user's own text (and so does anything indented under it); `- [ ] 2. buy milk`
is tracked as a task.

**Ids are repaired by position.** Insert, delete or reorder steps by hand and
the engine renumbers them back into position on the next load, leaving a note
on the affected step — `- note: id repaired from 1.3 to 1.2` — as the whole
record of the disagreement. Nothing is printed.

**Two `[>]` marks are a warning, not a repair.** Exactly one step across the
whole record should be `doing`. The engine keeps that true when *it* moves a
step, but it never overrides a hand-edited status, so it does not fix a second
`[>]`: it prints a line naming the documents involved every time it loads the
record in that state, and uses whichever it reads last. Neither document is
modified.

**A document is never renamed or deleted** once it exists — not even when the
task is retitled, which is why a file name can drift out of step with the
heading inside it. The heading is the one to trust.

## Quarantine

Broken indentation, an unknown status mark, or a step indented under nothing
makes a document unparseable. It is renamed `NNN-<slug>.broken-<stamp>.md` in
the same folder with its content untouched, a line is printed saying so, and
that task is absent from the engine's memory until the copy is fixed and
renamed back. Nothing is deleted.

## The dotdir half

`~/.be-code/engine/<key>/state.json` holds only what is not the user's to read,
and all of it is disposable: the turn counter, the `doing` node's id (for a
human reading the file — the document's own `[>]` is the truth), the verbatim
buffers, the hash of each document as the engine last wrote it (to notice
outside edits), the task's git baseline, and the content hash and symbol
outline of each file the record names. Losing this file costs the verbatim
buffer and the outlines, not the tree — the tree comes back from the Markdown,
which always wins over the dotdir, because it is the user's file and may have
been edited since.

`notes.md` beside it holds the durable notes (`task note` with `keep: true`).
They survive across sessions and are loaded even when the session id differs.

## Migration from 0.10.0

A 0.10.0 store (a flat `ledger.json`, `digests.json`, `lookups.json`) is lifted
on first open into one top-level task: steps become children, decisions and
facts become notes, digests and lookups become that task's evidence, and the
Markdown document is written. The three old files are then renamed aside as
`<name>.migrated-<stamp>` — they are never deleted. A migration that cannot
complete leaves the 0.10.0 store untouched and says so, and one that fails
irrecoverably renames the whole store directory aside rather than leaving it
half-converted.

## The `task` tool and the `/task` command

The model edits the tree through one tool with five actions: `plan` (a task and
its steps in one call), `add` (a node under `parent`, returns its id), `status`
(`doing` | `done` | `blocked` | `dropped`, with `reason` for the last two),
`note` (text, optional `id`, `file`, `decision`, `keep`) and `show` (a node, a
branch, or the whole tree).

A person uses `/task` (the whole tree), `/task show <id>` (one branch),
`/task open` (the path to the documents) and `/task clear`. **`/task clear`
never deletes anything**: it closes every still-open node as `dropped` with the
reason `cleared`, distilling buffers on the way, and resets the lookup cache
and the task baseline. The documents and the durable notes are untouched.
