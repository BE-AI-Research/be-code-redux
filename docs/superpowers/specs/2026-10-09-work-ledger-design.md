# Work ledger: who is working where, and what happens when two writers meet

Date: 2026-10-09
Status: design, approved in conversation; not yet implemented
Sub-project **A** of three (see §3).

## 1. What this is for

Two things, which share one record:

- **Nobody can see what else is running.** A second session opened in a
  workspace is told nothing about the first. Neither is a person who runs
  `be-code packs add` in another terminal while a session is live.
- **A write can lose somebody's work or decision without saying so.** The
  shared files in `~/.be-code/` have no cross-process guard, and the workspace
  files a session writes have no notion of "somebody else changed this since I
  read it".

**Intended outcome.** A session knows what other sessions are doing in the
same workspace and says so once; and a write that would overwrite work or a
decision that arrived after this session read the file stops and asks, with
the options spelled out — except where nobody is there to ask, in which case
it writes, but marks the old version so it can be restored.

**Who it is for.** Anyone running more than one BE-Code at once, which is the
normal case for the owner (a live host plus an SSH login, a VM, a second
terminal) and increasingly normal for anyone using live sessions.

**Owner's decisions, carried into this design.** Detection is **by content
hash** of what was read, not mtime and not a schema-level counter.
Restoration is **through git for workspace files** and a timestamped sidecar
for the dotdir. When nobody is there to ask, the rule is **write last, but
mark the old version in the git tree so it can be restored** — not refuse.

## 2. The distinction this design rests on

"A conflict" names two different events, and treating them alike produces
either noise or silence. They get different mechanisms:

**A mechanical race.** Two processes' read-modify-write interleave on a
dotdir file, and one delta is lost. `internal/packs/trust.go:mutate` is the
case: it re-reads, applies one key, and writes under a mutex that serialises
only within one process, so a genuine interleaving drops the other writer's
key. Nobody decided anything that is at stake here — both deltas were wanted,
and the window is microseconds. **This retries; it never prompts.** A modal
raised on a microsecond race is noise, and a person cannot answer a question
about an interleaving they did not witness.

**A write over somebody's work.** The file changed between this session
reading it and writing it in a way that represents a decision or an edit: a
`/packs off` another session recorded, a task document the person edited by
hand, a `schedules.md` they changed, a pack directory another install
replaced. **This prompts**, because there is a real choice and only the person
knows which answer is right.

The test that separates them is not the file, it is **whether the change that
arrived is itself recoverable from the delta we are about to apply**. A race
is: re-read and re-apply, and both writers' intent survives. An overwrite is
not: applying ours discards theirs.

## 3. Scope

This spec is sub-project **A**. Agreed decomposition and order:

| | What | Status |
|---|---|---|
| **A** | The record of active work, and conflict on shared files (sessions on this machine, plus the dotdir files) | this spec |
| **B** | Sub-agents and co-workers inside one session: refusing or prompting on overlapping scope, and naming in a hand-back what changed underneath it | next, reads A's record |
| **C** | Teammates through the repo | a separate decision, after A ships |

C is deferred with a reason, not forgotten: a committed record is stale by
construction — it only arrives when somebody pushes — so it can say "nobody
is on this" when somebody is, which is worse than silence. And it puts
coordination state in git history, where it conflicts in exactly the
situations it exists to prevent. When it is taken up, the first question is
whether it should be a ledger at all or a read of what git already knows:
branches, their upstreams, and who last touched a file.

In scope for A:

- A per-session work record in the dotdir, written by every interactive
  session, pruned when its process is gone.
- A notice, once per session, naming other live work in this workspace.
- Hash-based change detection on the shared dotdir files and on the workspace
  files a tool writes.
- The `work_conflict` approval, with options.
- Restore points: `gitctx` for workspace files, sidecars for the dotdir.
- Compare-and-retry for the mechanical race in `mutate`.

Out of scope, deliberately:

- Locking. A lock that outlives a crashed process is worse than a lost
  update, and BE-Code already chose advisory records plus pid liveness for
  live sessions (`live.List`, `record.go`). This follows that.
- Merging. Where two writes conflict, the person picks one or looks; nothing
  here attempts a three-way merge of JSON or of Markdown.
- Anything outside this machine. C.
- Any new per-turn text in the system prompt. The prompt-cache rule holds: a
  value that changes every turn belongs on the end of a stored message, never
  in the system prompt (`CLAUDE.md`, prompt cache section).

## 4. The record

`~/.be-code/work/<pid>-<started>.json`, 0600, one per running session:

```json
{
  "pid": 48231,
  "started": "2026-10-09T20:41:07Z",
  "updated": "2026-10-09T21:02:55Z",
  "workspace": "/home/me/proj",
  "branch": "feat/thing",
  "session": "JLSDWK",
  "model": "qwen3:32b",
  "task": "003",
  "writing": [".be-code/tasks/003-add-palette.md", "internal/tui/chat.go"]
}
```

**Why a new record rather than extending `live/<code>.json`.** That file is
written by `cmd/live.go:launchServed` for *hosted* sessions only; `--no-host`,
plain mode and `be-code run` have none, and those are exactly the sessions
most likely to be the second writer in a workspace. A separate record covers
every session and leaves the live registry's job — advertising a socket to
attach to — alone.

**Liveness is `procattr.Alive`, never `os.FindProcess`.** The Windows lesson
is already paid for: `os.FindProcess` asks for `PROCESS_QUERY_INFORMATION`,
which a non-elevated terminal is refused for an elevated session's process,
and `live.List` then deleted a live record and the launch forked a copy
(`CLAUDE.md`, Windows rules). A record whose pid cannot be queried is counted
alive.

**Workspaces are matched with `live.SameWorkspace`, never `==`.** Case folded
on Windows, links and 8.3 names resolved. Two shells spell one folder
differently, and a miss here would report no other work when there is some —
the same bug that forked a host onto a copy of a session.

**`writing` is updated at turn boundaries, not per tool call.** One file write
per turn, not per `write_file`. A record rewritten on every tool call is a
disk write in the middle of a tool loop for no benefit, and the only consumer
— another session's startup notice and a conflict prompt's detail — reads it
at human timescales.

The record's own fields come from where they already live, not from new
bookkeeping: `branch` from `gitctx`, `session` from the store's resume code,
`task` from the engine's doing node (`Store.DoingUnderID`'s root), `writing`
from the set of paths `fs.go` wrote during the turn. This is a *different*
record from §6's hashes: `writing` is for a human to read in a notice, the
hashes are for the machine to compare. Conflating them would mean writing a
hash file on every tool call.

**Removal.** `finishSession` removes this session's record, beside
`StopAllSubAgents` and `StopSchedules`. A crash leaves it, and the next
session's prune removes it once the pid is gone — same contract as the live
registry.

## 5. The notice

Once per session, at the end of `buildAgent` where `ExplainBudget` already
speaks, and only when there is something to say:

```
note> another session is working in this workspace: pid 48231 on feat/thing,
      task 003, started 21 minutes ago (be-code sessions lists them)
```

It is a **notice, not a question**. Opening a second session in a workspace is
an ordinary thing to do — the owner does it from an SSH login every day — and
a prompt there would train people to dismiss prompts. The decision this notice
exists to inform is the one that comes later, when a write collides.

A session whose own record is the only one says nothing at all.

## 6. Detection

The mechanism is the one `internal/engine` already uses for task documents:
`state.Docs` is "the hash of each document as the engine last wrote it, so an
outside edit is recognisable" (`store.go:9`). A generalises it rather than
inventing a second scheme.

```go
// package workledger

// Seen records the hash of a file as this session last read or wrote it.
func Seen(path string, content []byte)

// Changed reports whether path differs from what this session last saw, and
// is false for a file this session has never seen — an unseen file is not a
// conflict, it is a first read.
func Changed(path string) (changed bool, theirs []byte, err error)
```

Hashes live in memory for the session, and on disk in the work record's own
directory (`work/<pid>-<started>.seen.json`) so a resumed session does not
treat every file as unseen. SHA-256 of the exact bytes; no normalisation,
because a byte that changed is a change.

**Where `Seen` is called**, which is the half that makes `Changed` mean
anything — a file never seen is never a conflict, so a missed `Seen` is a
detection that silently never fires:

| Read | Where |
|---|---|
| A tool reads a workspace file | `internal/tools/fs.go`'s `read_file`, after the read succeeds |
| A tool writes one | the same file's write path, after the write — what we wrote is what we last saw |
| The pack trust file | `internal/packs/trust.go:readState`, its one reader |
| A task document | `internal/engine`'s `loadDocs`, which already records a hash in `state.Docs`; `Seen` is fed from that rather than hashing twice |
| A session file | `internal/store`'s load and save |

The engine's case is deliberate reuse: it already hashes every document it
writes for exactly this purpose, so A reads its hash instead of computing a
second one that could disagree. Where the two ever differ, `state.Docs` is
the truth — it is the one the engine acts on.

Why not the alternatives, recorded so they are not re-proposed: **mtime and
size** cannot distinguish two writes inside one mtime tick, which is the
common case for small JSON; **a generation counter in each file** is exact and
cheap to compare but needs a schema bump and a migration for every file
already on disk, including two published packs' worth of `packs.json`.

## 7. What happens on a conflict

| Situation | Behaviour |
|---|---|
| Mechanical race in a delta write (`mutate`) | Re-read, re-apply the delta, write. Three attempts, then an error naming the file. No prompt: both deltas were wanted and nothing is lost. |
| A write would overwrite a change that arrived since this session read the file, and a person is there | `work_conflict` asks (§8). |
| The same, during a fired scheduled turn | **Write last, snapshot first**, and say where the old version went (§9). Never refuse: an unattended turn has nobody to ask, and the schedules design is explicit that a prompt with no deadline hangs the turn. |
| The same, with no approver at all (headless without `-y`) | As the fired turn: snapshot, write, say so. `-y` is irrelevant here — there is no consent to give, only a record to keep. |
| The file this session is writing is one it has never seen | Not a conflict. Write. |

## 8. The `work_conflict` approval

A new action, named in all three approvers per `CLAUDE.md`'s rule
(`internal/tui/view.go`'s `a` handler and its title/hint switch,
`internal/ui/repl.go`'s `noAlwaysAction` and its display switch,
`cmd/commands.go:headlessApprover`). **No "always"**: each conflict is about
a specific pair of changes, and a session-wide "always keep mine" is a
licence to lose work silently, which is the thing this feature exists to stop.

The detail names what changed, who changed it where that is knowable from the
work records, and what each answer does:

```
.be-code/tasks/003-add-palette.md changed since this session read it
  another session (pid 48231, feat/thing) wrote it 40 seconds ago

  y  keep mine   — write this session's version; theirs is saved to
                   be-code/pre-overwrite/20261009-210255
  n  keep theirs — abandon this write and re-read theirs
  d  show me     — print the difference and ask again
```

`d` is the third option your answer asked to spell out, and it is why this is
an approval with a diff rather than a yes/no: `internal/diff` already renders
one, and `fs.go`'s `file_write` approval already shows one, so a person can
see what they are choosing between instead of guessing.

`-y` does not answer it, for the same reason it does not answer
`pack_install`: "run the commands I chose" is not "discard the other
session's work". Under `-y` it takes the §9 path — snapshot, write, say so —
which is what `-y` actually means here: do not stop, and do not lose anything.

## 9. Restore points

**Workspace files** — anything under the workspace, which is where overwriting
actually costs something: `.becode/packs/<name>/`, `.be-code/tasks/*.md`,
`CHANGELOG.md`, source. `gitctx.Snapshot` commits the whole working tree to a
fresh branch through a private `GIT_INDEX_FILE`, never moving HEAD, the index
or the checkout, and returns the branch name — the mechanism `/init` has used
since 0.7.1.

It needs one change: `SnapshotPrefix` is a const (`be-code/pre-init/`), so it
becomes a parameter.

```go
const (
    SnapshotPrefixInit      = "be-code/pre-init/"
    SnapshotPrefixOverwrite = "be-code/pre-overwrite/"
)

// SnapshotTo is Snapshot with the branch prefix named by the caller.
func SnapshotTo(ctx context.Context, root, prefix, message string) (string, error)
```

`Snapshot` stays, calling `SnapshotTo` with the init prefix, so `/init`'s
callers and tests do not move.

The notice names the branch and the line that restores it, as `/init`'s does:

```
note> wrote .be-code/tasks/003-add-palette.md over another session's change;
      theirs is on be-code/pre-overwrite/20261009-210255
      git restore --source=be-code/pre-overwrite/20261009-210255 --staged --worktree -- .be-code/tasks/003-add-palette.md
```

**Outside a git repository** there is nothing to snapshot to, so the old
version goes to a sidecar (below) and the notice says so. A workspace that is
not a repository is a real case — `/init` handles it the same way, keeping
`BECODE.md.bak`.

**Dotdir files** — `~/.be-code/engine/<key>/packs.json`, `schedules.json`,
`sessions/`. These are in no git tree, so "mark it in the git tree" cannot
apply literally. The old version is copied to
`<file>.overwritten-<yyyymmdd-hhmmss>` beside it, 0600, and sidecars older
than **seven days** are pruned when the directory is next written. Seven days
because the thing a person does with a lost decision is notice it the next
time they open that workspace, which is days, not minutes.

The alternative considered and rejected: making `~/.be-code/engine/<key>/` a
git repository of its own, for one uniform restore story and real history. It
is a repository per workspace key, a new class of failure in the dotdir, and
`git` becomes a hard dependency of writing state rather than of reading a
project. Sidecars are proportionate to files of a few hundred bytes.

## 10. Where detection is wired

Three places, and no others in A:

1. **`internal/packs/trust.go:mutate`** — compare-and-retry (the race), and
   nothing else: every writer of that file applies a delta.
2. **`internal/tools/fs.go`'s write path** — after approval and before
   `OnBeforeWrite`, so a conflict is caught before the checkpointer snapshots
   a file that is about to be overwritten anyway. This is the one that
   protects task documents, pack directories and source.
3. **`internal/store`'s session save** — which already has half of this:
   `Session.HostPID` detects a different live host owning the file and
   disables autosave for the run. That guard stays as it is; A adds the
   notice naming which session owns it, from the work record, because
   "owned by live host 48231" is a pid and the record can say what it is
   doing.

`schedules.md` is **not** in A. Its own design already reloads and merges on
every save, and its approvals file is keyed per schedule with a hash of what
was approved — the same pattern, arrived at independently. A notes this as
prior art rather than changing it.

## 11. Configuration

One key, following `PacksConfig`'s pattern:

```go
// WorkConfig covers the record of what other sessions are doing.
type WorkConfig struct {
    // Enabled writes this session's record and reads others'. Off means no
    // record, no startup notice, and no conflict detection — the behaviour
    // before this feature.
    Enabled bool `json:"enabled"`
    // KeepOverwritten is how long a dotdir sidecar is kept, as a duration.
    KeepOverwritten string `json:"keep_overwritten"`
}
```

Defaults: `enabled: true`, `keep_overwritten: "168h"`. Documented in the
README config reference.

## 12. Failure modes

Every one of these keeps the session running: a work ledger that can fail a
session is worse than no work ledger.

| Condition | Behaviour |
|---|---|
| `~/.be-code/work/` not writable | No record, no notice, detection still works in memory. One notice saying so. |
| A record that will not parse | Ignored and removed, like a pruned live record. |
| A record whose pid cannot be queried | Counted alive (§4). It is advisory; a false "someone is there" costs a notice, a false "nobody is there" costs work. |
| `gitctx.SnapshotTo` fails | The write does **not** proceed unattended. Unattended overwriting is only acceptable *because* it is recoverable; with no restore point it becomes the thing §7 refuses. A person is asked if there is one, and it errors if not. |
| A sidecar cannot be written | Same rule, same reason. |
| The clock goes backwards (a sidecar name collides) | Suffix with a counter, as `gitctx` already does for a branch name. |
| Two sessions conflict on the same file at once | Each sees the other's change; whoever writes second gets the prompt. No ordering is promised beyond "nothing is lost silently". |

## 13. Testing

- A fake clock for staleness and sidecar pruning, as `internal/schedule`'s
  tests use.
- Two stores over one path for the race: the test drives the interleaving
  directly rather than hoping for it, as `internal/agent/schedules_review_test.go`
  drives `begin`.
- `procattr.Alive` stubbed for liveness, including the "access denied counts
  alive" case.
- A real git repository in a temp directory for `SnapshotTo`, and a non-repo
  directory for the sidecar fallback — the pattern `internal/gitctx`'s own
  tests and `internal/packsrc`'s use.
- `work_conflict` named in all three approvers, with the guard
  `internal/tui`'s `TestEveryNoAlwaysActionHasItsOwnTitleAndHint` now
  provides.
- The fired-turn path: a conflict during a fired turn writes, snapshots, and
  notices, and never raises a prompt. `Registry.Fired()` is the test.
- `-y` does not answer `work_conflict`.
- A workspace that is not a git repository falls back to a sidecar and says so.
- `SameWorkspace`, not `==`: two spellings of one directory see each other.

## 14. Risks

- **A notice nobody reads.** The startup notice competes with every other
  startup notice. If it fires on an ordinary second terminal it will be
  ignored, and then the conflict prompt arrives with no context. Mitigation:
  it says what the other session is *doing* (branch, task), not merely that it
  exists.
- **The prompt in the wrong place.** §2's distinction is the whole design. If
  the mechanical race ever reaches `work_conflict`, people will learn to
  press `y`, and the feature becomes a worse version of silence.
- **Sidecars accumulating.** Seven days of overwrites in a busy dotdir is
  small, but it is litter in a directory people do not look at. The prune runs
  on write, so a workspace that goes quiet keeps its sidecars until it is used
  again.
- **`writing` is a turn behind.** A conflict detected mid-turn names the other
  session's *last* turn's files. It will occasionally say "pid N is writing X"
  when pid N has moved on. Honest cost of not writing a record per tool call.
