# Pack loader: installing a skill pack into a project

Date: 2026-10-09
Status: implemented and merged to `main` 2026-10-09

## 1. What this is for

A skill pack is data a model reads: rules in the system prompt, chunks it can
ask for, verify commands, playbooks. BE-Code can already *load* packs. It has
no way to *get* one. Today a person clones
[`be-skill-packs`](https://github.com/BE-AI-Research/be-skill-packs) and runs
`cp -r`, which is undiscoverable, unversioned, and leaves nothing behind that
says where the pack came from.

This adds the missing half: a built-in installer that fetches a pack from a
source and puts it in **this project**, reachable from the TUI menu, the
session, and the CLI.

**Intended outcome.** Anyone using BE-Code can install a skill pack into a
project from the menu, without knowing where packs live on disk or what a pack
directory has to contain. The project then carries its own pack setup, so a
clone of it has the pack too.

**Who it is for.** Anyone who uses BE-Code — not only this machine's owner.
That makes the installed layout and the marker format a public contract, and
makes the trust story load-bearing rather than a formality.

**Owner's decisions, carried into this design.** Packs install **into the
project only** — "packs should not be global objects", so nothing here writes
to `~/.be-code/packs`. The installed pack is **committed**, so it travels with
the project. Trust **follows provenance**, recorded at install. The fetcher is
**git** (§4). Removal is a **directory delete** (§10). A **`packs.source`
config key** lets a team point every install at their own mirror.

## 2. What already exists, and is not rebuilt

Read this before adding anything: most of the "enable per project" question is
already answered, and the design's job is to not duplicate it.

| Capability | Where |
|---|---|
| Discovery from two roots: `~/.be-code/packs` (`SourceGlobal`) and `<workspace>/.becode/packs` (`SourceProject`) | `internal/packs/discover.go:35` |
| Auto-activation by a pack's `detect` block, overridable per workspace | `Pack.Matches`, `internal/agent/packs.go` |
| Per-workspace on/off, persisted | `~/.be-code/engine/<key>/packs.json`, `Forced map[string]bool` |
| Per-workspace trust of a project pack, keyed by content | same file, `Trusted map[string]string` (name → `ContentHash`) |
| The trust prompt, and the rule that content change revokes it | `trustProjectPack`, `internal/agent/packs.go:310` |
| Validation of a pack directory | `be-code packs validate`, `cmd/packs.go:60` |
| A GitHub fetch with integrity checking, and its test seam | `internal/update`, `APIBase` injectable |

Two consequences matter.

**Consent already lives in the right place.** `trustProjectPack` returns true
without asking when `TrustedHash(path, name) == ContentHash(packRoot)`. The
dotdir is per-workspace and does not travel with the repository. So a
provenance marker committed beside the pack cannot and must not be what grants
trust — the marker is evidence, the dotdir is consent. That is what makes
committing the pack safe: a teammate's clone has the marker and no dotdir
entry, so it asks them.

**A project pack's checks are currently second-class.** `GlobalChecks`
(`internal/packs/discover.go:275`) deliberately refuses to run a project
pack's verify commands for any caller with no agent — `be-code verify`,
headless runs — and its note says to copy the pack to the global directory.
Since this design rules the global directory out, that note becomes advice
nobody can follow, and §11 changes it.

## 3. Scope

In scope:

- Fetching a pack from a git remote, from a local path, or from the default
  source, into `<workspace>/.becode/packs/<name>/`.
- A provenance marker committed with the pack.
- Recording trust and per-project activation at install.
- `add`, `update`, `remove`, and a picker of what a source offers.
- TUI menu entry, slash commands, CLI subcommands.
- Running an installed project pack's checks for callers with no agent.

Out of scope, deliberately:

- Any global install path. Ruled out by the owner.
- Pack-to-pack dependencies. The format has none; adding them is a format
  change this feature does not need.
- A pack registry, index file, or search. The source is a git repository and
  its `packs/` directory is the index.
- Signature verification. A pinned commit plus `ContentHash` plus a prompt is
  the integrity story; signing keys are a separate design with its own
  distribution problem.
- Any ability for the **model** to install a pack. See §8.
- Publishing packs. That belongs to the packs repository.

## 4. Architecture: one package, one fetcher

A new leaf package, `internal/packsrc`: stdlib plus `os/exec` and
`internal/procattr`. It resolves a source, fetches from it, and hands back a
directory. It knows nothing about trust, the agent, or any UI, and nothing
imports it except `cmd/` and the UIs.

```go
package packsrc

// DefaultSource is where a pack comes from when nobody names a source.
// A test points it at a local repository, exactly as update.APIBase is.
var DefaultSource = "https://github.com/BE-AI-Research/be-skill-packs"

type Kind int
const (
    KindPath Kind = iota // an existing directory on this machine
    KindGit              // anything git can clone
)

type Source struct {
    Spec string // as given, or DefaultSource when the caller gave nothing
    Kind Kind
    Ref  string // a branch, tag or commit; "" means the remote's default
}

// Resolve classifies a source by inspection, never by guessing.
func Resolve(spec, ref string) Source

type Fetched struct {
    Dir     string // a temp directory holding exactly the pack
    Pack    string // the pack's name, from its pack.json
    Version string
    Commit  string // the resolved commit, or "" for a path source
}

// Fetch puts one pack in a temp directory. The caller removes Dir.
func Fetch(ctx context.Context, s Source, pack string) (Fetched, error)

type Available struct{ Name, Version, Description string }

// List is what a source offers, for the picker.
func List(ctx context.Context, s Source) ([]Available, error)
```

### Classification

`Resolve` decides by looking, so a typo cannot silently become a clone of
something unexpected:

1. `spec` empty → `DefaultSource`, `KindGit`.
2. `spec` names an existing directory → `KindPath`.
3. `spec` contains `://`, or matches `user@host:path` → `KindGit`.
4. Otherwise → an error naming what was given. A bare word that is neither a
   directory nor a URL is a mistake, not a repository.

### Fetching

`KindPath`: the directory is copied. A path source whose directory contains a
`packs/` subdirectory is treated as a checkout of a packs repository and
`packs/<name>` is taken from it; a directory that is itself a pack (it has
`pack.json`) is taken whole, and `pack` must match its name or be empty.

`KindGit`: `git clone --depth 1 [--branch <ref>] <spec> <tmp>`, then
`git -C <tmp> rev-parse HEAD` for the commit, then `packs/<name>` is copied
out. A clone shallow by one commit is a few hundred kilobytes for the packs
repository today; if that ever matters, `--filter=blob:none --sparse` narrows
it without changing anything here.

**Every child process goes through `internal/procattr.Hide`.** The session
host is a detached process with no console, so an unhidden `exec.Command`
opens a visible console window on Windows. A test in `internal/tools` reads
source and fails on an `exec.Command` without it; this package gets the same
test.

`git` is already a hard dependency of BE-Code (`internal/tools/gittools.go`,
`internal/gitctx`, `verify`, `discover`), so requiring it here adds no new
prerequisite. Its absence is reported as itself: `git is not on PATH; a pack
can still be installed from a local directory with --source <dir>`.

## 5. Where a pack lands

`<workspace>/.becode/packs/<name>/` — the existing `SourceProject` root. Note
the two different dot-directories, which are easy to confuse and already
documented in `CLAUDE.md`: `.be-code/` is the task engine's, `.becode/` is
packs and custom commands. A pack written to the wrong one is never found.

Nothing in this feature writes to `~/.be-code/packs`.

## 6. The marker

Written beside the pack's own files as `.pack-source.json`, and committed with
them:

```json
{
  "pack": "assist",
  "version": "0.1.0",
  "source": "https://github.com/BE-AI-Research/be-skill-packs",
  "ref": "main",
  "commit": "45f1be8c9e2a1d4f6b8c0a3e5d7f9b1c2e4a6d80",
  "installed": "2026-10-09T16:12:04Z"
}
```

It carries **no checksum of the pack**, for two reasons. It would be circular:
`ContentHash` hashes every file at the pack root that is not prose, so it
hashes the marker. And leaving the checksum out is strictly better — the
marker is *inside* the hash, so editing it to forge a provenance claim changes
the hash and voids consent on its own.

Install order is therefore fixed: write the pack, write the marker, hash the
result, record the hash. Any later edit to either the pack or its marker
breaks the match.

`ref` is kept alongside `commit` because a ref is what a person asked for and
a commit is what they got; `packs update` needs the first and `packs` listing
should show the second.

## 7. Trust and activation

| Who put the pack there | What happens |
|---|---|
| This person, through the installer | After a successful install: `packs.Trust(path, name, hash)` against `~/.be-code/engine/<key>/packs.json`. No trust prompt follows — the install *was* the consent. Enablement comes from the marker instead (§7a), so nothing per-workspace has to be written for the pack to be on. |
| A clone of someone else's repository | The marker is present, the dotdir entry is not. `trustProjectPack` asks, unchanged. |
| Anyone who edits the committed pack | `ContentHash` no longer matches, consent is void, the next activation asks. |

## 7a. The marker is the project's declaration, so enablement survives a move

Installing a pack into a project is an unambiguous statement that this project
wants it, and the marker is already committed in the project. So the marker —
not a dotdir entry — is what says the pack is wanted.

**Activation precedence**, replacing today's two-step:

1. An explicit per-workspace choice, if one is set: `Forced[name]` true or
   false, from `/packs on` and `/packs off`. A person's direct instruction
   wins over everything.
2. Otherwise, a **valid marker** in the pack directory: the pack is wanted.
3. Otherwise, the pack's own `detect` block, exactly as today.

A marker is valid when it parses and its `pack` field is the pack's own name.
Nothing more: the marker declares intent, and trust is a separate question
answered by §7. `/packs default <name>` clears an explicit choice and returns
the pack to rungs 2 and 3.

This is why the rung ordering matters rather than being a detail. Without it,
moving or renaming a project orphans its `~/.be-code/engine/<key>/` entry —
the key is a SHA-256 of the absolute path (`internal/engine/store.go:267`) —
and a pack with no `detect` block goes silently inactive while sitting right
there in `.becode/packs`. `/packs` reports it as `does not match this
workspace`, which is true and useless. With the marker as rung 2, a move costs
one trust prompt and nothing else.

**It opens no hole.** A repository can ship a marker to declare its pack
wanted, but wanting is not trust: an untrusted pack still raises
`pack_trust` before any of its rules reach the system prompt, and its checks
are still withheld by §11. The marker can move a pack from "inactive" to
"asks you", never to "active".

## 8. The install approval, and who may install

A new approval action, **`pack_install`**, raised once per install before
anything is fetched. Its detail names the source, the ref, the pack and the
target directory, so the person approving sees what will be contacted and
what will be written.

Following `CLAUDE.md`'s rule for a new action, it is named in the TUI's `a`
handler, the REPL's `noAlwaysAction`, and `headlessApprover`. It has **no
"always"**, and `-y` never answers it — the same treatment the updater's
`update` action gets, and for the same reason: it reaches the network and then
writes rules into the model's own system prompt.

**The model cannot install a pack.** There is no tool for it. The only
entry points are the menu, `/packs add`, and the CLI, each of them a person's
own act. A fired scheduled turn refuses, like `AddSchedule` does, because the
prompt would have no deadline and nobody to answer it. This is the one place
in the design where a capability is deliberately withheld rather than gated:
a model that can install a pack can rewrite its own instructions.

## 9. Surface

**CLI** (`cmd/packs.go`):

```
be-code packs add [<name>[@<ref>]] [--source <path|git-url>] [-C <dir>]
be-code packs update [<name>]
be-code packs remove <name>
```

`add` with no name lists what the source offers and exits asking for one, so
the non-interactive path never guesses.

**Session**: `/packs add [<name>[@<ref>]]`, `/packs update [<name>]`,
`/packs remove <name>`. All three are added to `ui.IsPacksBlocking`, which
already lists every subcommand that can raise a question: `Session.Ask`
relocks the mutex `Update` holds, and an inline question there froze every
attached terminal with the alt screen up — the deadlock found in the last
review. `/packs add` with no name opens the picker.

**Menu** (`internal/tui/palette.go:menuEntries`): one row,
`{"Packs", "Install a skill pack", "fetch a pack into this project", cmd("/packs add")}`.

**The picker** reuses the existing `picker` component, built from
`packsrc.List`. Rows are name, version and description, read from each
`pack.json` in the source's `packs/` directory. Its loader runs on
`openPicker`'s Cmd — Bubble Tea's goroutine, not `Update` — which is what
makes the clone inside it safe, and a pick hands the install to `packsCmd`,
the same off-thread path `/packs add <name>` takes, because the install
raises `pack_install` through `Session.Ask`.

Plain mode has no picker, so there `/packs add` with no name lists what the
source offers and says how to pick one. Both UIs answer the same question;
only the TUI can answer it with a cursor.

**`ui.SlashCommandTable`**: the `/packs` row's description gains the three
verbs, so the palette, the menu's autocomplete and both UIs stay in sync.

## 10. Update and remove

`update` re-fetches at the marker's `ref`, validates, and replaces the pack
the same way an install does, then re-records trust and reports the commit it
moved from and to. A pack with no marker — one that arrived in a clone — is
refused with that as the reason: there is nothing to say where it came from.

`remove` deletes the pack directory and clears that pack's `Trusted` and
`Forced` entries. It uses `os.RemoveAll`, which is `rm -rf`'s semantics
without a shell: Windows has no `rm`, and putting a path through a shell adds
an injection surface for no gain. The two dotdir keys are cleared because
leaving them is stale state that a later reinstall or a same-named pack from
elsewhere would inherit. `ClearForced` already exists; **clearing a trust
entry does not, and is the one function this feature adds to `trust.go`**:

```go
// Untrust forgets a pack's approval for this workspace. Removing a pack
// without it leaves an approval for content that is no longer there.
func Untrust(path, pack string) error
```

It is written with the same read-modify-write-under-lock shape as
`ClearForced`, and like it, a missing file or a missing key is not an error. Nothing else is touched — the person's `.gitignore`
and git index are theirs, and a removed pack that was committed is a deletion
they commit.

## 11. Running an installed pack's checks

`GlobalChecks(packsDir, workspace)` becomes:

```go
func Checks(packsDir, workspace, trustPath string) (checks []verify.Check, notes []string)
```

A project pack whose `ContentHash` matches `TrustedHash(trustPath, name)` has
its checks returned. One that does not still gets today's note, reworded: the
pack needs approving, which happens by running BE-Code interactively once, and
no longer suggests copying it anywhere.

The trust path is a **parameter** rather than computed inside, so
`internal/packs` does not import `internal/engine` and stays a leaf. Its one
caller, `cmd/commands.go:672`, computes it from the workspace.

## 11a. Everything this feature adds or changes

So the implementation has nothing to infer:

**New**

| Thing | Where |
|---|---|
| `internal/packsrc` — `Resolve`, `Fetch`, `List`, `DefaultSource`, `Source`, `Fetched`, `Available` | new package |
| `packs.Untrust(path, pack string) error` | `internal/packs/trust.go` |
| `config.PacksConfig{Source string}` + its default | `internal/config/config.go` |
| `pack_install` approval action | TUI `a` handler, REPL `noAlwaysAction`, `headlessApprover` |
| `packs add` / `update` / `remove` subcommands | `cmd/packs.go` |
| `/packs add` / `update` / `remove` | `internal/ui/packs.go:PacksCommand` |
| The install picker | reuses `internal/tui`'s `picker` |
| Menu row **Install a skill pack** | `internal/tui/palette.go:menuEntries` |
| `.pack-source.json` marker format | written by the installer; read by `update`, the `packs` listing, and the activation precedence (§7a) |

**Changed**

| Thing | Change |
|---|---|
| `packs.GlobalChecks(packsDir, workspace)` | becomes `packs.Checks(packsDir, workspace, trustPath)`; honours recorded trust; its note no longer suggests copying the pack anywhere |
| `cmd/commands.go:672` | the one caller, computes the trust path |
| `ui.IsPacksBlocking` | gains `add`, `update`, `remove` |
| Pack activation | gains the marker as a rung between an explicit `Forced` choice and `detect` (§7a) |
| `ui.SlashCommandTable`'s `/packs` row | gains the three verbs |
| README config reference | gains `packs.source` |

**Untouched**, and worth stating because the temptation exists: `Pack.Matches`
and the `detect` mechanism, `trustProjectPack`'s prompt, `ContentHash`'s
coverage rules, and anything under `~/.be-code/packs`.

## 12. Failure modes

Every one of these is an error that explains itself and leaves nothing behind:

| Condition | Behaviour |
|---|---|
| `git` absent, git source | Error naming git, and suggesting `--source <dir>`. |
| Host unreachable, clone fails | The clone's own stderr, trimmed, as the cause. |
| Pack not in the source | Error listing what the source does offer. |
| The fetched directory is not a valid pack | Refused by the same validation `packs validate` runs, with its diagnostics. No install. |
| A pack of that name is already installed | Refused, pointing at `packs update`, unless `--force`. |
| Install interrupted | The pack is assembled in a temp directory and moved into place only once valid; a partial fetch is discarded. A rename across filesystems falls back to copy-then-remove. |
| `.becode/packs` not writable | Error naming the path. |
| Approval refused | Nothing is fetched and nothing is written. |

**Atomicity is the rule**: validate in temp, then move. There is no state in
which `.becode/packs/<name>/` holds half a pack.

## 13. Configuration

One new key, following `SchedulesConfig`'s pattern:

```go
// PacksConfig covers installing packs. Discovery and activation need no
// configuration: they follow the workspace.
type PacksConfig struct {
    // Source is where `packs add` fetches from when no --source is given.
    // A team pointing this at their own mirror makes every install in every
    // workspace come from it.
    Source string `json:"source"`
}
```

Default: `https://github.com/BE-AI-Research/be-skill-packs`, set in
`config.Default`, documented in the README's config reference. An empty value
means the built-in default rather than an error, so clearing the key is not a
way to break installs.

## 14. Testing

**No test reaches the network.** `DefaultSource` and `PacksConfig.Source` are
both injectable, and the git path is exercised against a real local repository
built in a temp directory — the pattern `internal/verify` already uses to test
against the real Go toolchain. Nothing is mocked that can be built for real.

`internal/packsrc`:

- `Resolve` classifies each of: empty, an existing directory, an `https://`
  URL, a `git@host:path` spec, and a bare word that is none of them.
- A path source installs, from both a packs-repository checkout and a bare
  pack directory.
- A git source installs from a local repository, and `Fetched.Commit` is that
  repository's `HEAD`.
- `@<ref>` installs the content at a tag, not at the default branch.
- `List` reports every pack in a source with its version.
- A source missing the named pack errors and names what it has.
- `git` absent (PATH without it) errors naming git, not "not found".
- The `procattr.Hide` source check, as `internal/tools` has.

Install, trust and activation (`internal/packs`, `cmd`):

- An invalid pack is refused and `.becode/packs` is left untouched.
- A successful install writes the marker, and `ContentHash` covers it.
- After install, `TrustedHash` matches, so `trustProjectPack` does not prompt.
- The same pack directory with no dotdir entry — a clone — does prompt.
- Editing one byte of the installed pack makes it prompt again.
- Editing the marker makes it prompt again.
- After install the pack is active with **no** `Forced` entry written, because
  the marker is what declares it wanted.
- A pack with a marker and no `detect` block is active; the same pack with its
  marker removed is not.
- `/packs off` beats a marker; `/packs on` beats a pack whose `detect` does not
  match; `/packs default` returns it to the marker.
- A pack whose dotdir entry is deleted — the move-or-rename case — is still
  active, and asks for trust once.
- A marker naming a different pack than the directory is not a valid
  declaration and does not activate anything.
- `remove` deletes the directory and both dotdir keys.
- `update` on a pack with no marker is refused with that reason.
- `Checks` returns an installed project pack's checks, and withholds an
  unapproved one's with a note.
- A refused `pack_install` writes nothing.
- `-y` does not answer `pack_install`; `headlessApprover` refuses it.
- A fired turn refuses an install.

## 15. Shelved: per-user pack profiles

Raised during design and **deliberately out of scope for v1**: a per-user
"profile" — a save file referenced only by that user on the system — recording
which packs they want, so the choice survives a workspace moving and follows
them between projects. PAM was suggested as the way to identify the user.

Kept out of v1, with the reasoning, because it will come up again:

- **The dotdir is already per-user.** `config.Dir()` is `os.UserHomeDir()` +
  `.be-code`, so `packs.json` is already a file only that OS user reads. The
  separation the idea asks for exists; what it would add is a different *key*,
  not a different owner.
- **PAM is a poor fit here.** It means cgo and a platform-specific dependency
  in a tool that is offline-first, stdlib-leaning, and cross-compiled to
  linux/amd64, linux/arm64, windows/amd64 and darwin/arm64 from one `make
  release`. And it would re-authenticate someone the OS has already
  authenticated to reach `$HOME`.
- **BE-Code's existing notion of a "user" is not an auth principal.**
  `internal/inbox`'s identity is derived from a terminal's IP with the ARP
  table consulted best-effort (`internal/inbox/mac.go`). It exists to address
  chat messages, and it is explicitly not a basis for a trust decision.
- **A per-user trust grant would be a real hole.** Pack names are neither
  unique nor authenticated. "This user trusts a pack called `assist`" applied
  across projects would mean cloning any repository containing a pack of that
  name inherits the grant. Trust has to stay bound to content, which is what
  `ContentHash` does today.
- **Per-client packs are impossible by construction.** A pack's rules live in
  the system prompt, and a shared live session has one system prompt for one
  agent however many terminals are attached. Two people in one session cannot
  have different packs; only separate sessions can.

What remains worth solving, if the idea returns: `~/.be-code/engine/<key>/`
keys on a SHA-256 of the **absolute workspace path**
(`internal/engine/store.go:267`), so moving or renaming a project orphans its
entry. The fix is a stable project key — the repository's root-commit SHA, with
the path hash as the fallback outside a repository — and it is independent of
users, profiles and PAM. The same orphaning affects the task engine, so it is
its own change rather than part of this one.

## 16. Risks

- **A clone's pack is trusted by its author, not by you.** The design's whole
  safety rests on the dotdir not travelling. A future change that put trust
  in the repository would silently hand every cloned repository's pack rules
  into the model's system prompt. The tests above pin both directions.
- **`--force` is a loaded gun.** It replaces a pack a project may depend on.
  It refuses nothing except the validation, and it is a flag rather than a
  prompt because it is a person's own CLI act.
- **A moving ref is not a pin.** Installing at `main` records the commit it
  resolved to, and `packs update` is what moves it; but the marker's `ref`
  being a branch means two people installing "the same" pack a week apart get
  different content. The listing shows the commit for exactly this reason.
- **Shallow clone of a growing repository.** Fine at three packs. If the
  packs repository grows large, narrow the clone (§4) rather than changing
  the design.
