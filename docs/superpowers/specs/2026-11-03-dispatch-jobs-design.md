# Dispatching jobs to sub-agents — design

**Status:** approved in brainstorming, 2026-11-03. Target release: 1.4.1 (not yet released).
Branch `feature/dispatch-jobs` off `main`.

**Builds on:** `2026-09-22-sub-agent-task-engine-design.md` (sub-agents on the task tree: pens,
confined scope, lanes, `ask_main`, hand-backs through the queue), the co-worker and consent rules
in the README's "Co-working models" and "Sub-agents" sections, and the fired-turn rules in
`2026-09-26-schedules-design.md`.

## §1 Intent

The main model should work the way the owner's own agent harness works: hand a scoped job to a
lighter or cheaper model, keep its own thread, and be told when the job is done, or when it has
stalled, with enough in hand to decide what to do about it on its own. Today a sub-agent can only
be reached by decorating a step of the task record with an owner and a scope and waiting for the
scheduler; there is no verb for "run this now", no way to wait for a job, and nobody watches a
job that has gone quiet.

**Goals.**
- One tool call starts a job: who runs it, what it is, which files it may write.
- The hand-back reaches the main model as soon as it exists, mid-turn or idle.
- The main model can look at, wait for, stop, extend or answer a running job.
- A stalled job is reported to the main model, which decides; only when its own intervention
  fails does the operator hear about it, at their next turn.
- Everything the owner already relies on holds: the sub-agent's writes stay inside its scope and go
  through the operator's approval seam; an online sub-agent asks consent once; the task record
  stays the one source of truth; a scheduled event's turn dispatches nothing.

**Non-goals.** A sub-agent dispatching its own sub-agents; jobs that outlive the session; jobs on a
model that is not a configured co-worker; a priority queue beyond `max_concurrent`.

**Anti-goals (a failure even if it works).** A job that writes outside its scope. A hand-back the main
model never sees. A stall that nobody notices. A sub-agent's question the main model cannot answer.
A second kind of sub-agent that `/task` and `/agents` do not show.

## §2 Decisions

| Decision | Owner's choice |
|---|---|
| Who decides on a stall | The main model, on its own, to keep autonomy. The operator is asked only when the main model's intervention fails, and then at the operator's next turn. |
| Everything else | The design below, as presented in chat, with the calls marked "builder's call" left to the plan. |

## §3 The `dispatch` tool

Registered (by `cmd/root.go`, with the consult tool) when at least one co-worker is configured. Any
configured co-worker may be dispatched; `sub_agent: true` keeps its present meaning (a step with that
owner's `@name` tag is dispatched by the scheduler) and is not required here. The lanes a dispatch
needs are built at startup for every co-worker's server (`EnableSubAgents` runs when any co-worker
exists, not only when one is a sub-agent).

```
dispatch {
  who:     "<co-worker name>",            // required
  job:     "<what to do, one paragraph>", // required; the first line names the step
  scope:   ["internal/x/", "cmd/y.go"],   // required; "." means the whole workspace
  context: "<facts the job needs>",       // optional
  wait:    false                          // optional; true blocks until the job ends
}
```

**What it does, in order.**
1. Refuses during a fired turn (`errDuringFired`, as `task owner` does), when `who` is not a
   co-worker, when `job` or `scope` is empty, when a scope path escapes the workspace, when the
   scope is outside `who`'s `max_scope` (a co-worker whose `max_scope` was entirely invalid at
   validation cannot be dispatched at all: the stated confinement never widens to the workspace),
   when the scope overlaps a running job's or an assigned step's scope (`"."` overlaps everything,
   so it is accepted only while nothing else runs), when the task store is unavailable (a
   `noopLedger` session: "dispatch needs the task record"), and when `max_concurrent` jobs are
   already running or reserved (the error names how many and suggests `jobs wait`). The scope
   checks are the ones `task scope` already applies, reused.
2. Adds a step to the task record: a child of the active root (or of the doing node, when one is
   doing under that root), text = the job's first line, with the rest of `job` and `context`
   recorded as the step's note; owner `who`, scope as given. The step's id is the **job id** the
   tool returns and every later message names. A session with the task engine off still runs the
   job; the id is then `job-N` and nothing is filed.
3. Dispatches it at once through the existing path (`dispatchPicked`): same consent, same snapshot
   branch, same scoped registry, same `ask_main`. The dispatch text is the step's text plus the
   note, so the sub-agent reads exactly what the main model wrote. The step carries a **job
   mark** (an engine node field, persisted in the document as a trailing field like owner and
   scope) that the ready rule skips, so the scheduler never dispatches it a second time, and that
   `/task` renders as `job`. One co-worker may run several jobs at once, within `max_concurrent`;
   `/agents` and `jobs status` list every run, not one per co-worker.
4. Returns `dispatched <id> to <who> (scope: …)`; with `wait: true` it behaves as `jobs wait <id>`
   (§4) and returns what that returns.

The main model is told, in the guidance that already describes sub-agents, that `dispatch` exists,
that a job's hand-back arrives as a later message, that it must not edit inside a running job's
scope, and what the stall messages look like.

## §4 The `jobs` tool

One tool, five actions, so the catalog grows by two names, not six.

```
jobs { action: "status" }                      // every running and finished job this session
jobs { action: "wait",   id: "2.3", for: 300 } // block up to `for` seconds (default max_runtime)
jobs { action: "stop",   id: "2.3", reason: "…" }
jobs { action: "reply",  id: "2.3", text: "…" } // answers the job's open ask_main (as task reply)
jobs { action: "extend", id: "2.3" }            // clears the stall and grants another stall_after
```

`status` lists id, owner, state (`running` / `waiting` / `asking` / `stalled` / `done` /
`blocked` / `stopped`), elapsed, tool calls, the doing node inside the job, last activity
("read_file x.go, 40 s ago"), and for a finished job its hand-back summary. `wait` returns as
soon as the job (or, with no id, any job) **ends, asks, or stalls**, and its result is the line the
queue would have carried (the hand-back, the question, or the stall line); it returns at once when
nothing is running. The queued copy is always written first and `wait` takes it out of the queue
when it returns, so a `wait` cancelled mid-way loses nothing and the main model never reads a line
twice. `stop` closes the step `blocked` with the reason, exactly as `/agents stop` does. `/agents`
keeps showing the same jobs for the operator; `/agents stop <name>` is unchanged.

## §5 Hand-backs and asks

Unchanged in shape: the hand-back is the queued line `handBackLine` produces today (status, summary,
files, elapsed, calls, drift), delivered before the main model's next call; when the main model is
idle the leftover queue starts a turn in both UIs, which is the "ping". A `wait` returns the same
line as the tool's result instead. A sub-agent's `ask_main` still parks the job as `asking` and
reaches the main model as the queued question; `jobs reply` and `task reply` both answer it.

## §6 Stalls

**Progress** is any of: a tool call completing, a streamed delta, a reply to an ask. Each updates
`lastActivity` on the run (the scratch agent's `OnToolEnd`/`OnDelta` hooks; the scoped registry's
`OnWrote`). One supervisor goroutine per session, started with `StartSubAgents`, ticks every
30 s and compares `now - lastActivity` with `subagents.stall_after` (seconds, default 180; 0
disables supervision). **The clock does not run while the job is waiting on someone else:** parked
on `ask_main`, queued behind the primary for its server's lane, waiting on the online-consent
modal, or waiting on a write approval. The run records which of these it is in (`waiting`, with
the reason shown by `jobs status` and `/agents`), and the clock resumes from zero when the wait
ends.

**First stall.** The job is marked `stalled` (it keeps running) and one line is queued to the main
model:

```
job 2.3 (big) has been quiet for 3m: 14 tool calls, last read_file internal/x/y.go 3m ago.
jobs extend 2.3 to give it more time, jobs stop 2.3 to end it, jobs reply 2.3 <text> to steer it,
or jobs status 2.3 to look.
```

The line reaches the main model at its next call, or starts a turn when it is idle, like a
hand-back. The main model decides. `extend` resets the clock; `stop` ends the job; `reply` is
delivered to the job as the answer to a pending ask, or, when it has none, as a queued user line
in the job's own conversation before its next call (the sub-agent's inbox; the scratch agent has
one already); any progress by the job also clears `stalled`.

**Counting.** A stall is counted each time the clock reaches `stall_after` with no progress; after
a stall line the clock restarts, so a job the main model ignores stalls again one `stall_after`
later, not at once. Real progress (a tool call, a delta) resets the count to zero. `extend` and
`reply` restart the clock but not the count. `status` and `wait` are looks, not interventions,
and change nothing. A stall that fires during a scheduled event's turn is held and delivered when
that turn ends (the fired turn receives no queued lines and could not act anyway).

**Second stall** (the count reaches two): the line says `stalled again after your extend/reply`
(or `stalled again; you did not act on the first notice`), and the main model decides again.

**The operator.** The third stall of one job is "the main model's intervention failed": the job
is stopped (`blocked: stalled`, its hand-back queued as usual) and a notice tells the operator at
their next turn: `job 2.3 (big) stalled three times and was stopped; its step is blocked. /task
show 2.3`. No modal: the operator reads it in the transcript. A headless `-y` run, where nobody
reads a third notice, stops at the second stall instead.

**Hard cap.** `subagents.max_runtime` (seconds, default 1800) of running time, waits excluded: a
job past it is stopped with `blocked: ran out of time`, hand-back queued. Existing `max_turns`
still applies.

**Session end.** `finishSession` stops every job as it stops every sub-agent, but a job's step is
closed `blocked: session ended`, never put back to `todo` as an interrupted scheduler step is, and
the next session's resume pass never offers it again. The job mark is what tells the two apart.

## §7 Config, UI, docs

- `subagents.stall_after` (180) and `subagents.max_runtime` (1800) join `max_concurrent`,
  `max_turns`, `ask_timeout`; README config reference and the Sub-agents section describe
  `dispatch`, `jobs`, and the stall rules; a changelog bullet.
- `/agents` rows gain `stalled` and the last-activity column; nothing else in the UIs changes.
- The fired-turn tool gate: `dispatch` and `jobs` are classified like `task` (self-gated:
  `dispatch` refuses during a fired turn; `jobs status`/`wait` are allowed, `stop`/`reply`/`extend`
  refused), and `TestEveryBuiltinToolIsClassified` covers them.

## §8 Testing

- Tools: schema flatness; every refusal in §3 step 1; the task-record step and its id; the
  engine-off `job-N` path; `jobs` actions on a missing id.
- Agent, with the scripted co-worker provider the sub-agent tests use: dispatch → run → hand-back
  queued; `wait: true` returns the hand-back; `jobs wait` returns on the first finished job and at
  once when idle; `stop` closes the step blocked; `reply` answers a parked ask and, with no ask,
  lands in the job's inbox before its next call; the supervisor with a fake clock: first stall line,
  `extend` resets, second stall line, third stall stops the job and notices the operator; a job
  on `ask_main` never stalls; `max_runtime` stops a job; a fired turn refuses `dispatch` and the
  mutating `jobs` actions.
- UIs: `/agents` shows a stalled job with its last activity.
- e2e: a `dispatch` scenario on the mock backend (the mock answers the sub-agent's model with a
  write inside scope and a final summary): the primary dispatches, the hand-back line appears in
  the primary's next turn, the file is written; and a stall branch with `stall_after: 2` where
  the mock holds the sub-agent's reply, asserting the stall line reaches the primary and `jobs
  extend` clears it.
- Manual: one real job on the LAN Ollama co-worker from the TUI, watching `/agents`.

## §9 Out of scope

Nested dispatch; jobs surviving a session end; dispatch without the task store (a session whose
store failed to open refuses with "dispatch needs the task record"; `engine.tools: minimal` still
has the store and works); choosing a model by tier ("light"/"heavy") rather than by co-worker
name; a budget per job.

## §10 Calls made after the builder check (2026-11-03)

Settled by the author, not the owner; each is reversible in the plan: the stall clock pauses on
every wait that is not the job's doing (§6); the stall count is per job and resets on progress,
a third stall stops the job, and ignoring a notice is not itself a failure (§6); `wait` returns
on end, ask or stall and consumes the queued line (§4); dispatch reuses `task scope`'s
confinement checks, refuses `max_scope` violations and overlaps, and a co-worker with an invalid
`max_scope` cannot be dispatched (§3); a job's step carries a job mark the scheduler skips, one
co-worker may run several jobs, and the UIs list every run (§3); a job's step closes
`blocked: session ended` at exit (§6); dispatch needs the task store (§9).
