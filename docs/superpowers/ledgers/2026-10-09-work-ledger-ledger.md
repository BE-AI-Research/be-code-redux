# Work ledger (A) — what was decided along the way

Plan: `docs/superpowers/plans/2026-10-09-work-ledger.md`. Spec:
`docs/superpowers/specs/2026-10-09-work-ledger-design.md`. Branch
`feat/work-ledger`, from `main` at 1.2.5 after the pack loader merged.

This is the record of the rulings made while the plan ran — decisions taken
without stopping to ask, each with why and what it costs if wrong — so the
owner can see them in one place and overturn any of them.

## What went wrong, and what changed because of it

**The feature was built correctly and guaranteed nothing.** Every piece passed
its own tests; the fresh reviewer found that the promise the pieces exist to
keep was broken at three independent points, each invisible from inside the
piece that broke it:

- `KeepOld` reported a git branch as the restore point for files `git add -A`
  never captured, because it honours `.gitignore` — and `.be-code/` is
  ignored in this repository, so the spec's own worked example of a conflict
  was the broken case. The code said "recoverable" and printed a `git restore`
  line that could not work.
- `Resolve` inferred "nobody to ask" from a nil approver. `be-code run`
  installs one unconditionally, so that inference never held headless, and the
  approver refused `work_conflict` — turning the owner's "write last, never
  refuse" ruling into a refusal in exactly the runs it was about. The README
  this branch added promised the opposite, and a test pinned the bug.
- Every mid-run notice went to `QueueNotice`, which is flushed once at UI
  start. So the line that distinguishes a recoverable overwrite from silent
  loss was never delivered.

Each is small. None was findable by testing the unit that contained it, and
all three were in the seams between units — the place a per-task reviewer
would also have missed them, and the reason the whole-branch review exists.

**The second lesson is about tests that pass for the wrong reason.** Three of
mine did: `cmd/workconflict_test.go` asserted the refusal that *was* the
defect; `internal/engine/seen_test.go` passed `OpenAt`'s arguments reversed
and so exercised a layout production never uses; and the engine hook it tested
never fired in production at all, because the hooks were assigned after the
one `loadDocs` a session runs. A test that passes tells you less than it
appears to when you wrote both sides.

## What the design got right

The §2 distinction held up: a mechanical race retries and never prompts, and
the reviewer confirmed it can never reach `work_conflict`. The one place the
plan chose strictness over the owner's "never refuse" — refusing when the
restore point itself fails — was the one place strictness is correct, and the
reviewer agreed. `Registry.OnConflict`/`OnRead` as injected hooks kept
`internal/tools` free of the new package, as intended.

---

# SDD ledger — plan: docs/superpowers/plans/2026-10-09-work-ledger.md

Spec: docs/superpowers/specs/2026-10-09-work-ledger-design.md (read; binding
authority where it and the plan disagree).

Setup: Ruling: executing on the feature branch `feat/work-ledger` in the main
checkout rather than a git worktree, with the owner's consent to the branch.
A worktree pinned a previous session's cwd inside a directory the merge then
had to remove, which made the merge impossible from the session that did the
work. Cost if wrong: the main checkout is occupied while this runs.

Setup: superpowers:test-driven-development is loaded in this session and
governs every step below.

Pre-flight: shared-interface rows, from the plan's Interfaces blocks.

| consumer | producer | what | found |
|---|---|---|---|
| T4 | T3 | gitctx.SnapshotTo, SnapshotPrefixOverwrite | exact match |
| T6 | T2 | Seen (via Ledger in T9, not directly) | T6 takes Theirs as a value; no direct use |
| T6 | T4 | KeepOld, Restore, RestoreNone | exact match |
| T6 | T1 | Record (for Conflict.Others) | exact match |
| T9 | T1,T2,T4,T6 | Record/Others, Seen, PruneSidecars, Resolve/Asker | exact match |
| T10 | T9 | Ledger.NoteHash | **T9's Produces block does not list NoteHash**; T10 adds it. Ordering 9→10 covers it, and T10's Step 9 writes it. |
| T8 | — | nothing; the seam is a hook, so T8 adds no import | by design |

Pre-flight: three notes, none a conflict.

- T6's test file uses `gitRepo(t)`, defined in T4's restore_test.go. Same
  package, so it is an ordering dependency rather than an interface: 4→6
  covers it.
- T1's package-level `Others(dir, workspace, selfPID)` and T9's method
  `(*Ledger).Others()` share a name. Legal in Go (one is a method) and both
  are in the plan deliberately; worth knowing before reading either.
- T5 is independent of the workledger package entirely — it is the mechanical
  race in internal/packs, and shares no interface with anything here. It
  could run in any position; the plan's order is fine.

Pre-flight: no conflicts to rule on. Starting Task 1.
Task 1: Ruling: the plan's `var Alive = procattr.Alive` does not compile — `procattr.Alive` exists only in `alive_windows.go` (`//go:build windows`); `procattr/alive.go` holds only the decision helpers. Added this package's own per-platform pair, `alive_unix.go` (`syscall.Kill(pid, 0)`, EPERM counts alive) and `alive_windows.go` (`procattr.Alive`), which is exactly the shape `internal/live/pid_unix.go`+`pid_windows.go` and `internal/ide/pid_windows.go` already carry — so this duplicates two lines the codebase duplicates on purpose rather than inventing anything. The spec's requirement is the rule, not the function: "a pid that cannot be queried counts as alive", honoured on both platforms. Verified with `GOOS=windows go build`. Cost if wrong: one more small file pair in a package that already has two precedents for it.
Task 1: complete (commits d794db7..50a17e9, tests: go test ./internal/workledger → ok  	github.com/brown-enterprises/be-code/internal/workledger	0.004s)
Task 2: Ruling: the plan's `TestFlushToAnUnwritablePlaceIsNotFatal` noted `/x`, a path that does not exist, and then asserted `Changed` was false — which contradicts the plan's own `TestADeletedFileIsAChange`, where a noted file that is now missing is deliberately a change. The implementation is right and the test's path was wrong, so the test now uses a real file and asserts both directions (unchanged reads false, changed reads true) after a failed flush, which is what "the in-memory map still works" actually means. Cost if wrong: nothing; the deleted-file semantics stay pinned by their own test.
Task 2: complete (commits 50a17e9..e8016b8, tests: go test ./internal/workledger → ok  	github.com/brown-enterprises/be-code/internal/workledger	(cached))
Task 3: complete (commits e8016b8..0556c39, tests: go test ./internal/gitctx ./internal/ui → ok  	github.com/brown-enterprises/be-code/internal/ui	0.641s)
Task 4: complete (commits 0556c39..e0b7930, tests: go test ./internal/workledger → ok  	github.com/brown-enterprises/be-code/internal/workledger	0.053s)
Task 5: Note: my first edit replaced everything from `mutate` to end-of-file, which removed `readState`, `TrustDetail`, `coreExcerpt` and `sortedKeys` as well — caught immediately by `go vet` (`undefined: sortedKeys`), reverted with `git checkout`, and redone as a surgical replacement of `mutate`'s own text. Watched the real RED afterwards: the delta was lost ("the concurrent writer's trust was lost") before the retry was added, which is the behaviour the task exists to fix.
Task 5: complete (commits e0b7930..e1130f8, tests: go test ./internal/packs → ok  	github.com/brown-enterprises/be-code/internal/packs	(cached))
Task 6: complete (commits e1130f8..54e2111, tests: go test ./internal/workledger → ok  	github.com/brown-enterprises/be-code/internal/workledger	(cached))
Task 7: complete (commits 54e2111..b67c34c, tests: go test ./cmd ./internal/ui ./internal/tui → ok  	github.com/brown-enterprises/be-code/internal/tui	2.455s)
Task 8: Ruling: the plan's test used `r.ApproveFileWrites` and `r.Dispatch(ctx, name, map)`, neither of which exists — the field is `Registry.ApproveWrites` (tool.go:87) and `Dispatch(ctx, provider.ToolCall)` takes one call value whose Arguments are a JSON string. Rewrote the calls through this package's existing `call(name, args string)` helper (scoped_test.go:13) rather than adding a second one, which would have shadowed it. Cost if wrong: the tests exercise Dispatch with JSON arguments, which is how the wire actually delivers them, so this is closer to production than the plan's shape was.
Task 8: complete (commits b67c34c..bebafb5, tests: go test ./internal/tools → ok  	github.com/brown-enterprises/be-code/internal/tools	(cached))
Task 9: Note: my edit_file `noteRead` insertion landed inside the error branch, after its `return` — `go vet` caught it as unreachable code and `make verify` failed on it, which is exactly what vet is in the pipeline for. Moved after the branch. The write_file and read_file insertions were correct.
Task 9: Ruling: built the ledger *after* `agent.New` rather than before it as the plan's Step 6 implies, because the conflict closure needs `ag.QueueNotice` for both its notice sink and `Warn`. The hooks are assigned to `reg` in the same block, before any UI is wired and so before anything can write. Cost if wrong: nothing — `reg` is the only thing that reads the hooks and it reads them per call.
Task 9: complete (commits bebafb5..9aa7b64, tests: make -f build.mk verify → ok  	github.com/brown-enterprises/be-code/internal/workledger	(cached))
Task 10: Ruling: the plan's engine test called `storeWithTasks`, which does not exist — this package's fixture is `testStore(t)` (report_test.go:11) over `OpenAt(workspace, dotdir, id, resumed, limits)`, and none of its fixtures take a map of documents. Wrote the fixture through the real write path instead: `Plan` a task (which writes a document), `Flush`, then `OpenAt` the same workspace and dotdir again so `loadDocs` reads it back. Cost if wrong: nothing — it exercises the production write-then-read path rather than a hand-placed file, which is stronger than the plan's shape.
Task 10: complete (commits 9aa7b64..c8221b3, tests: make -f build.mk verify → ok  	github.com/brown-enterprises/be-code/internal/workledger	(cached))
Task 11: Ruling: the plan's Step 3 writes an architecture section into `CLAUDE.md`, but this repository has no CLAUDE.md — the project's is at the parent, `/home/sbrown/Dev/BE-CodeRedux/CLAUDE.md`, and the owner's standing instruction is that it is outside git and must never be `git add`ed. Wrote the section there, because that is the file that serves the purpose the plan names, and staged only the README. The change is therefore unversioned and will not appear in this branch's diff or in the final review package. Cost if wrong: the invariants are written where the next session reads them but are not under review; the same content is in the spec, which is committed.
Task 11: complete (commits c8221b3..134649a, tests: make -f build.mk verify → ok  	github.com/brown-enterprises/be-code/internal/workledger	(cached))

Final review: fresh reviewer, Opus, high effort, on the whole-branch package (5b6d1e2..134649a), pointed also at the out-of-git CLAUDE.md section. 3 Critical, 10 Important, 12 Minor, 9 declined. Two findings came with runnable repros.
Final: fixed C1 (a gitignored workspace file's restore point held nothing while `KeepOld` reported `RestoreGit` and a `git restore` line — and `.be-code/` is gitignored in this repository, which makes a task document, the spec's own worked example, exactly the broken case; `git add -A` honours .gitignore). `KeepOld` now skips the git route for an ignored path and verifies with `gitctx.InTree` that the branch holds the file before promising it, falling through to the sidecar otherwise. New `gitctx.Ignored`/`InTree`. `TestKeepOldOfAGitignoredFileUsesASidecar` RED→GREEN, suite 34/34.
Final: fixed C2 (`Resolve` inferred "nobody to ask" from `ask == nil`, which is never true headless because `be-code run` installs `headlessApprover` unconditionally — and that approver refused `work_conflict`, so the owner's "write last, never refuse" ruling was unreachable in every headless and `-y` run while the README this branch added promised it there). Attendance is now a parameter (`workledger.Attendance`), decided by `cmd/root.go:conflictAttendance` from fired/headless/TTY, and `work_conflict` is out of `headlessApprover`'s two refusal lists. `TestConflictAttendance` (5 cases) RED→GREEN, suite 34/34.
Final: Ruling: deleted the two `cmd/workconflict_test.go` tests rather than fixing them — they asserted the refusal, which *was* the defect, so they passed because the bug was present. Replaced with `TestConflictAttendance` over the extracted decision and a source guard that `headlessApprover` no longer special-cases the action. Cost if wrong: the `-y` behaviour is pinned one level below the approver rather than through it.
Final: fixed C3 (every mid-run notice was discarded: `QueueNotice` only buffers and `FlushQueuedNotices` runs from `ResolveModel`, i.e. once at UI start — so "wrote X over a change this session did not make; the old version is at <branch>" was never delivered, making a recoverable overwrite indistinguishable from silent loss, the one outcome the feature exists to prevent). Notices now go through `Registry.OnNotice`, which both UIs already wire, with the queue as the pre-UI fallback.
Final: fixed I1 (`Ledger.Turn` was never called, so `Updated` was written once at `Open` and with `StaleAfter` at an hour **every** live session older than that was announced as "the process may be unrelated"). `OnRead` is now a throttled heartbeat (once a minute), and `Open` resolves the branch with one `gitctx` call so the notice can say "pid N on <branch>" rather than a bare pid. `TestActivityKeepsTheRecordFresh` and `TestTheRecordNamesTheBranch` RED→GREEN.
Final: Ruling: `writing` and `task` still are not populated — they need a real turn boundary, and the only hook of that shape (`Events.OnToolEnd`) is assigned by the UIs, so wrapping it from `buildAgent` would be overwritten. `Turn` stays as the API that change will call. Cost if wrong: the conflict detail can say who and on what branch but not which file they were last writing; §4's `writing` is unmet and §14's "a turn behind" risk is moot because it is never set.
Final: fixed I2 (`Others` filtered on `.json`, which also matches the seen store `<pid>-<stamp>.seen.json` in the same directory — and `{"path":"hash"}` unmarshals into `Record` without error at PID 0, so it took the "not a record" branch and **deleted every session's seen file, including its own, on every call**). A positive filter (`recordName`) and nothing outside it is ever removed. `TestOthersLeavesFilesItDoesNotRecognise` RED→GREEN.
Final: fixed I3 (`work.keep_overwritten` did nothing: `PruneSidecars` was called on `~/.be-code/work`, where no sidecar is ever written). Pruning now happens beside each file as a sidecar is written there, and `Open` sets `KeepSidecarsFor` instead.
Final: fixed I4 (`Registry.Subset` copied `OnBeforeWrite` but not `OnConflict`/`OnRead`, so a sub-agent's write went over another *session's* change with no prompt, no restore point and no notice — the other writer there is another process, which is this sub-project's remit, not B's). `TestSubsetKeepsTheConflictGate` RED→GREEN.
Final: fixed I6 (`noteRead` recorded what was passed in, *before* `post_write` hooks ran — and a hook's whole purpose can be to rewrite the file, so `post_write: gofmt -w` made this session's own next edit raise "Another session changed this file" with the body "changed outside BE-Code" when nobody had). `noteWritten` now reads the file back after the hooks. `TestAPostWriteHookDoesNotCreateAFalseConflict` RED→GREEN.
Final: fixed I7 (the hooks were assigned at the end of `buildAgent`, after `store.Load`, `attachEngine`'s `loadDocs` and `attachPacks` — and `loadDocs` runs once per session, so `engine.OnDocHash` never fired in production at all; my Task 10 test passed only because it opened a second store by hand). The read hooks moved ahead of all three readers; the conflict closure stays where it is because it needs `ag`.
Final: fixed I9 (an attended "yes" discarded `KeepOld`'s error silently, while the detail the person had just read promised "theirs is kept and the notice says where"). It now writes anyway — their choice, made with the difference in front of them — and says the old version could not be kept. `TestAttendedYesSaysSoWhenTheOldVersionCouldNotBeKept` RED→GREEN.
Final: fixed I10 (`note` returned early while `Warn` was nil, *before* setting `warned`, and `Warn` is assigned after `Open` returns — so §12's "one notice saying so" was dropped by two independent routes, and the `Dir()` failure branch never called `note` at all despite its comment). A pending notice is held and delivered by the new `SetWarn`.
Final: Ruling on I8: the dotdir half of detection is groundwork, not protection, and the spec oversold it. `Changed` is only ever asked about a workspace path (`fs.go` is its one caller), so `packs.OnStateRead` and `store.OnSessionRead` feed a map nothing consults today. I did not wire a dotdir prompt, because the dotdir files' actual protection is `mutate`'s compare-and-retry, which *is* implemented and tested, and a prompt there would be the mechanical race §2 forbids prompting about. §10's third wiring point (naming the owning session from the record in the `HostPID` guard) is unbuilt. Cost if wrong: the spec claims more than the code does for the shared dotdir files; the hooks cost two nil checks.
Final: **I5 was fixed after the owner asked whether the branch was ready to merge, and it was the reason the honest answer was "not quite".** The difference now lives in the detail itself — `diff.Preview(theirs, mine)`, bounded to 24 lines — and the `d` key is gone from both the detail and the TUI hint. That required threading what the write is about to put there through `Registry.OnConflict` and `Ledger.OnConflict`, which is where it should have been from the start: the gate was deciding about a write whose content it could not see. It is strictly better than either option I had offered, because nobody has to press anything to see what they are choosing between. `TestDetailShowsTheDifferenceAndAdvertisesNoDeadKey` and `TestDetailBoundsTheDifference` RED→GREEN; `TestDetailNamesTheFileTheOptionsAndWhoChangedIt` had to drop its "show me" assertion, which is the Task 6 test catching the change correctly. Suite 34/34.

Superseded ruling, kept for the record: Ruling on I5: removed nothing and implemented nothing — `d` ("show me") is still advertised in the detail and the TUI hint and is still a dead key in the TUI and a "no" in plain mode. **This is the one Important I did not fix**, and it is deliberate: implementing it means rendering `diff.Preview(theirs, mine)` and re-asking, which is a new interactive flow in two UIs, and removing it means shipping two options where the spec promised three and the spec's own argument for making this an approval rather than a yes/no ("it is why this is an approval with a diff") falls away. The owner should choose. Cost if wrong, and it is not small: a person presses `d` expecting to see the difference and, in plain mode, abandons their write instead.
Final: minor (deferred): `mutate`'s window is narrowed rather than closed — another process can still land a write between the byte comparison and the rename; the comment overstates it as "both writers' intent survives".
Final: minor (deferred): `mutate` reads the file twice (`rawState` then `readState`) where parsing `before` would do, and `OnStateRead` therefore fires for bytes a retry discards.
Final: minor (deferred): `Seen.Forget` and the `deleted` sentinel have no production caller.
Final: minor (deferred): `KeepOld` runs a whole-working-tree snapshot per conflict, inside the tool call, and leaves a `be-code/pre-overwrite/*` branch per conflict that nothing prunes.
Final: minor (deferred): `workledger.Open` can never return a non-nil error, yet its signature and `cmd/root.go`'s `_` both suggest it can.
Final: minor (deferred): `be-code init` builds an agent, writes a work record, and never calls `finishSession`, so it always leaves its record behind.
Final: minor (deferred): `workLedger` is a package var and the three global hooks are never cleared at `Close`, so a `store.Save` after `finishSession` still notes into a flushed map.
Final: minor (deferred): the fallback seen path under `os.TempDir()` records absolute workspace paths into a shared directory and is never removed.
Final: minor (deferred): `Conflict.Detail` does not say a record is `Suspect`, so a stale record reads with the same confidence as a fresh one.
Final: minor (deferred): the spec's §5 notice ends "(be-code sessions lists them)"; `NoticeLine` drops it, and `be-code sessions` lists live records rather than work records.
Final: Ruling: re-graded two Minors to Important and fixed them. M7: my own `internal/engine/seen_test.go` called `OpenAt(ws, dot, …)` where the signature is `OpenAt(dir, root, …)` — the arguments were reversed, so the fixture put task documents in the dotdir and state in the workspace and the test passed against a layout production never uses. M12: three factual errors in the CLAUDE.md section I wrote (it credited `procattr.Alive` for Unix pruning, omitted the store and engine read sites, and described the attendance inference C2 disproved). Both are wrong facts rather than polish, and the second is in the file I wrote a memory about this session for exactly this reason. Cost if wrong: two small corrections nobody asked for.
Final: fix pass complete, `make -f build.mk verify` green (34 packages, exit 0), ~/.be-code/config.json mtime 1791327107 unchanged. One `verify` run mid-pass failed three `internal/tui` mailbox tests; each passed alone twice and the next full run was green, so it was CPU contention — the known pattern in this repo — not a regression. No second review dispatched: every fix has a test that failed first.

---

# Post-merge review, 2026-10-10

A second reviewer (a different model, reading `67bd7e3..f26b798` cold) found
no Critical and four Important, all in seams the final review had not
reached. Fixed on `fix/review-importants`, each with a test that failed
first; `make -f build.mk verify` green, 34 packages.

Post-merge: fixed the seen store leak — `work/<pid>-<started>.seen.json` is
keyed by a pid and start time no later process ever has, so the file `Close`
flushed had no reader and accumulated one per session with nothing to remove
it (I2's fix had, correctly, stopped `Others` deleting unclassified files, and
so also stopped the only thing that had ever removed them). `Close` removes
it; `Others` prunes one whose pid is gone. `TestCloseLeavesNoSeenStoreBehind`,
`TestOthersPrunesASeenStoreWhoseProcessIsGone` RED→GREEN. Ruling: the spec's
"a resumed session does not treat every file as unseen" is withdrawn rather
than re-engineered — it needs a key that survives the process, and the rule
for an unseen file is already "a first write is not a conflict". Cost if
wrong: a resumed session's first write to a file it saw before the restart is
not gated.

Post-merge: fixed the README fence (`README.md:1282` had prose after the
closing fence, so the block ran to line 1489 and the rest of the section plus
the whole config reference rendered as code). `TestReadmeFencesCloseOnTheirOwnLine`
RED→GREEN.

Post-merge: fixed `edit_file`'s false conflict. It applies the replacement to
the current contents, so the other writer's change is already in what it
writes; the gate then asked a person to choose between two things not in
conflict, and unattended it took a whole-tree snapshot and noticed "wrote X
over a change this session did not make", which was untrue. Ruling: by §2's
own test ("applying ours discards theirs") an edit is not a conflict; it is a
note to the model, through the new `Registry.OnChanged`/`Ledger.Changed`, and
the gate is `write_file`'s alone. Spec §7 and §10 amended. Cost if wrong: a
person is not told that a file the model is editing moved under it; the model
is, and their change is intact either way.
`TestEditNeverPromptsButTellsTheModelTheFileMoved` RED→GREEN.

Post-merge: fixed attendance-by-accident. The session host runs with
`cmd.Stdin = nil`, which is `/dev/null`, a character device, so `stdinIsTTY`
said "terminal" and the default interactive mode was Attended for that reason
alone; a correct TTY check would have turned every hosted session Unattended
with nobody told. `attendedHere()` now counts a session host (its terminals
are attached through its socket) or a terminal on stdin.
`TestAttendedHereCountsAServedHost` pins it. `stdinIsTTY`'s own `/dev/null`
reading is pre-existing and untouched.

Post-merge: minor (deferred): three doc comments were orphaned onto the new
hook vars — `engine/store.go` (`Key`), `store/sessions.go` (`Save`),
`packs/trust.go` (`readState`) — the slip the pack-loader review fixed once.
Post-merge: minor (deferred): `Record.Session` is never populated (§4).
Post-merge: minor (deferred): `trust.go`'s `writeState` does not note its own
bytes, so the trust file's hash in `Seen` is stale after every write; harmless
until dotdir detection is wired (I8).
Post-merge: minor (deferred): README says `keep_overwritten` covers "a dotdir
file", but no dotdir path can reach `KeepOld` (`Changed` is asked only about
workspace paths); sidecars arise for non-repo workspaces and gitignored files.
Post-merge: minor (deferred): `internal/packs/race_test.go` is not gofmt-clean.
Post-merge: minor (deferred): CHANGELOG.md has no entry for the pack loader
or the work ledger.
Post-merge: minor (deferred): the `Seen` file form (`NewSeen` load, `Flush`)
now has no production caller.

---

# Sub-project B: what moved under a sub-agent (2026-10-10)

Branch `feat/subagent-drift`, on top of `fix/review-importants`. No plan
document: the owner approved the approach in conversation — a per-run record
of what the sub-agent read, *and* a git snapshot of the tree at dispatch "so
the agent can always refer to the existing code … both solutions work, and
the primary and agent can then decide which is relevant".

B: Ruling: overlap between sub-agents needs nothing new — `subagent.Ready`
refuses a candidate whose scope overlaps a running root, and
`engine.Store.SetScope` refuses an overlapping scope — so B is the drift half
only. Cost if wrong: none; both refusals have tests.

B: Ruling: the run's record (`workledger.Run`) is its own, never the
session's `Seen`. The main model writing a file the sub-agent had read
updates `Seen` and reads as "no change" — true of the session, wrong for the
sub-agent, and the case where a hand-back's advice is most likely stale.
`TestRunCountsAWriteByThisSessionToo` pins it. The first read is the
baseline; a later read of a file that already moved does not quietly become
one. Cost if wrong: a sub-agent that re-reads a file after it moved, and
whose summary rests on the second read, is still reported as drifted — the
honest over-report.

B: Ruling: `Run.Wrote(path)` forgets the baseline and the registry's
following `OnRead(path, on-disk)` re-sets it — the order `noteWritten`
already uses, so a post_write hook's rewrite is the run's own result and a
path only written and never read back is never drift. The earlier WIP took
the content directly; the hooks do not carry it. Cost if wrong: none seen;
`TestRunDoesNotReportItsOwnWrites` and `TestRunNeverReportsAFileItOnlyWrote`.

B: Ruling: the snapshot is taken in `runSub` after consent, on the run's own
goroutine, not in `dispatchPicked` on the agent goroutine: a whole-tree
`git add -A` into a private index is seconds on a large repository, and a
dispatch from inside `run()` should not pay them. A failed snapshot is one
notice and the step runs without a base — orientation, never a gate. Cost if
wrong: a snapshot that fails leaves `Dispatch.Base` empty and the hand-back
names drift without a branch.

B: Ruling: one `be-code/sub-agent/<stamp>` branch per dispatch, never
pruned — the same shape as `pre-overwrite` and `pre-init`, and the same
deferred cost (a branch list that grows). The dispatch tells the sub-agent
that files `.gitignore` excludes are not on it, the lesson `KeepOld` paid
for. Cost if wrong: litter.

B: `HandBackDrift` is the one definition both UIs and the queued line use for
the drift clause, so they cannot disagree. `run --json` rows carry `drifted`
and `base`.

B: tests: `TestHandBackNamesFilesThatMovedUnderTheSubAgent` (a hooked
provider moves a file between the sub-agent's read and its next request; no
repository, so no base), `TestDispatchSnapshotsTheTreeAndTellsTheSubAgent`
(a real repository: the branch holds the pre-dispatch file, the sub-agent's
system prompt names it, HEAD and the checkout did not move),
`TestRenderDispatchNamesTheBaseBranch`, `TestHandBackLineNamesWhatChangedUnderneath`,
and the five `Run` tests. Each watched RED first.
