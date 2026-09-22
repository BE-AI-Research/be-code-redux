# Sub-Agents on the Task Engine — Design

**Date:** 2026-09-22
**Status:** approved (design sections reviewed in conversation)
**Builds on:** the task engine (`internal/engine`, 0.10.0–0.13.0), co-working
(`internal/agent/cowork.go`, `CoworkerFactory`, `consultAgent`, 0.9.0–1.0.1), the
approval seam (`tools.ApproveFunc`, `review.Coordinator`), the mid-task queue
(`Agent.Enqueue`), shared asks and per-terminal rendering (0.8.0)
**Target version:** 1.1.0

---

## Problem

Co-working (1.0.x) lets the primary model *ask* a bigger or different model for
advice, but the primary still has to do every step itself. A small local model
working a long task is slowest and least reliable on the steps that are most
mechanical — porting a module, writing the tests for a finished function,
filling in a documented interface — and those are exactly the steps a second
model could own outright while the primary carries on.

The owner's sketch (2026-09-22): task nodes carry an owner tag — `(01) M` the
main local model, `(02) O-x` a local co-worker, `(03) O~x` an online co-worker,
later the grid models — each assignee has a model card with a **confine** rule,
and a sub-agent owns a *subtree* of the task tree, not a message. The grid
("a way to connect remote environments to a host, allowing that host to expand
its own workspace and capabilities") is a later feature; this design shapes
its records so the hand-off can travel later without being redesigned.

Chosen approach (C): sub-agents are in-process scratch agents now, built the
way `consultAgent` is built, given a *write-capable but confined* registry
and one subtree of the shared task tree. The dispatch and the hand-back are
plain records, so a mailbox transport can carry them to a remote grid host
later.

## Goals

- The main model, or the operator, can assign a step of the task tree to a
  configured co-worker, which then does that step on its own — reading,
  writing inside its scope, running the project's own checks — while the main
  model continues.
- A sub-agent can never write outside the scope it was given, run an
  arbitrary command, consult anyone, or reach an editor or MCP server.
- The main model is told when a sub-agent finishes or asks a question, through
  the queue it already reads, and still runs the request's verification itself.
- Everything a sub-agent did is filed on its own nodes, in the same document
  the operator already reads, marked with who did it.
- File writes by a sub-agent are approved exactly as the main model's are:
  the modal in approve mode, nothing under auto-approve.
- Several sub-agents may run at once, but one server is never asked to serve
  two requests at once.
- Nothing changes for a configuration with no `sub_agent: true` co-worker.

## Non-goals

- The grid transport: dispatching to a remote host, a remote host's
  workspace, or any network protocol. The records are shaped for it; nothing
  sends them.
- Git worktrees or branches per sub-agent. Sub-agents write into the one
  working tree, inside their scope; the checkpointer and the task document are
  the record.
- Sub-agents dispatching sub-agents (depth is one), consulting co-workers, or
  reviewing each other's work.
- The "settings auto-tune" feature the owner mentioned; it is next, not this.
- A sub-agent running the verification loop. Verification stays the main
  model's, once per request, as today.

---

## 1. The record: owner and scope on a node

### 1.1 Task document

A node line gains two optional trailing fields, and a third for ordering:

```markdown
- [ ] 3. port the scanner to the new tokenizer
  - [x] 3.1. list the call sites  @main
  - [ ] 3.2. port internal/scan  @big  scope: internal/scan, internal/scan_test.go
    - [ ] 3.2.1. replace the token loop
    - [ ] 3.2.2. update the tests
  - [ ] 3.3. write the migration note  @claude!  scope: docs/scanner.md  after: 3.2
```

- `@<name>` names the owner: `main` (the primary — the default when the tag is
  absent, and never written for it), or a co-worker's `name`. A trailing `!`
  (`@claude!`) means the **operator** assigned it; the model may not change or
  remove an operator assignment, and the `task` tool refuses the attempt with
  `3.3 was assigned by the operator; ask them to change it`.
- `scope:` is a comma-separated list of workspace-relative paths (directories
  or files). A sub-agent may write only under them. A node with an owner other
  than `main` and no scope is **not ready** (§2.1); the document is still valid.
- `after:` lists node ids that must be closed before this one is ready,
  replacing the positional rule (§2.1). It is rare; the default order is the
  document's order.
- Children of an assigned node inherit the owner and scope and carry no tag of
  their own. A child tagged with a different owner is a parse warning and the
  parent's owner wins — one subtree, one pen.

Rendering and parsing live in `internal/engine/markdown.go` beside `nodeLine`
(the fields are matched off the end of the text, so `@` and `scope:` inside
ordinary task text stay text — they are recognised only as whole trailing
tokens separated by two or more spaces). A hand-edited assignment is the
operator's intent, exactly as a hand-edited status is: the document wins on
load.

### 1.2 Node

```go
type Node struct {
    // …existing fields…
    Owner       string   `json:"owner,omitempty"`        // "" means main
    OwnerPinned bool     `json:"owner_pinned,omitempty"` // "@name!": set by the operator
    Scope       []string `json:"scope,omitempty"`
    After       []string `json:"after,omitempty"`
    DoneBy      string   `json:"done_by,omitempty"`      // who closed it, when not main
}
```

`DoneBy` is stamped by the sub-agent runner when it closes a node, so a report
line reads `3.2 port internal/scan — done by big (14m, 22 tool calls)`; the
`(18m, 31 tool calls)` suffix on closed nodes already exists and gains the
`by <name>` prefix only when `DoneBy` is set. Nothing about a node the main
model does changes.

### 1.3 One doing node per pen

Today exactly one node is `doing`. A dispatched subtree needs its own: the
sub-agent moves through `3.2.1`, `3.2.2` as the main model moves through its
own steps. The invariant becomes **one doing node per owner**: the main
model's, plus at most one inside each subtree that is currently dispatched.
`SetStatus(doing)` sends the previous doing node *of the same owner* back to
`todo` (distilled first), exactly as it does for the one pen today, so a
dispatched root is `todo` while the sub-agent works its children and the
active branch under that owner is the doing child plus its ancestors; the
scheduler's running set, not a status, records that the root is dispatched.
`ParseDoc`'s two-`[>]` warning fires only for two doing nodes under the same
owner. The `unfiled` node the store opens for evidence with nothing doing is
per owner too, opened under the sub-agent's root node, never the main's.

### 1.4 Co-worker card

```json
"coworkers": [
  {"name": "big", "provider": "ollama-lan", "model": "qwen3:32b",
   "skills": "long mechanical ports", "sub_agent": true},
  {"name": "claude", "provider": "anthropic", "model": "claude-opus-5",
   "online": true, "sub_agent": true, "max_scope": ["docs", "README.md"]}
]
```

- `sub_agent: true` makes a co-worker assignable. Without it, `@name` on a
  node is a parse warning (`big is not a sub-agent; add "sub_agent": true to
  its coworkers entry`) and the node is not ready. Consultation is unchanged.
- `max_scope` (optional) is the widest scope that co-worker may ever be given.
  A `scope:` not entirely inside it is refused at assignment time by the
  `task` tool and `/task scope` (`claude may only own paths under docs,
  README.md`), and a hand-edited one is a load warning that leaves the node
  not ready. Empty means the whole workspace. `ValidCoworkers` cleans the
  paths and drops entries that escape the workspace, with the existing
  `warn:` line.

`/agents` (§3.4) is the model card the sketch drew: it shows each entry's
model, `local`/`online`, whether it may be a sub-agent, its `max_scope`, and
what it is doing.

### 1.5 Evidence

The sub-agent's tool calls are recorded on *its* nodes through the same
`recorder`, keyed by owner, with the same `item_cap`/`node_cap` bounds and the
same `distill` on close. The main model's `Working memory:` block renders a
dispatched subtree as one line under the active branch —
`3.2 port internal/scan  @big  running (at 3.2.1, 9 tool calls)` — never the
sub-agent's raw buffer: that buffer is the sub-agent's context, not the
main's, and it would cost the main's window for nothing. After the hand-back
the subtree renders as any closed subtree does, condensed by the same ladder.

### 1.6 The dispatch and hand-back records

Both live in `internal/subagent` as plain structs with no dependency on
`agent`, `tui` or `engine` beyond ids and strings — the shape a mailbox can
carry later:

```go
type Dispatch struct {
    Session   string   // session id, so a remote answer can be routed back
    Node      string   // "3.2"
    Owner     string   // "big"
    Text      string   // the node's text
    Children  []string // "3.2.1 replace the token loop", …
    Scope     []string
    Checks    []string // the shell commands it may run (§2.4)
    Context   string   // parent task text, project notes, notes.md — bounded
    MaxTurns  int
    Interrupted bool   // re-dispatched after a session ended mid-step (§3.6)
    Touched   []string // files its earlier attempt wrote, when Interrupted
}

type HandBack struct {
    Node     string
    Owner    string
    Status   string        // done | blocked
    Reason   string        // for blocked
    Summary  string        // the sub-agent's closing message, sanitised
    Files    []string      // files written
    Elapsed  time.Duration
    Calls    int
}

type Ask struct {   // one question, one answer
    Node, Owner, Question string
}
```

`Context` is bounded the way a consultation's seed is: the parent task's
text and status line, the project notes as the main model has them (already
trimmed to `MaxProjectNotes`), and `notes.md`. No transcript, ever: the
sub-agent's knowledge is the tree and the repository, which is what makes the
record portable.

---

## 2. The scheduler

### 2.1 Ready

A node is **ready** when all of these hold:

1. its owner is a co-worker with `sub_agent: true` that survived
   `ValidCoworkers`;
2. it has a non-empty scope, inside the owner's `max_scope` and inside the
   workspace;
3. its scope does not overlap the scope of another assigned node that is
   dispatched or ready (one pen per path; the later one in document order
   waits, and `/agents` says why);
4. its status is `todo` and it is not in the scheduler's running set (a
   dispatched root is `todo` while its child is `doing`, §1.3);
5. every node named in `after:` is closed (`done` or `dropped`), or, with no
   `after:`, every earlier sibling is closed;
6. it is not itself inside an assigned subtree (only the top of a subtree is
   dispatched; its children are the sub-agent's own steps).

Readiness is re-evaluated whenever the tree changes: after every `task` tool
action, every `/task` command, a document reload, a hand-back, and at
resume. The evaluation is a pure function of the tree and the config
(`subagent.Ready(tree, cards) []Candidate`), in id order.

### 2.2 Lanes

A **lane** is one server: the scheme, host and port of the provider entry's
`base_url`, so two provider entries pointing at the same Ollama share a lane. Every co-worker
and the primary itself map to a lane. A lane serialises **model calls**, not
whole runs: the primary takes its own lane around each request to its server,
a sub-agent on the same lane takes it around each of its own, and between
model calls (tool dispatch, an approval that is waiting, an `ask_main`)
the lane is free. When a lane frees and both the primary and a sub-agent are
waiting, **the primary goes first** — the person is watching that one.

This is the owner's rule for a server that cannot serve two: "handle each
thing as an individual task as to not overload the system". A sub-agent on
a *different* server runs fully in parallel with the primary. Grid nodes,
later, are further lanes.

`sub_agents.max_concurrent` (default 2) caps how many sub-agents are running
at once across all lanes. A ready node past the cap waits in id order.

**Measured cost, stated plainly.** On the owner's server the prompt cache is
reusable only when a request strictly extends the previous one on that
server (0.14.0). A sub-agent sharing the primary's server interleaves its
requests with the primary's, so the primary's next request after an
interleave is a cold read (measured 43 s for 19k tokens, against 0.3 s warm),
and so is the sub-agent's. The lane rule bounds the damage (one request at a
time, primary first) but does not remove it; `max_concurrent` and a second
server do. The startup line already printed for a same-server co-worker
(`noteSharedServer`, 1.0.1) gains a clause: `sub-agents on this server
interleave with the primary and cost it a cold prompt read per hand-over`.
Nothing here is changed for the prompt layout: each agent's own history stays
append-only.

### 2.3 Dispatch

Dispatching a ready node:

1. build the `Dispatch` record (§1.6);
2. build the sub-agent: a scratch `Agent` on the co-worker's provider through
   `CoworkerFactory` (which returns its window; the scratch agent budgets
   against that window and compacts on its own, as `consultAgent` does since
   1.0.1), system prompt `SubAgentFrame` (§2.5), tools from §2.4, `max_turns`
   = `sub_agents.max_turns` (default 40), the primary's `retryBase`/`stallAfter`
   and `OnTransient` forwarded;
3. mark the node `doing` **for that owner** (§1.3) with `Started` set;
4. run it on its own goroutine, fenced against panics exactly as `Consult` is,
   under a context derived from the session's *root* context — never the
   turn's, so Esc on the main run does not cancel it (§3.5);
5. on return, close the node (§2.6) and enqueue the hand-back.

The same-server, same-model case (a sub-agent card naming the primary's own
model on the primary's server) sends the window the primary already holds and
never reloads. A different model on the same server goes through the loader
with no approver, as a consultation does today — it never sends a differing
window; the notice from 1.0.1 covers it.

An **online** sub-agent (`online: true`, or forced online by `ValidCoworkers`)
asks consent once per session through the existing seam,
`Tools.Approve("consult", detail)`, before its first dispatch, with the
detail naming the scope: `send code under internal/scan to claude
(claude-opus-5, online)?`. `-y` allows through `AutoApproveConsult`, as a
consultation does. A refusal marks the node `blocked` with reason `consent
refused` and hands back; the refusal is remembered for the session, as
`AllowCoworker`'s opposite already is.

### 2.4 The sub-agent's tools

`Registry.Scoped(scope []string, checks []string)` yields a registry that:

- **reads anywhere** in the workspace (`read_file`, `list_dir`, `search`,
  and whichever of `lookup`, `history`, `show`, `changes` the session
  registered — the read side of the git tools);
- **writes only under `scope`** (`write_file`, `edit_file`): a path outside is
  refused with `path is outside your scope (internal/scan,
  internal/scan_test.go); use ask_main if you need it widened` — a tool
  error, never a prompt;
- runs **`shell` only for the project's detected checks**: the exact commands
  `verify.Detect` produced for the workspace (and nothing else — not a check
  plus flags, not a compound line), refused otherwise with `only the
  project's checks may be run: go vet ./..., go build ./..., go test ./...`;
  timeouts and process groups as today;
- offers **`ask_main`** (§2.7) and the **`task`** tool restricted to its own
  subtree (`add`, `status`, `note`, `show` under nodes whose id begins with
  its root's; `plan`, `owner`, `scope` and `reply` refused);
- has **no** `process`, `consult`, `web_*`, `ide_*` or other MCP tool.

`Approve`, `OnBeforeWrite`, `ReviewWrite` and `ReviewInvolvesEditor` are the
main registry's own, so a write is approved and checkpointed the way the
main model's is (§3.3). Confinement reuses `Registry.Root`'s cleaned-`Rel`
check with a second check against `scope`; there is no new path logic.

### 2.5 The sub-agent's prompt

`SubAgentFrame`, in `internal/agent`, modelled on `ConsultFrame` and kept
short (prompt guidance moves a local model more than mechanism does):

> You are a sub-agent working one step of a larger task for a main model that
> owns the whole task. Your step, its sub-steps, and the files you may change
> are listed below. Do the step completely: read what you need anywhere in
> the repository, change only files inside your scope, run the project's
> checks when you have changed code, and mark each sub-step done as you finish
> it. If you need a file outside your scope, a decision you cannot make, or
> information only the main model has, use ask_main once with a precise
> question and wait. When the step is done, reply with a short summary of what
> you changed and anything the main model must know. Do not restate the task.

Followed by the `Dispatch` record rendered as Markdown (the step, its children
as a checklist, the scope, the checks it may run, the bounded context) and,
when `Interrupted`, `This step was interrupted earlier; these files were
already written: …` so it reads them before writing again.

The **main model** gets one short block, `subAgentGuidance`, appended to
`taskGuidance` only when at least one co-worker is a sub-agent:

> Sub-agents: a step you assign with an owner and a scope is done by that
> model on its own; you are told in a later message when it finishes or asks
> something. Assign whole steps with a clear file scope, do not edit inside a
> running sub-agent's scope, and answer its questions with task reply.

### 2.6 Hand-back

When the sub-agent's run returns:

- **done**: its closing message, sanitised by `sanitizeAdvice`, becomes
  `HandBack.Summary`; the node and any open children are closed `done` with
  `DoneBy` stamped and the buffer distilled;
- **blocked**: turn cap reached, a tool error the sub-agent could not get
  past (its own `blocked` status), an `ask_main` that timed out, a backend
  error after `chatWithRetry` gave up, a consent refusal, a stop — the node is
  `blocked` with the reason, its children left as they were;
- a **cancelled** run (session end, `/agents stop`) is §3.5/§3.6, not a
  hand-back.

The hand-back is delivered to the main model's queue (`Agent.Enqueue`) as one
line plus the summary:

```
sub-agent big finished 3.2 (done, 14m, 22 tool calls; wrote internal/scan/token.go, internal/scan/scan_test.go):
<summary>
```

or `sub-agent big stopped on 3.2 (blocked: turn cap of 40 reached): …`. The
queue already lands it after the tool results the main model was waiting on,
and if the main model is idle the existing leftover-queue rule starts a turn
with it — the main model then verifies (`RunFull`'s loop runs on that request
as on any other), marks the parent, and carries on. The line is also posted
to the transcript in the sub-agent's voice (§3.2), so every terminal sees it
whether or not the main model is mid-run.

### 2.7 `ask_main`

`ask_main(question)` parks the sub-agent: its goroutine blocks on a reply
channel, its lane is released, and `Ask` goes to the main model's queue as
`sub-agent big asks about 3.2: <question>` and to the transcript as
`big (3.2)? <question>`. Three things resolve it:

- the main model: `task action: reply, id: 3.2, text: …` — the text becomes
  the tool result of `ask_main` and the sub-agent continues; `task action:
  scope, id: 3.2, paths: …` widens the scope (inside `max_scope`) and
  resolves the ask with `scope widened to …`; `task action: owner` to another
  owner or `main` stops the sub-agent and re-queues the node (§3.5);
- the operator: `/task reply 3.2 <text>`, `/task scope 3.2 <paths>`, `/task
  assign 3.2 <owner>` — the same three;
- `sub_agents.ask_timeout` (default 600 s): the node is `blocked` with reason
  `no answer to: <question>` and hands back.

A sub-agent may have one ask open at a time; the tool refuses a second. The
question and its answer are recorded on the node as a `note` evidence line,
so the report shows them.

---

## 3. What the operator sees and controls

### 3.1 The document

Exactly §1.1. The operator can assign, scope and order by editing the file;
the engine reloads it as it does today, and a hand-edited `@name!` is the
pinned form the `task` tool will not touch.

### 3.2 The transcript

The sub-agent has a voice in every attached terminal, in the theme's `Cowork`
colour, using the co-working entry kinds with the node in the label:

- `big (3.2)> ` — the hand-back summary (Markdown), and a one-line
  `started 3.2 port internal/scan` when it begins;
- `big (3.2)? ` — an `ask_main` question;
- an answer from the operator or the main model is shown as `reply to big
  (3.2): …`.

Tool calls, diffs and intermediate text are **not** streamed to the
transcript; the sub-agent's work is in the document and in the files. Plain
mode prints the same lines through `ui.Events()`; headless `run` prints them
and `run --json` carries `sub_agents: [{node, owner, status, elapsed,
calls}]`.

### 3.3 Approvals

The owner's ruling: sub-agent file writes follow the main model's approval
schema exactly. The scoped registry shares the main registry's `Approve`, so

- in approve mode, a sub-agent write raises the same shared modal
  (`internal/tui/ask.go`), labelled `big (3.2) · write_file internal/scan/token.go`
  with the diff preview, answerable from any terminal; `a` there means what
  it means for the main model — all file changes this session, for everyone;
- under `approve_file_writes: true` or `-y`, nothing is asked after the
  initial online consent (§2.3);
- the editor review (`ReviewWrite`) applies as configured, so `both` races
  the editor and the modal for a sub-agent's write too;
- shell never prompts for a sub-agent: a check on the allow-list runs, anything
  else is refused (§2.4).

A sub-agent write joins the current checkpoint turn (`OnBeforeWrite`), so
`/undo` and the restore point cover it like any other write.

### 3.4 Commands

| Command | Effect |
|---|---|
| `/task assign <id> <owner\|main>` | pins the owner (`@name!`); `main` unassigns; refused for a dispatched node unless it is parked on an ask (§2.7) |
| `/task scope <id> <path>[, <path>…]` | sets the scope; validated against `max_scope`, the workspace, and overlap (§2.1) |
| `/task reply <id> <text>` | answers an open `ask_main` |
| `/agents` | the model cards: name, model, `local`/`online`, `sub-agent`/`consult only`, `max_scope`, and the current state — `idle`, `3.2.2 (9 tool calls, 4m)`, `waiting for lane`, `asking: …`, `waiting: scope overlaps 3.4` |
| `/agents stop <name>` | cancels that sub-agent; its node is `blocked` with reason `stopped by operator` and hands back so the main model knows |

All are busy-safe (they change the tree and the scheduler, not the main run)
and live in `ui.SlashCommandTable` so the palette, `/menu` and plain mode
stay in sync. The `task` tool grows the matching `owner`, `scope` and
`reply` actions for the main model (the enum in `internal/tools/task.go`).

The bottom line shows `⚙ big 3.2.2` while one sub-agent runs and `⚙ 2 lanes`
for more; the header is unchanged.

### 3.5 Cancel and stop

- **Esc** cancels the main model's run and the consultation it may be waiting
  on, as today — and **not** the sub-agents. They were dispatched from the
  session's root context (§2.3) and keep working; the bottom line still shows
  them.
- `/agents stop <name>` cancels one (§3.4). Reassigning a running node
  (`/task assign`, `task action: owner`) is refused unless the sub-agent is
  parked on an ask; then it is stopped, the node returns to `todo` under the
  new owner, and the scheduler re-evaluates.
- **Session end** (`/quit`, the host quitting, `sessions kill`) cancels every
  sub-agent: each running node is set back to `todo` with a `note` line
  `interrupted <time> after N tool calls; files written: …` and the store
  is flushed before the session saves. Nothing is left `doing` under a
  sub-agent in a saved session.

### 3.6 Resume

The owner's ruling: re-dispatch silently, and any failure surfaces. On
`--resume` / `/resume`, once the store is open and the session restored, the
scheduler runs its ordinary evaluation: every assigned `todo` node that is
ready is dispatched with `Interrupted: true` and its earlier `Touched` files
when the note from §3.5 is present, with no prompt and one transcript line
per node (`big resumed 3.2 port internal/scan`). Online consent is asked again
— it is per session — before the first online dispatch.

A node that **cannot** be re-dispatched is not left quiet: the owner missing
from the config or no longer `sub_agent: true`, its scope outside a changed
`max_scope`, a provider that fails to build, a consent refused — the node is
`blocked` with the reason, a hand-back goes to the main model's queue, and
the terminal prints a notice (`sub-agent big cannot resume 3.2: big is not in
coworkers`) so whichever of the operator or the main model acts first can
reassign it. A sub-agent that starts and then fails (backend down, turn cap)
takes the ordinary `blocked` hand-back of §2.6.

### 3.7 Errors, in one place

| Situation | What happens |
|---|---|
| write outside scope, shell not on the allow-list | tool error naming the scope or the list; never a prompt |
| `max_scope` violated at assignment | `task` tool / command error; document warning on load, node not ready |
| scope overlaps another assigned subtree | later node waits; `/agents` explains |
| owner not a sub-agent | parse warning, node not ready |
| turn cap, backend failure, timed-out ask, consent refused, operator stop | node `blocked` with reason; hand-back |
| panic inside a sub-agent | fenced (as `Consult`); node `blocked` with `internal error: …`; hand-back |
| session ends mid-step | node back to `todo` with the interruption note; re-dispatched on resume |
| re-dispatch impossible | node `blocked`; hand-back; terminal notice |
| main model writes inside a running sub-agent's scope | allowed, footer on the result: `note: internal/scan is owned by big (3.2) until it hands back` |

Nothing here fails the main run, ends the session, or deletes a document.

---

## 4. Configuration, testing, boundaries

### 4.1 Configuration

```json
"coworkers": [
  {"name": "big", "provider": "ollama-lan", "model": "qwen3:32b", "sub_agent": true},
  {"name": "claude", "provider": "anthropic", "model": "claude-opus-5",
   "online": true, "sub_agent": true, "max_scope": ["docs"]}
],
"sub_agents": {
  "max_concurrent": 2,
  "max_turns": 40,
  "ask_timeout": 600
}
```

- `coworkers[].sub_agent` (bool, default false) and `coworkers[].max_scope`
  ([]string, default empty = whole workspace).
- `sub_agents.max_concurrent` (2), `sub_agents.max_turns` (40),
  `sub_agents.ask_timeout` (600 s). Zero or missing takes the default;
  `max_concurrent` below 1 is 1, so a configuration cannot silently disable
  dispatch of a node the operator assigned (they assign it, it runs).

Defaults in `config.Default`, documented in the README config reference, and
`ValidCoworkers` extended as §1.4 says.

### 4.2 Code layout

- `internal/subagent` — `Dispatch`, `HandBack`, `Ask`; `Ready(tree, cards)`;
  `Lanes` (per-server mutex with primary priority); the scheduler's
  bookkeeping (running set, cap, id-order queue). No import of `agent`,
  `tui` or `provider`; tested with scripted trees and fake runners.
- `internal/engine` — the node fields, parse/render, per-owner doing and
  unfiled, `DoneBy` in `statusLine`, the one-line render of a dispatched
  subtree.
- `internal/tools` — `Registry.Scoped`, the `ask_main` tool, the `task`
  tool's `owner`/`scope`/`reply` actions and subtree restriction.
- `internal/agent` — `subagents.go`: the runner (scratch agent as §2.3),
  `SubAgentFrame`, `subAgentGuidance`, the hand-back enqueue, `Events`
  (`OnSubAgentStart`, `OnSubAgentAsk`, `OnSubAgentEnd`); `SubAgentFactory`
  is not needed — `CoworkerFactory` already builds the provider.
- `internal/tui`, `internal/ui`, `cmd/` — commands, transcript lines, bottom
  line, `/agents`, wiring in `buildAgent` after `attachEngine`, cancel on
  session end, the resume pass.

### 4.3 Testing

- **`internal/subagent`** against scripted trees and fake runners: the ready
  rule (each of the six conditions, `after:` against the positional rule,
  overlap), lane serialisation (two same-lane requests never overlap, two
  lanes do, the primary goes first when both wait), the cap, id order, the
  `ask` round trip and its timeout, the interrupted re-dispatch carrying
  `Touched`, stop.
- **`internal/engine`**: `@owner`, `@owner!`, `scope:`, `after:` render and
  parse round trip; `@` and `scope:` inside ordinary text untouched; a
  hand-edited assignment wins on reload; per-owner doing and the relaxed
  two-`[>]` warning; `done by` on reports; the dispatched-subtree line.
- **`internal/tools`**: `Scoped` refuses a write outside scope and a shell line
  not exactly a detected check, allows reads anywhere; `ask_main` refuses a
  second open ask; `task` refuses `owner` on a pinned node, `scope` outside
  `max_scope`, and any action outside the sub-agent's subtree.
- **`internal/agent`**: a scripted co-worker provider owns one node to `done`
  with the hand-back queued and delivered before the main model's next call;
  a blocked hand-back; the panic fence; the online consent detail names the
  scope; a write approval reaches the registry's `Approve` with the
  sub-agent's label; Esc leaves the sub-agent running; session end resets the
  node to `todo` with the note.
- **`internal/tui`/`ui`**: the three `/task` commands and `/agents`, the
  transcript lines in both UIs, the bottom line.
- **e2e** (`test/e2e`): the scripted mock gains a second endpoint used as a
  sub-agent; a plan with one `@sub` step runs to `done by`, a write outside
  scope produces an `ask_main` that the main model answers with `reply`, and
  the request's verification still runs on the main model afterwards.
- **Live checklist** (`docs/live-checklist.md`, on the VM): a small local
  co-worker (4–8B beside the 27B on .150) owns one step of a real task; a
  same-server card naming the primary's own model shows no reload in
  `/api/ps`; an online sub-agent shows the consent modal naming its scope;
  `/quit` mid-step and `--resume` re-dispatches with the interruption note.

### 4.4 What this design deliberately leaves out

Named so a later reader knows they were seen: the grid transport and remote
workspaces; worktrees per sub-agent; depth greater than one; sub-agents
consulting or reviewing; per-sub-agent verification; streaming a sub-agent's
tool calls to the transcript; a scheduler that reorders by estimated cost;
prefilling the primary's prompt after a lane hand-over (a measured
improvement to make once the lane cost is observed on a real session, not
guessed at); and settings auto-tune.
