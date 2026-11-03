# Dispatching Jobs to Sub-agents Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The main model hands a scoped job to a configured co-worker with one tool call, is told when it finishes, asks or stalls, and can look at, wait for, stop, steer or extend it; a stall it cannot fix reaches the operator.

**Architecture:** Two new tools (`dispatch`, `jobs`) over a `JobRunner` interface the agent implements. A job is a task-record step carrying a job mark (owner, scope, note), dispatched at once through the existing `dispatchPicked`/`runSub` path, so confinement, consent, snapshot, `ask_main`, hand-back and the lanes are reused unchanged. A supervisor goroutine with an injectable clock watches each run's last activity, pauses while the run waits on someone else, and queues stall lines; the third stall stops the job and notices the operator.

**Tech Stack:** Go 1.25; existing packages `internal/engine`, `internal/subagent`, `internal/agent`, `internal/tools`, `internal/ui`, `internal/tui`; the sub-agent test fixture `newSubFixture` in `internal/agent/subagents_test.go`; the e2e mock in `test/e2e/mock_server.py`.

**Spec:** `docs/superpowers/specs/2026-11-03-dispatch-jobs-design.md`

## Global Constraints

- Branch `feature/dispatch-jobs` off `main`. Version stays 1.4.0 in `build.mk` until release (the changelog entry goes under a new `## Unreleased` heading).
- Commit with `--no-gpg-sign`; add files by name, never `git add -A` (untracked `k.pgp`, `x.txt` must never be committed).
- Never touch the real `~/.be-code`: tests use `t.TempDir()` HOMEs; the packages' `TestMain`s guard it.
- A sub-agent's writes stay inside its scope and pass the operator's approval seam; an online sub-agent asks consent once; a fired (scheduled) turn dispatches nothing and cannot `stop`/`reply`/`extend`.
- Exact strings from the spec: `dispatched <id> to <who> (scope: …)`; the stall line `job <id> (<who>) has been quiet for <d>: <n> tool calls, last <tool> <file> <d> ago.` + `jobs extend <id> to give it more time, jobs stop <id> to end it, jobs reply <id> <text> to steer it, or jobs status <id> to look.`; second stall `stalled again after your extend/reply` or `stalled again; you did not act on the first notice`; operator notice `job <id> (<who>) stalled three times and was stopped; its step is blocked. /task show <id>`; hard cap reason `ran out of time`; stall stop reason `stalled`; session end reason `session ended`; refusal `dispatch needs the task record`.
- Defaults: `subagents.stall_after` 180 s (0 disables), `subagents.max_runtime` 1800 s; existing `max_concurrent` 2, `max_turns` 40, `ask_timeout` 600 unchanged.
- The stall clock pauses while a run is `waiting` (ask_main, lane, consent, approval) and `max_runtime` counts running time only.
- `make -f build.mk verify` green before any task is reported complete; `gofmt -l` empty.

## Review Focus

1. A `dispatch` whose scope is `["."]` while another job runs must be refused (overlap), and a scope of `["./internal/x/"]` or `["internal/x"]` must normalise to the same `internal/x` the ready rule compares. (Task 3: `TestDispatchJobRefusesOverlapAndNormalisesScope`.)
2. A job that finishes while the main model is inside `jobs wait` on a different job must still have its hand-back queued, not lost. (Task 4: `TestJobWaitOnOneJobKeepsAnothersHandBack`.)
3. Two stall lines for one job must never be queued at once (a slow main model that has not consumed the first); the supervisor coalesces. (Task 5: `TestSupervisorQueuesOneStallLineAtATime`.)
4. A job stopped by `max_runtime` while parked on `ask_main` must not fire (waits are excluded), and a job that has been waiting on an approval for an hour must not be stopped as "ran out of time". (Task 5: `TestMaxRuntimeExcludesWaits`.)
5. `/task show` of a job step and the saved task document must round-trip the job mark, so a document edited by hand keeps the step out of the scheduler. (Task 1: `TestJobMarkRoundTrips`.)

---

### Task 1: The job mark in the task record and the ready rule

**Files:**
- Modify: `internal/engine/node.go` (field), `internal/engine/markdown.go` (`renderFields`, `trailingField`, parse), `internal/engine/store.go` (`AddJob`, `Steps()` carries the mark)
- Modify: `internal/subagent/records.go` (`Step.Job`), `internal/subagent/ready.go` (skip job steps)
- Test: `internal/engine/job_test.go` (new), `internal/subagent/ready_test.go` (append)

**Interfaces:**
- Produces: `Node.Job bool` (json `job,omitempty`; document trailing field `  job`, after `after:`); `func (s *Store) AddJob(parent, text, note, owner string, scope []string) (id string, err error)` — `Add` + `SetOwner(id, owner, false)` + `SetScope(id, scope)` + mark + `Note(id, note, "", false, false)` when note is non-empty, one document write; `subagent.Step.Job bool`; `Ready` never returns a job step as ready or waiting.

- [ ] **Step 1: Write the failing tests**

`TestJobMarkRoundTrips` (engine): open a store in a temp workspace, `StartTask`, `AddJob(root, "port the scanner", "details here", "big", []string{"internal/scan"})`; assert the returned id is a child of the root, the node has `Job`, `Owner == "big"`, `Scope == ["internal/scan"]`, the note is on the node; reopen the store from disk and assert `Job` survived; read the document and assert the line ends `  @big  scope: internal/scan  job`. `TestJobMarkSurvivesHandEdit`: write a document line with `  job` by hand, load, assert `Job`. `TestReadySkipsJobSteps` (subagent): a job step with owner, scope and a sub-agent card is in neither `ready` nor `waiting`; the same step without `Job` is ready.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/engine/ ./internal/subagent/ -run 'TestJobMark|TestReadySkips'` — Expected: build failure on `Job`/`AddJob`.

- [ ] **Step 3: Implement**

Add the field; extend `trailingField` to accept `job` as a bare trailing word and `renderFields` to emit it last; `AddJob` in `store.go` beside `SetScope`; copy the mark into `Steps()`; in `Ready`'s walk, `if s.Job { return }` before the owner check (a job's subtree is the sub-agent's own, as an assigned subtree is).

- [ ] **Step 4: Run the tests and the two packages' suites**

Run: `go test ./internal/engine/ ./internal/subagent/` — Expected: `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/node.go internal/engine/markdown.go internal/engine/store.go internal/engine/job_test.go internal/subagent/records.go internal/subagent/ready.go internal/subagent/ready_test.go
git commit --no-gpg-sign -m "feat(engine): a job mark on task steps the scheduler skips"
```

---

### Task 2: Config knobs, run state, and every run in `/agents`

**Files:**
- Modify: `internal/config/config.go` (`SubAgentsConfig.StallAfter`, `MaxRuntime`; defaults), `README.md` (config reference, two lines)
- Modify: `internal/agent/subagents.go` (`subRun` fields; `SubAgentState` fields; `SubAgentStates` lists every run)
- Modify: `internal/ui/agents.go` (`AgentLines` rows)
- Test: `internal/config/config_test.go` (append), `internal/agent/subagents_test.go` (append), `internal/ui/agents_test.go` (append)

**Interfaces:**
- Produces: `SubAgentsConfig{…, StallAfter int `json:"stall_after"`, MaxRuntime int `json:"max_runtime"`}` defaulting to 180 and 1800; on `subRun`: `job bool`, `lastActivity time.Time`, `lastTool string` ("read_file x.go"), `waiting string` ("" or `ask_main` | `lane` | `consent` | `approval`), `waitSince time.Time`, `runFor time.Duration` (accumulated running time, waits excluded), `stalls int`, `stalled bool`, `lastIntervention time.Time`; on `SubAgentState`: `Job bool`, `Waiting string`, `LastActivity time.Time`, `LastTool string`, `Stalls int`; `SubAgentStates()` returns one row per run (plus idle rows for co-workers with no run and the waiting rows as today).

- [ ] **Step 1: Write the failing tests**

`TestSubAgentsConfigDefaults`: `Default().SubAgents.StallAfter == 180`, `MaxRuntime == 1800`; a loaded config with the keys absent gets the defaults. `TestSubAgentStatesListsEveryRun`: with `newSubFixture`, two runs for one owner (install two `subRun`s under `s.runs` directly) → two rows named the owner with different `Node`s. `TestAgentLinesShowStalledAndWaiting`: a state with `Stalls: 1, LastTool: "read_file x.go"` renders `stalled` and `last read_file x.go`; one with `Waiting: "approval"` renders `waiting (approval)`.

- [ ] **Step 2: Run to verify they fail** — `go test ./internal/config/ ./internal/agent/ ./internal/ui/ -run 'TestSubAgentsConfigDefaults|TestSubAgentStatesListsEveryRun|TestAgentLinesShowStalled'`.

- [ ] **Step 3: Implement** the fields and defaults; `SubAgentStates` iterates `s.runs` (sorted by node id) instead of `runs[0]` per owner; `AgentLines` appends `stalled` / `waiting (<reason>)` and `last <tool> <ago>` to a running row. README: `subagents.stall_after` (180) and `subagents.max_runtime` (1800) with one sentence each.

- [ ] **Step 4: Run** `go test ./internal/config/ ./internal/agent/ ./internal/ui/ ./cmd/ -run 'SubAgent|AgentLines|Readme'` — Expected: `ok`.

- [ ] **Step 5: Commit** `feat(subagents): stall and runtime knobs; one /agents row per run`.

---

### Task 3: `Agent.DispatchJob` on the existing runner

**Files:**
- Modify: `internal/agent/subagents.go` (`DispatchJob`, `EnableSubAgents` for any co-worker, session-end close for jobs), `cmd/root.go:429-442` (enable when `len(cws) > 0`)
- Test: `internal/agent/jobs_test.go` (new)

**Interfaces:**
- Consumes: `Store.AddJob` (Task 1); `dispatchPicked(steps, candidate, cw)`; `subagent.Within`, `subagent.Overlap`; `a.firedTurn()`; the scope normalisation `task scope` uses (`internal/tools/task.go` cleans with `filepath.ToSlash(filepath.Clean(strings.TrimSpace(p)))` and refuses `..`/absolute).
- Produces:
  ```go
  // DispatchJob starts a job now. Errors are the spec §3 refusals, each
  // a plain sentence: errDuringFired; "<who> is not a co-worker";
  // "a job needs a description"; "a job needs a scope"; "scope <p>
  // escapes the workspace"; "scope is outside <who>'s max_scope (<list>)";
  // "<who> cannot be dispatched: its max_scope is invalid";
  // "scope overlaps <id>"; "dispatch needs the task record";
  // "<n> of <max> jobs already running; jobs wait first".
  func (a *Agent) DispatchJob(ctx context.Context, who, job, scope, extra string, scopePaths []string) (id string, err error)
  ```
  (`scope` is the raw list joined for messages; `scopePaths` the list.) `EnableSubAgents` is called for any co-worker; a card for a co-worker without `sub_agent` has `SubAgent: false`, which `Ready` still honours for scheduler steps.

- [ ] **Step 1: Write the failing tests** (all with `newSubFixture`, whose scripted sub-agent provider answers "done"):

`TestDispatchJobRunsAndHandsBack`: `DispatchJob(ctx, "big", "port the scanner\nkeep the API", "internal/scan", "", []string{"internal/scan"})` returns a child id of the active root; the store shows the step `Job`, owner `big`, scope, and the note `keep the API`; after `WaitSubAgents`, the hand-back line is in the inbox (`Peek()` contains `big` and the id) and the step is `done`. `TestDispatchJobRefusals`: each refusal in the Interfaces list, one call each, asserting the exact sentence; the invalid-`max_scope` case uses a co-worker whose `max_scope` is `["../x"]`. `TestDispatchJobRefusesOverlapAndNormalisesScope`: first job scope `["./internal/x/"]`; a second with `["internal/x/y.go"]` is refused `scope overlaps <id>`; a second with `["."]` is refused; after the first finishes `["."]` is accepted. `TestDispatchJobWorksForANonSubAgentCoworker`: a co-worker without `sub_agent: true` can be dispatched; the same co-worker's `@name` scheduler step stays `waiting: … is not a sub-agent`. `TestDispatchJobRefusedDuringFiredTurn`: with `a.Tools.SetAllowance` installed (as `schedules_review_test.go` does) → `errDuringFired`. `TestJobStepClosesAtSessionEnd`: dispatch a job whose provider blocks until cancelled; `StopAllSubAgents("session ended")`; the step is `blocked` with reason `session ended`, not `todo`, and a reopened store's `Ready` offers nothing.

- [ ] **Step 2: Run to verify they fail** — `go test ./internal/agent/ -run 'TestDispatchJob|TestJobStep'`.

- [ ] **Step 3: Implement** `DispatchJob`: validate in the listed order; `AddJob` through `engineDo` (refuse with `dispatch needs the task record` when the engine is absent or the fenced ledger is the no-op one); build the `subagent.Step` for the new node from `st.Steps()` and a `Candidate{ID, Owner}`; reserve in `s.pending`; call `dispatchPicked`; set `run.job = true` on the installed run (under `s.mu`, right after `s.runs[c.ID] = run`: add a `job bool` argument to `dispatchPicked` or set it through the candidate). Session end: in `settleSub`, when `run.job && interrupt`, close the node `blocked: session ended` through `CloseAs` instead of resetting it, and never append it to the resume list. `cmd/root.go`: `EnableSubAgents(cws, …)` when `len(cws) > 0`.

- [ ] **Step 4: Run** `go test ./internal/agent/ ./cmd/` — Expected: `ok` (the resume tests must still pass: scheduler steps keep their interrupted behaviour).

- [ ] **Step 5: Commit** `feat(agent): DispatchJob runs a scoped job on a co-worker now`.

---

### Task 4: `jobs` actions on the agent: status, wait, stop, reply, extend

**Files:**
- Modify: `internal/agent/subagents.go` (or new `internal/agent/jobs.go` holding the five methods and `JobState`)
- Modify: `internal/agent/inbox.go` (a predicate to take one harness line out: reuse `RemoveWhere`)
- Test: `internal/agent/jobs_test.go` (append)

**Interfaces:**
- Produces:
  ```go
  type JobState struct {
      ID, Owner, State string // running | waiting (reason) | asking | stalled | done | blocked | stopped
      Elapsed  time.Duration; Calls int; At string; LastTool string; LastActivity time.Time
      Summary  string // finished jobs: the hand-back summary
  }
  func (a *Agent) JobStates() []JobState                 // running then finished, by id
  // JobWait blocks until the job (any job when id == "") ends, asks, or
  // stalls, or until d elapses or ctx ends; it returns the queued line for
  // that event and removes that line from the inbox. "" and nil when
  // nothing is running.
  func (a *Agent) JobWait(ctx context.Context, id string, d time.Duration) (string, error)
  func (a *Agent) JobStop(id, reason string) error         // StopSubAgent by node id; reason into the step
  func (a *Agent) JobReply(id, text string) error          // ReplyAsk when asking, else the job's own inbox
  func (a *Agent) JobExtend(id string) error               // clears stalled, restarts the clock, stamps lastIntervention
  ```
  Inbox: hand-back, ask and stall lines are enqueued with `EnqueueHarness`; `JobWait` finds them by a prefix the three producers share (`job <id>` / the existing hand-back line's leading `sub-agent <owner> …`), so give `handBackLine` and the ask line a `jobTag(id)` prefix when the run is a job, and match on it.

- [ ] **Step 1: Write the failing tests**

`TestJobWaitReturnsTheHandBackAndConsumesIt`: dispatch, `JobWait(ctx, id, time.Minute)` returns a line containing the id and `done`; `Peek()` no longer holds it. `TestJobWaitReturnsOnAnAsk`: the scripted sub-agent calls `ask_main` (as `TestSubAgentAskMainRoundTrip` arranges); `JobWait` returns the question line; `JobReply(id, "yes")` resumes it; a second `JobWait` returns the hand-back. `TestJobWaitOnOneJobKeepsAnothersHandBack`: two jobs; wait on the slower one; the faster one's hand-back is still in `Peek()` after the wait returns. `TestJobWaitWithNothingRunningReturnsAtOnce`. `TestJobStopClosesBlockedWithReason`. `TestJobReplyWithoutAnAskLandsInTheJobsInbox`: the scratch agent's `Pending()` is 1 after `JobReply` on a running, non-asking job. `TestJobStatesShapes`: running, asking, done rows with the fields set.

- [ ] **Step 2: Run to verify they fail.**

- [ ] **Step 3: Implement** on `subRun`: a `events chan jobEvent` (buffered 8) the run writes on hand-back, ask and stall (Task 5 adds the stall writer); `JobWait` selects on the matching run's channel (or a fan-in over all runs when `id == ""`), the timer and `ctx`, then removes the line via `RemoveWhere(item.Harness && strings.HasPrefix(item.Text, jobTag(id)))`. `JobReply` without an ask: `run.scratch.EnqueueHarness(text)`.

- [ ] **Step 4: Run** `go test -race ./internal/agent/ -run 'TestJob'` then `go test ./internal/agent/` — Expected: `ok`.

- [ ] **Step 5: Commit** `feat(agent): jobs status, wait, stop, reply and extend`.

---

### Task 5: The stall supervisor

**Files:**
- Create: `internal/agent/supervisor.go`
- Modify: `internal/agent/subagents.go` (activity and waiting hooks around `laneAcquire`, `subConsent`, the scoped registry's approvals, `askMain`; `StartSubAgents` starts the supervisor; `StopAllSubAgents` stops it)
- Modify: `internal/tools/tool.go` (`Registry.OnAsk func(start bool)` hook on a scoped registry, called around the parent's `Approve`)
- Test: `internal/agent/supervisor_test.go` (new)

**Interfaces:**
- Consumes: `subRun` fields from Task 2; `a.EnqueueHarness`; `a.notice`; `a.firedTurn()`; `CloseAs`; `run.events` (Task 4); `a.Cfg.SubAgents.StallAfter/MaxRuntime`; a new exported `Agent.Headless bool`, set by `cmd/root.go`'s `buildAgent` from its `headless` argument (the agent has no such field today; `a.sched` being nil is not a reliable stand-in).
- Produces:
  ```go
  // supervisor ticks every 30 s (tick and now are fields replaced by tests)
  // over every run: advances runFor while not waiting; a run quiet for
  // StallAfter (waits excluded) is marked stalled, its count raised, one
  // line queued (never a second while the first is unconsumed); the third
  // stall — the second on a headless run — stops the job with reason
  // "stalled" and notices the operator; a run past MaxRuntime stops with
  // "ran out of time". Lines fire after a fired turn ends, never during.
  type supervisor struct { now func() time.Time; tick time.Duration; stop chan struct{} }
  func (a *Agent) startSupervisor()
  func (a *Agent) stopSupervisor()
  func (a *Agent) noteActivity(run *subRun, tool, file string)   // tool call done or delta
  func (a *Agent) setWaiting(run *subRun, reason string)         // "" ends the wait
  func stallLine(run *subRun, now time.Time) string              // the exact spec text
  ```
  Hooks: the scratch agent's `Events.OnToolEnd` and `OnDelta` call `noteActivity`; `laneAcquire` wraps with `setWaiting(run,"lane")` / `setWaiting(run,"")`; `subConsent` the same with `"consent"`; `askMain` with `"ask_main"`; the scoped registry's `OnAsk(true/false)` with `"approval"`.

- [ ] **Step 1: Write the failing tests** (fixture plus a fake clock: set `a.subs.sup.now` and call `a.subs.sup.sweep()` directly instead of waiting on the ticker)

`TestFirstStallQueuesOneLineAndCountsOne`: a job whose provider blocks; advance the clock past `StallAfter`; `sweep()` → `Peek()` holds exactly the spec's stall line (assert the `has been quiet for 3m` and `jobs extend <id>` fragments) and `run.stalls == 1`. `TestSupervisorQueuesOneStallLineAtATime`: advance twice without consuming → still one line. `TestExtendResetsTheClockNotTheCount`: `JobExtend`; advance half of `StallAfter` → no new line; advance the rest → second line says `stalled again after your extend/reply`, `stalls == 2`. `TestIgnoredNoticeStallsAgainLater`: consume the line, do nothing, advance → `stalled again; you did not act on the first notice`. `TestProgressResetsTheCount`: `noteActivity` → `stalls == 0`, `stalled == false`. `TestThirdStallStopsAndNoticesTheOperator`: three stalls → step `blocked` reason `stalled`, hand-back queued, a notice recorded through `a.Events.OnNotice` with the exact operator text. `TestHeadlessStopsAtTheSecondStall`. `TestWaitingPausesTheClock`: `setWaiting(run,"approval")`, advance a day, `sweep()` → no stall and `runFor` unchanged; `setWaiting(run,"")` then advance `StallAfter` → stall. `TestAskMainNeverStalls`. `TestMaxRuntimeExcludesWaits`: `runFor` reaches `MaxRuntime` only through running time; stopped with `ran out of time`; a run that waited an hour is not stopped. `TestStallDuringFiredTurnIsHeld`: with the allowance installed the line is not queued; after `ClearAllowance` the next `sweep()` queues it. `TestStallAfterZeroDisables`.

- [ ] **Step 2: Run to verify they fail.**

- [ ] **Step 3: Implement** `supervisor.go` with `sweep()` doing one pass under `s.mu` (collect decisions, release the lock, then enqueue, close and notice — `a.notice` and `CloseAs` take other locks); the ticker goroutine calls `sweep()`; the hooks in `subagents.go` as listed; the `OnAsk` hook on the registry (`Scoped` installs it into its `Approve` closure).

- [ ] **Step 4: Run** `go test -race ./internal/agent/ -run 'Stall|Supervisor|MaxRuntime|Waiting'`, then the package and `./internal/tools/`.

- [ ] **Step 5: Commit** `feat(agent): a supervisor that reports stalled jobs to the main model and stops the hopeless ones`.

---

### Task 6: The `dispatch` and `jobs` tools, registration, guidance

**Files:**
- Create: `internal/tools/jobs.go`, `internal/tools/jobs_test.go`
- Modify: `internal/tools/allowance.go` (`firedSelfGated` gains `dispatch` and `jobs`), `cmd/root.go` (register with the consult tool when co-workers exist), `internal/agent/subprompt.go` (`subAgentGuidance` names `dispatch` and `jobs` and the stall line; `cmd` appends the guidance when any co-worker exists, not only a sub-agent)
- Test: `internal/tools/allowance_test.go` (`TestEveryBuiltinToolIsClassified` passes), `cmd/root_test.go` (append: registration)

**Interfaces:**
- Consumes: Task 3's `DispatchJob`, Task 4's five methods, behind:
  ```go
  type JobRunner interface {
      DispatchJob(ctx context.Context, who, job, scope, extra string, scopePaths []string) (string, error)
      JobStates() []JobState        // tools gets its own copy of the struct (tools never imports agent)
      JobWait(ctx context.Context, id string, d time.Duration) (string, error)
      JobStop(id, reason string) error
      JobReply(id, text string) error
      JobExtend(id string) error
  }
  func NewDispatch(r JobRunner, fired func() bool) Tool   // name "dispatch"
  func NewJobs(r JobRunner, fired func() bool) Tool       // name "jobs"
  ```
  Schemas, flat: `dispatch{who, job, scope []string, context, wait bool}`; `jobs{action: status|wait|stop|reply|extend, id, for int, reason, text}`. `dispatch` with `wait: true` calls `DispatchJob` then `JobWait(id, MaxRuntime)`. `jobs status` renders one line per `JobState` (`<id>  <owner>  <state>  <elapsed>  <calls> calls  at <At>  last <LastTool> <ago>` and the summary for finished ones). During a fired turn: `dispatch` and `jobs stop|reply|extend` return the `errDuringFired` text as an error result; `status` and `wait` run.

- [ ] **Step 1: Write the failing tests** with a fake `JobRunner` recording calls: schema is flat and names only those fields; each refusal passes through as an error result; `wait: true` chains; `jobs` with an unknown action errors `action must be status, wait, stop, reply or extend`; `jobs stop` without an id errors; the fired-turn matrix; `TestEveryBuiltinToolIsClassified` once the two names are in `firedSelfGated`.

- [ ] **Step 2: Run to verify they fail.**

- [ ] **Step 3: Implement** the two tools; register in `cmd/root.go` right after `RegisterConsultTool` with `ag` as the runner (the agent satisfies `tools.JobRunner` once Task 4's `JobState` fields match; add a conversion if the structs differ); the guidance text:

```
Sub-agents: dispatch {who, job, scope} hands a scoped job to a co-worker, which runs it on its own and reports back in a later message; jobs status|wait|stop|reply|extend looks at, waits for, ends, steers or extends a job. Never edit inside a running job's scope. A job that goes quiet is reported to you as "job <id> … has been quiet"; decide: extend it, stop it, or reply to it. A step you assign with an owner and a scope in the task record is also run by that model. Answer a sub-agent's question with jobs reply or task reply.
```

- [ ] **Step 4: Run** `go test ./internal/tools/ ./internal/agent/ ./cmd/` — Expected: `ok`.

- [ ] **Step 5: Commit** `feat(tools): dispatch and jobs`.

---

### Task 7: Docs and the UIs' job rows

**Files:**
- Modify: `README.md` (Sub-agents section: `dispatch`, `jobs`, the stall rules; command reference unchanged), `CHANGELOG.md` (`## Unreleased` with one bullet), `internal/engine/markdown.go` only if `/task show` needs `job` rendered (Task 1 already renders the field), `internal/tui/view.go` + `internal/ui/repl.go` only where `/agents` rows need the new columns (Task 2 did them in `AgentLines`).
- Test: `cmd/readme_test.go` passes.

- [ ] **Step 1: Write the README paragraph** under "Sub-agents": how `dispatch` and `jobs` work, that any co-worker may be dispatched, the stall line and the three-stall rule, the two new config keys, that a job step shows as `job` in `/task`. One changelog bullet under `## Unreleased`: "**Jobs.** The main model dispatches a scoped job to any co-worker with one tool call, waits for it or is told when it ends, asks or stalls, and extends, steers or stops it; a job that stalls three times is stopped and the operator told. `/agents` shows every job, `subagents.stall_after` and `subagents.max_runtime` tune it."

- [ ] **Step 2: Run** `go test ./cmd/ -run Readme` — Expected: `ok`. Commit `docs: jobs`.

---

### Task 8: e2e `dispatch` scenario

**Files:**
- Modify: `test/e2e/mock_server.py` (a `dispatch scenario:` branch: the primary's first reply calls `dispatch` for co-worker `mock-cw` with scope `pkg/`; the sub-agent model (`coworker-model`) replies with a `write_file pkg/x.go` then a summary; the primary's next turn, which carries the hand-back line, replies "done"), `test/e2e/run_e2e.sh` (the scenario after `sub-agent`; `[PASS] dispatch`)
- Create: `test/e2e/dispatch_e2e.sh` only if the branch needs its own HOME; otherwise inline in `run_e2e.sh` like the `sub-agent` scenario.

- [ ] **Step 1: Write the scenario**: config with `coworkers:[{name:"mock-cw", provider:"mock", model:"coworker-model"}]` (no `sub_agent`), `subagents.stall_after: 2`; run `be-code run -y -C "$WS" "dispatch scenario: port the scanner"`; assert `pkg/x.go` exists, the run output contains the hand-back (`sub-agent mock-cw` and `done`), and the task document holds a line ending `job`. Stall branch: a second run `dispatch scenario stall:` where the mock holds the sub-agent's reply for 5 s; assert the primary's transcript contains `has been quiet for` and that the mock then received a `jobs` call with `extend` (the mock records request bodies; grep its log).

- [ ] **Step 2: Run** `make -f build.mk build && sh test/e2e/run_e2e.sh` — Expected: `[PASS] dispatch` and `E2E PASS`. Commit `test(e2e): dispatch scenario`.

---

### Task 9: Manual check (controller, last)

From the TUI on the LAN Ollama co-worker (`gemma4:26b` or whichever `coworkers` entry the config has): dispatch one real job with a small scope, watch `/agents` show the run and its last activity, let it finish, confirm the hand-back arrives in the transcript and `/task` shows the step as `job … done`. Then a stall: set `subagents.stall_after: 20` in a throwaway HOME, dispatch a job whose model is stopped mid-run (`ollama stop` on the LAN box), and confirm the stall line reaches the main model and `jobs extend` clears it.
