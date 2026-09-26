# Scheduled events — design

**Status:** approved in brainstorming, 2026-09-26. Target release: 1.2.0. Branch
`feature/schedules` off `main`.

## Purpose

Let a session that is left running — a hosted session with nobody attached, for hours or days —
wake the model at a chosen time to do a pre-decided piece of work. A person sets a schedule
("every weekday at 09:00, pull, run the tests and summarise what broke"), or the model sets one
for itself during a task ("check back on this build in 20 minutes"). When it comes due, the
model gets a turn with the instruction it was given.

Schedules are not only for code: an instruction is free text and an allowance can name any
tool, so the same mechanism serves the general-purpose task engine planned after this.

## Decisions taken in brainstorming

| Question | Decision | Why |
| --- | --- | --- |
| What are schedules for? | **All of it:** one-off follow-ups (mostly set by the model), recurring upkeep (mostly set by a person), unattended agent work. | The most versatile system; the three differ only in who sets them and what they may do. |
| When do they run? | **Only while a session for that workspace is running.** No system cron, no OS hooks. | System hooks are a later question. A missed time never starts BE-Code. |
| What may a fired event do unattended? | **An allowance travels with the schedule**, approved by a person up front; anything outside it asks as usual. | The only option under which unattended work gets done while every grant still comes from a person. |
| When is consent given? | **Up front, in one prompt:** at creation, and again for every saved schedule when a session starts. | Nobody waits for a first edit to be asked; nothing fires before a person has seen what will. |
| Where do schedules live? | **Recurring ones in the project** (`.be-code/schedules.md`, readable and hand-editable); **one-off timers in the session.** | "Every morning" is a property of the project and survives `--new`; "check the build in 20m" means nothing outside the task that set it. |
| Missed times | **A recurring schedule runs at most once** on startup if its last due time was missed; a one-off whose time passed runs once. | Catching up every missed occurrence is never what anyone wants after a weekend off. |
| Due while busy? | **Queued**, never interrupting, always its own turn. | Reuses the hardened queue; one conversation, one pen. |
| Architecture | **A `schedule` package beside the engine, driven by the session** — not schedules as task nodes, not a machine-wide service. | Task-node ids are positional and nodes finish; a recurring job never does. A service breaks "only while a session runs". |

## §1 Parts and data

### 1.1 `internal/schedule` — a leaf package

Stdlib only. It knows nothing of the agent, the tools or the UI.

- **`Spec`** — a parsed time. Accepted forms (all local time):
  - `in 20m`, `in 2h`, `in 1h30m` — one-off, relative to creation.
  - `at 09:00` (the next 09:00), `at 2026-09-27 09:00` — one-off, absolute.
  - `every 30m`, `every 2h` — recurring interval on a fixed grid from creation (`created + k·interval`), so a slow run never drifts the times.
  - `daily 09:00`, `weekdays 09:00`, `mon,thu 14:30` — recurring by clock.
  - five-field cron (`0 9 * * 1-5`) — recurring, for whoever wants it.

  `Parse(s string, now time.Time) (Spec, error)`; `Spec.Next(after time.Time) (time.Time, bool)`
  (false for a spent one-off); `Spec.Recurring() bool`; `Spec.String()` gives back the canonical
  form. One-off versus recurring is decided by the form alone; there is no separate flag. A
  recurring spec whose shortest gap is under `schedules.min_interval` is refused at parse
  validation with the minimum named. Local clock times that do not exist (spring-forward) run at
  the first instant after the gap; ones that occur twice (fall-back) run once, at the first.
- **`Allowance`** — a flat list of grants, each written `kind: value`:
  - `shell: <glob>` — matched exactly the way `shell_allow` is (a simple command only; anything
    compound still asks). A bare `*` is refused.
  - `write: <path prefix>` — workspace-relative, cleaned, confined like every file tool. `.` means
    the whole workspace and is allowed, flagged as broad (§3.4).
  - `browser: <host>` — the same host matching as the browser tool's `allow` tier.

  An empty allowance is valid: the event can read, and anything else asks.
- **`Schedule`** — `ID` (short, stable, never positional), `Name` (unique per store, `[a-z0-9-]`),
  `Spec`, `Instruction`, `Task` (optional task-node id), `Allow Allowance`, `State`
  (`active` / `paused` / `done`), `CreatedBy` (`person` / `agent`), `Created`, `LastRun`,
  `LastOutcome`, `Failures` (consecutive), `AskTimeout` and `MaxRuntime` (zero = config default).
- **`Clock`** — `Now()` and `NewTimer(d)`; tests use a fake that advances by hand and never sleeps.

### 1.2 The two stores

**Recurring — `<project>/.be-code/schedules.md`.** One `## <name>` section per schedule, each a
list of `key: value` lines (`when`, `instruction` — may continue on indented lines, `task`,
`allow` — one line per grant, `state`, `created-by`, `created`, `last-run`, `last-outcome`,
`failures`, `ask-timeout`, `max-runtime`), under a short header paragraph explaining the file.
Unrecognised lines are kept verbatim on rewrite, as the task documents keep theirs. **The file
wins** over anything cached: it is reread whenever the scheduler reloads (startup, every
`/schedule` command, every `schedule` tool call, and before each fire). A structurally broken
file is renamed aside as `schedules.broken-<stamp>.md` with a notice, never silently dropped;
a single unparseable section is skipped with a notice naming it and left in the file.

**One-off — `store.Session.Timers`.** A new field (JSON `timers`) holding `[]schedule.Schedule`,
saved and restored with the session like `Chat`. A recurring spec never goes here; a one-off
never goes to the project file.

**Approval hashes — the dotdir.** `~/.be-code/engine/<key>/schedules.json` maps schedule ID to
the hash of its approved content (spec, instruction, task, allowance). Never stored in
`schedules.md`, so editing the file cannot forge an approval (§3.3).

### 1.3 The scheduler, in `internal/agent`

A `scheduler` value owned by the `Agent`:

- One goroutine and one timer, always armed for the nearest due time across both stores, re-armed
  on every add, remove, pause, resume, edit or reload.
- On a due time it queues the event: an `InboxItem` with a new `Scheduled` field (the schedule ID;
  such an item is also `Harness: true`). A schedule that already has an event waiting in the
  queue is not queued again; its next due time is computed from now.
- It is started only by a UI, once that UI's approvals and events are wired —
  `Agent.StartSchedulesAsync()`, the same shape and the same reason as `StartSubAgentsAsync` — and
  never by `buildAgent`. Headless `run` never starts it: a one-shot run has no "later".
- `finishSession` stops it before its own early returns, next to `StopAllSubAgents`.
- `schedules.enabled: false` means no scheduler, no `schedule` tool, and `/schedule` reports it
  is disabled.

## §2 When an event fires

### 2.1 Always its own turn

- `deliverInbox` skips `Scheduled` items: a fired event is never folded into a turn already
  running.
- When the session goes idle, anything a person queued starts the next turn first; then scheduled
  events, one per turn, in the order they came due. A person's line never shares a turn with a
  scheduled event, so exactly one set of rules governs every turn.
- Both UIs' leftover-queue paths and `DrainItems` callers are changed accordingly; a drain that
  takes a scheduled item takes it alone.

### 2.2 The turn

- The request reads
  `[Scheduled event "<name>" — <spec>, set by <you|the model> <date>]` followed by the
  instruction on its own lines. The TUI shows a new entry kind, `entrySchedule`, rendered
  `⏰ <name>` in the theme's new `Schedule` colour; the plain REPL prints the same header as text.
- If the schedule names a `task`, that node is set `doing` through the engine's existing
  `SetStatus` before the run. A missing or closed node gets a notice and the event still runs
  without it.
- It runs through `RunFull` like any request: verification and repair, review, working memory and
  the prompt layout all apply unchanged. The request is an ordinary new user message, so the
  prompt cache is extended, not disturbed.
- It is not a request a person typed: it never calls `BeginTypedRequest` and never clears the
  untrusted-web flag.

### 2.3 The allowance

`Registry.SetAllowance(*schedule.Allowance)` is set when the fired turn begins and cleared in a
`defer` when `RunFull` returns — after the repair rounds, and on a panic. It lives on the primary
registry only; `Subset` and `Scoped` registries never copy it, so plan mode and sub-agents never
inherit it (and a sub-agent's writes, which reach the parent's seam through a captured `Approve`,
are unaffected — this is why the allowance is checked in the tools rather than by wrapping
`Approve`).

The shell, file-write and browser tools consult it at the point they would otherwise prompt, in
this order:

1. Deny globs, the browser's `deny` tier and sensitive-field refusals refuse, as always.
2. `shell_after_web` still asks while the untrusted-web flag is set, allowance or not.
3. An exact match within the allowance approves without asking: a `shell:` glob for a simple
   command, a `write:` prefix for a write (editor diff review is skipped too; the checkpoint
   snapshot in `OnBeforeWrite` still runs), a `browser:` host for an interaction the host's tier
   would otherwise ask about (never `browser_watch`, which always asks).
4. Anything else asks as usual.

### 2.4 Outside the allowance

The normal prompt is raised in every attached terminal. During a fired turn the tools ask through
`ApproveCtx` (both UIs wire it) with a deadline of the schedule's `ask_timeout`. When the deadline
passes the question is withdrawn — every view closes its modal with `withdrawn: scheduled event
"<name>" waited <n>m with nobody to answer` — and the action is refused. The model sees an
ordinary refusal and can wrap up or report. The event is never frozen indefinitely.

### 2.5 Bounds

- The usual `max_turns`.
- `max_runtime` per schedule (default `schedules.max_runtime`): the turn's context is cancelled
  when it runs out, exactly as Esc cancels it.

### 2.6 Afterwards

- `LastRun` and `LastOutcome` are recorded — `ok`, `checks failed`, `refused: <action>`,
  `timed out`, `cancelled`, `error: <first line>` — in `schedules.md` (recurring) or the session
  (one-off). A one-line notice goes to the transcript.
- A one-off becomes `done` (kept for `/schedule` to show until the session ends).
- A recurring schedule that ends in something other than `ok` `schedules.pause_after_failures`
  times running pauses itself with a notice. `ok` resets the count.

## §3 Consent

### 3.1 Created by the model

The `schedule` tool's `add` raises a new approval action, **`schedule`**. The prompt shows
everything at once: name, the spec and its next three due times, the instruction, the task link,
and the allowance one grant per line. Answers are yes and no only: like `browser_watch`, the TUI's
`a` does nothing on it (and `a`'s handler must list it explicitly — its final branch disables
file-write previews), and the REPL asks `approve? [y/N]`. **`-y` never approves a schedule**: it
means "run this task now", not "act for me later". A headless or non-interactive approver
refuses with `(non-interactive; this action always asks a person)`.

### 3.2 Created by a person

`/schedule add …` parses the command and shows the same summary as a confirmation before saving,
so the allowance is seen as it was understood.

### 3.3 Changes

- **Widening asks** as in §3.1: a new grant, a changed instruction, a changed task, a shorter
  interval or an earlier time.
- **Narrowing never asks:** pause, cancel, removing a grant.
- The model may pause or cancel schedules it created. Pausing or cancelling one a person created
  asks that person first.
- A schedule edited by hand in `schedules.md` no longer matches its approval hash; it is treated as
  unapproved and marked `changed since approved` at the next startup prompt.

### 3.4 Refused outright

- A bare `shell: *`.
- A `write:` or `shell:` grant that resolves outside the workspace.
- More than `schedules.max_active` active schedules across both stores.
- **Creating or widening a schedule while the untrusted-web flag is set** — refused, not asked,
  because a web page must never be able to plant a future job. The refusal says so and points at
  `/schedule add`, which a person can still use.

A `write: .` grant is allowed but the prompt labels it `anywhere in the workspace` in the warning
colour.

### 3.5 The startup prompt

When a session starts or resumes with any active schedule or timer, `StartSchedulesAsync` raises
one prompt (action `schedule`, detail listing every schedule, those whose hash no longer matches
marked `changed since approved`) before anything fires. **yes** records the hashes and starts the
scheduler, which then runs missed times once each (§Decisions). **no** pauses every one of them;
`/schedule resume <name>` brings them back individually, each with its own §3.1-style prompt. In a
shared session any attached terminal answers; the first answer wins (the existing shared-ask
behaviour). The prompt, like the sub-agent startup gate, is raised on its own goroutine, so a
terminal attaching afterwards is shown it.

### 3.6 Paths that cannot create schedules

A DM never reaches the agent. An `@agent` chat mention may lead the model to call the tool, which
still raises the §3.1 prompt. Sub-agents and plan mode do not have the tool.

## §4 Tool, commands, UI, config

### 4.1 The `schedule` tool

Flat schema: `action` (`add | list | pause | resume | cancel`), `name`, `when`, `instruction`,
`task`, `allow` (array of `kind: value` strings). Registered in interactive sessions (TUI hosted or
local, plain REPL) when `schedules.enabled`; never in headless `run`, sub-agents (`Scoped`) or
plan mode (`Subset`). It must be registered before the prompt is composed (known-tools for
embedded calls). Guidance rides with the tool: use it for follow-ups and recurring upkeep, never as
a way to put off work the request asks for now; ask for the narrowest allowance that does the job.

### 4.2 `/schedule`

Added to `ui.SlashCommandTable`; busy-safe in both UIs.

- `/schedule` — a table: name, spec, next due, state, last outcome, created by.
- `/schedule add <name> <when> -- <instruction> [allow <grant>; <grant> …]`
- `/schedule show <name>`
- `/schedule pause | resume | cancel <name>`
- `/schedule run <name>` — queues it now, under its own allowance.

### 4.3 UI

`entrySchedule` in the transcript (`⏰ <name>`, `Schedule` theme colour, added to every palette),
and a status-line segment `next: <name> <time>` when anything is active. The plain REPL prints the
same text.

### 4.4 Config

```json
"schedules": {
  "enabled": true,
  "min_interval": "5m",
  "max_active": 20,
  "ask_timeout": "10m",
  "max_runtime": "30m",
  "pause_after_failures": 3
}
```

Each key gets a default in `config.Default` and a line in the README config reference.

### 4.5 What does not change

The task engine's rules (ids, one `doing`, documents win) — the only contact is `SetStatus` on a
linked node. The prompt layout and cache invariant. The approval seam's shape: one new action
name, and `ApproveCtx` used by tools during a fired turn.

## §5 Failure handling

- A panic in the scheduler goroutine is fenced and noticed (`schedules stopped: …`); the session
  continues without them.
- An unreadable or broken `schedules.md` is renamed aside (§1.2); timers in the session are
  unaffected.
- A clock jump (suspend/resume, NTP) is handled by re-arming from `Now()` on every wake; anything
  that became due during the jump runs at most once.
- A fired turn that errors records `error: …` and counts as a failure (§2.6).
- Save failures of either store are noticed and retried on the next change; they never stop a run.

## §6 Testing

- **`internal/schedule`**: table tests for every form and its errors; DST gap and overlap; month
  ends; `weekdays`; cron fields; `min_interval` refusal; Markdown round-trip keeping foreign lines;
  a golden file; allowance parsing and matching (bare `*` refused, prefixes confined).
- **Scheduler (fake `Clock`)**: fires in due order; re-arms on add; never double-queues; a missed
  time runs once; nothing fires before the startup prompt; `no` pauses all; clock jump runs once;
  pause-after-failures.
- **Turns**: a scheduled item is never delivered mid-run; a person's queued line runs before it and
  never shares its turn; a task link sets `doing`.
- **Allowance**: a match approves silently, a miss asks; deny globs win; `shell_after_web` still
  asks; `browser_watch` still asks; `Subset`/`Scoped` never inherit; cleared after a panic;
  `ask_timeout` withdraws the question and refuses.
- **Consent**: `-y` does not approve; `a` is ignored; creation refused under the untrusted-web
  flag; a hand edit is marked changed; the model cannot cancel a person's schedule without asking;
  `max_active` refused.
- **e2e** (`test/e2e`, mock backend): an `in 1s` timer fires into an idle session and the
  transcript shows the `⏰` turn.
- **Live checklist** (`docs/live-checklist.md`): a hosted session left overnight with nobody
  attached, results read the next morning; a resume that raises the startup prompt.

## §7 Out of scope

- OS-level hooks (systemd timers, Task Scheduler, launchd) that start BE-Code — later.
- Schedules that start a session, or run in a workspace with no session.
- Per-event notifications outside the session (email, push).
- Sub-agents owning a scheduled event.
