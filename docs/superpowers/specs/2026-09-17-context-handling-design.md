# Context Handling Overhaul — Design

**Date:** 2026-09-17
**Status:** approved for planning
**Supersedes:** the flat working-memory ledger introduced in 0.10.0 (`docs/superpowers/specs/2026-09-15-task-engine-design.md`), which this design rewrites. Digests, the lookup cache and durable notes survive; the ledger does not.

## 1. Why

Two failures, one cause: the model loses its grip on a long task.

**The window is smaller than it needs to be.** `config.json` has no way to set Ollama's context window. Every normal request goes through Ollama's OpenAI-compatible endpoint, which cannot carry `num_ctx`, so the harness can only read the window the server happens to have and clamp its own budget down to it. On the validation VM this means compaction fires early and often.

**Compaction loses the thread.** The working memory that survives compaction is one flat ledger: a single task, its steps, a list of decisions and a list of facts. It cannot say what was actually done, in what order, with what result. So after a compaction the model re-reads files it has already read, repeats work, and in the worst case stops mid-task. A summary that comes back empty leaves it with nothing but a trimmed transcript.

This design fixes both, as one piece of work, because they are the same problem seen from two ends: how much room the model has, and what it keeps when the room runs out.

## 2. What we are building

1. **Task Engine v2.0** — `internal/engine` rewritten around a task tree. Every piece of evidence hangs off the node that produced it. Finished branches become Task Reports. The recorder runs continuously, so compaction costs nothing extra and loses nothing.
2. **Full native Ollama** — `/api/chat` with an options block as the normal path for `type: ollama`, carrying `num_ctx`, streaming and tool calls.
3. **An editable, self-checking window** — `context_window` in config, reconciled with the server under an explicit consent gate.
4. **A model loader** — per-model parameters, applied on every switch, resolved off the UI thread.

Out of scope: the OpenAI provider's behaviour beyond staying a working fallback, the context engine in `be-code-exp` (separate lineage), and any change to the approval seam.

## 3. Global constraints

- **Advisory, always.** No engine or loader path may fail a turn, block the loop, or panic a session. A store that will not open leaves the `task` tool registered over a no-op. A recorder panic is fenced and reported once. This matches `Agent.observe` today.
- **Deterministic recording.** The recorder never calls a model. Everything in a Task Report is derived from what actually happened.
- **The user's server is shared.** Any action that changes server state — a reload, or a `num_ctx` that differs from the loaded instance — happens only after explicit consent, and never in a non-interactive run.
- **Path confinement and atomic writes** as everywhere else in the harness.
- **No new dependencies.**
- Go 1.22+, `make -f build.mk verify` green, `go vet` clean.

## 4. The task tree

### 4.1 Nodes

A node is:

| field | meaning |
|---|---|
| `id` | dotted path, e.g. `2.1.3`; a bare number is a top-level task |
| `text` | one line, what this node is |
| `status` | `todo` \| `doing` \| `done` \| `blocked` \| `dropped` |
| `reason` | why, for `blocked` and `dropped` |
| `children` | ordered; depth is unbounded |
| `evidence` | see 4.2 |
| `opened` / `closed` | timestamps |

Depth is unbounded so a step that turns out to be bigger than it looked can grow children in place, without renumbering its siblings. `blocked` and `dropped` are distinct because "could not" and "decided not to" read very differently in a report six turns later.

Exactly one node is `doing` at a time. Marking a node `doing` closes the previous one (distilling it, 4.3) and adopts any `unfiled` evidence (4.2).

### 4.2 Evidence

Evidence attaches to the node that was `doing` when it happened:

- **files** — path, ranges seen, content hash, edited flag, note
- **commands** — the command, exit status, a capped excerpt of output
- **lookups** — tool, query, hits
- **notes and decisions** — text, optional file, `keep` flag for cross-session notes
- **errors** — first line, plus the command that produced it

When nothing is `doing`, the engine opens an `unfiled` node under the active top-level task rather than guessing which step the work belonged to. Filing evidence under the wrong step produces a report that is confidently wrong, which is worse than one that says unfiled. The next `status doing <id>` adopts the unfiled evidence.

### 4.3 Fidelity and distillation

The `doing` node keeps raw tool calls and their output **verbatim**, capped per item (`engine.item_cap`, default 4 KiB) and per node (`engine.node_cap`, default 32 KiB), oldest dropped first, so one large read cannot swallow the buffer. A drop is recorded in the node, so the record never silently claims to be complete.

When a node leaves `doing` it is **distilled in place** from that buffer: files and ranges with the edited flag, commands with outcomes, decisions, notes, and the first line of each error. Distillation reads the buffer, never the transcript, so it does not depend on the transcript still existing.

### 4.4 Task Reports

When a top-level task reaches a terminal status, the engine rolls its branch up into a Task Report, deterministically: what was done in order, files touched, commands and outcomes, decisions taken, what was left `blocked` or `dropped` and why.

## 5. Storage

### 5.1 Markdown in the workspace

Truth lives in the project at `.be-code/tasks/`:

```
<project>/.be-code/
  tasks/
    README.md
    001-fix-the-parser.md
    002-add-the-cache.md
```

One document per top-level task. `.be-code/` is added to the project `.gitignore` on first use; deleting that line is how a user commits the record. `README.md` documents the format, the id scheme, which fields a human may edit safely, and what the engine does when it disagrees with what it reads. It addresses both readers, human and model, because both edit these files.

### 5.2 Reconciliation with hand edits

On load the engine parses tolerantly:

- unknown lines are preserved verbatim and round-trip untouched;
- a hand-edited status, text or note is respected as the user's intent;
- a document that cannot be parsed is **quarantined** — renamed `NNN-name.broken-<stamp>.md` — and rebuilt from the dotdir state, never silently overwritten;
- ids are read from the document; a duplicate or missing id is repaired by position, and the repair is noted in the document.

### 5.3 Dotdir state

`~/.be-code/engine/<workspace-key>/state.json` holds only what is not the user's to read: the active node id, the verbatim buffer for the `doing` node, the turn counter, and the last-known hash of each task document (to detect outside edits). It is written atomically and is disposable: losing it costs the verbatim buffer, not the tree.

### 5.4 Migration

On first open of an existing 0.10.0 store the engine lifts the flat ledger into one top-level task: steps become children, decisions and facts become notes, digests and lookups become that task's evidence. The Markdown document is written. A store that fails to migrate is renamed aside and started fresh with a notice, never half-converted.

## 6. The prompt

The `Working memory:` block becomes a task view, composed fresh every turn, in this order:

1. **Task Reports** for finished branches, inline, oldest first.
2. **The active branch** — the path from the top-level task to the `doing` node, with sibling statuses so what remains is visible.
3. **The `doing` node verbatim** — the lossless part.
4. **Durable notes** (`keep: true`), as today.

### 6.1 The budget ladder

The block is capped by `engine.budget` (default 6144 bytes, as today). Under the cap a report renders in full. As the block approaches the cap, reports condense oldest first, through: full → headline plus outcomes and decisions → one line → a pointer the model opens with `task show <id>`.

**The active branch and its verbatim step are never what gets cut.** They are the work in flight. If the cap cannot be met with them intact, the block reports that reports were condensed and carries on.

## 7. Compaction

The tree is always current, so the threshold triggers no harvest and no engine work. Compaction then does two independent things:

1. **Trims the transcript**, as today.
2. **Asks the model for a fresh prose summary** — a periodic prompt rewrite. On a long run the prompt drifts; this pulls it back.

The difference from today is what happens when that call fails or returns empty: the run continues on the tree with a notice, instead of falling back to blind trimming. The tree, not the transcript, is what carries the model's picture of its own work across a compaction. That is the point of the design and also its risk, which is why the recorder is deterministic and never a model call.

## 8. The command surface

One `task` tool, JSON actions, forgiving parsing in the existing style (`tools.ParseArgs`).

| action | arguments | effect |
|---|---|---|
| `plan` | `text`, `steps[]` | create a task and its children in one call |
| `add` | `text`, `parent?` | create a node; returns its id |
| `status` | `id`, `status`, `reason?` | `doing` \| `done` \| `blocked` \| `dropped` |
| `note` | `text`, `id?`, `file?`, `decision?`, `keep?` | record a note against a node |
| `show` | `id?` | render a node, a branch, or the whole tree |

`plan` exists because small local models do badly at chatty multi-call setup and already know this shape. `show` is the query side and the escape hatch when a report has condensed to a pointer.

Human commands: `/task` renders the tree, `/task show <id>` one branch, `/task open` prints the path to the Markdown document. `engine.tools: full|minimal` keeps working: `minimal` registers `task` alone and drops the git lookups.

## 9. Native Ollama

For `type: ollama`, native `/api/chat` becomes the normal path for every request: streaming, tool calls, and an options block the harness controls (`num_ctx`, `keep_alive`, and a passthrough map). The OpenAI path remains for `type: openai` and as the fallback when a native call is rejected, so an older server still works.

### 9.1 Config

```jsonc
"providers": {
  "lan": {
    "type": "ollama",
    "base_url": "http://192.168.1.150:11434",
    "context_window": 32768,          // num_ctx the harness sends
    "keep_alive": "30m",
    "options": { "temperature": 0.6 } // passthrough
  }
},
"models": {
  "hf.co/unsloth/Qwen3.8-27B-GGUF:UD-Q4_K_XL": {
    "context_window": 32768,
    "keep_alive": "30m",
    "options": { "top_k": 40 }
  }
}
```

`context_tokens` stops being a number to guess: unset, it is derived from the window minus the generation reserve. Set, it still wins, so existing configs keep behaving.

### 9.2 Resolution order

Per model: the `models` entry, else the provider block, else a probe. **An explicit window means no probe** — no `/api/ps` read, no Modelfile parse, and above all no loading the model to read its window, which is the multi-minute startup stall in the current code.

### 9.3 Consent, because the server is shared

Sending a `num_ctx` that differs from how the model is currently loaded **makes Ollama reload it**, evicting other applications on that box. So it is a server change and goes through the same gate as an explicit reload:

```
model <name> is loaded with an 8192-token window; config asks for 32768
[r] reload it at 32768   [k] keep 8192 this session   [a] always for this model
```

`a` persists `ollama.reload_on_mismatch: always`. On `k` the harness runs inside the server's window and clamps its budget, which is today's behaviour and safe on a shared box. **A non-interactive or headless run never prompts**: it keeps what is loaded, clamps, and prints the reason.

When the existing backend-status check (`internal/agent/resilience.go`) sees a window change the harness did not cause, another client reloaded the model. The harness adapts — reports it, re-derives the budget, carries on — and never reloads back. A reload war between two clients on a shared server is the worst outcome available.

A model whose hard ceiling is below the configured window cannot be talked into more: the harness reports the ceiling and clamps once, rather than fighting every request.

## 10. Model switching

`SetModel` refreshes the profile and the reserve today, but nothing re-reads the window, so a switch to a smaller model leaves the budget too large and the server truncates silently.

On a switch:

1. The model changes immediately; the profile applies as today.
2. The loader resolves that model's parameters (9.2) **off the UI thread**, so `/model` returns at once and the new window lands as a notice.
3. Window, budget and reserve are re-derived; `num_ctx` goes out with the next request, under the consent gate of 9.3.

A switch never loads a model just to read its window. If the window is unknown, the harness sends its configured default and reconciles from what the first real response reports.

### 10.1 The loader is the only path

Every request for a model goes through the loader, which applies that model's resolved parameters including `num_ctx`: startup, `/model`, a pick from `/models`, a warm, and a recovery after the backend-status check trips. Nothing else sends options or loads a model, so there is one place where a model's parameters are decided and one place to look when they are wrong.

When the protection trips, what the loader does depends on which trip it was:

- **Evicted or idle-expired** — the model is not resident, so the next request reloads it regardless. The loader supplies the resolved `num_ctx` for that reload. No other application loses anything, because nothing was holding it, so this needs no consent.
- **Window changed underneath us** — another client reloaded the model at a different size. The loader adapts: re-derive the budget, notice it, carry on. It re-applies our `num_ctx` only where consent already exists for that model (`reload_on_mismatch: always`), and otherwise leaves the other client's model alone.

If the new model's window is smaller than what the conversation currently occupies, the harness compacts once on the spot rather than letting the next request truncate, and says so.

`/models` shows what is being chosen between: parameter size and quantization from the tag listing, window, and whether the model is resident.

## 11. Failure modes

| failure | behaviour |
|---|---|
| store will not open | `task` registered over a no-op ledger; one `warn:` line; session continues |
| recorder panic | fenced, reported once as a notice |
| task document unparseable | quarantined, rebuilt from dotdir state |
| dotdir state lost | tree intact from Markdown; verbatim buffer lost |
| summary call empty or failing | continue on the tree with a notice |
| native call rejected | fall back to the OpenAI path for the session, once, with a notice |
| window mismatch declined | clamp the budget, run inside the server's window |
| model switch to a smaller window | compact once immediately, notice |

## 12. Testing

1. **Compaction keeps the thread** — an end-to-end run against the scripted mock backend that compacts mid-task and demonstrates the model continuing from the tree, with assertions anchored on the tree's own content, not on words the prompt always contains.
2. **Native Ollama** — a fake Ollama server proving streaming, tool calls and `num_ctx`; one manual run against the LAN box.
3. **Consent** — no server-changing call without a yes; non-interactive never prompts.
4. **Hand-edited documents round-trip** — user edits survive, a broken document quarantines rather than destroying work.
5. **Model switching** — window, budget and reserve follow the model; the UI does not block; a smaller window compacts once.
6. **Migration** — a 0.10.0 store lifts into a tree with nothing discarded.
7. **Advisory discipline** — a panicking, hanging and lying engine each leave the session working.

## 13. Open questions

None. Decisions taken during design: full rewrite rather than an additive layer; unbounded depth; continuous recording rather than threshold harvest; summary call retained as a drift-control rewrite; JSON actions rather than a command string; reports inline with a condensation ladder; Markdown in the workspace, untracked by default; consent before any server change.
