# Pack loader — what was decided along the way

Plan: `docs/superpowers/plans/2026-10-09-pack-loader.md`. Spec:
`docs/superpowers/specs/2026-10-09-pack-loader-design.md`. Branch
`feat/pack-loader`, from `main` at 1.2.5.

This is the record of the rulings made while the plan ran — decisions taken
without stopping to ask, each with why and what it costs if wrong — so the
owner can see them in one place and overturn any of them.

## What went wrong, and what changed because of it

**The plan contradicted the spec on the one consent seam the feature exists to
protect, and the contradiction shipped.** Spec §8 and §14 say `-y` never
answers `pack_install`, for the reason §8 states plainly: an install records
trust, which writing a pack directory by hand cannot do, so "a model that can
install a pack can rewrite its own instructions." The plan's own code comment
said the opposite — "`-y` here approves only this one command the person
typed" — and I transcribed it without noticing, and without a ruling.

The pre-flight scan this skill asks for compares task *interfaces* against
each other; it never compares task *text* against the spec. Both passes were
clean and both missed it. The fresh reviewer found it in minutes and
demonstrated the consequence: `be-code packs add x --source <url> -y` run
through `shell` under `AutoApproveShell` installs and trusts a pack with
nobody asked.

What changed: the `-y` branch is gone, the test that asserted the wrong
behaviour was replaced with one that drives the real approver through a
stubbed TTY and a piped `y`, and the refusal message no longer promises that
`-y` would work. The broader lesson is for the next plan: **where a plan's
prose explains why it diverges from the spec, that is the exact place to stop
and rule**, because an explanation is what a contradiction looks like when
you are the one who wrote both.

**A second miss had the same shape.** `packs.Checks` kept gating on the pack's
`detect` block, so the marker rung — the whole point of §7a — never reached
`be-code verify`. I met this twice while writing Task 6's fixture, added a
`detect` block and then a file for it to match, and ledgered both as fixture
problems. They were the product telling me the rung was missing, and I wrote
down the symptom instead of hearing it. The published `assist` pack is exactly
the case that breaks: no `detect` block at all.

What changed: one definition of the rung (`packs.MarkerDeclares`) and one of
the precedence (`packs.Wanted`), used by the agent, by `Checks` and by the CLI
listing — three places that had been answering one question three ways.

## What the design got right

The reviewer attacked the central claim — that committing a pack is safe
because the marker travels and consent does not — and could not find a path
where cloning a repository activates its pack without a prompt on that
machine. `copyTree` fails closed on a FIFO, an unreadable file, and a symlink
directory pointing outside the pack, leaving nothing behind in each case. The
executable bit survives a real clone. That part of the spec held up under
someone trying to break it.

---

# SDD ledger — plan: docs/superpowers/plans/2026-10-09-pack-loader.md

Spec: docs/superpowers/specs/2026-10-09-pack-loader-design.md (read; it is the
binding authority where the plan and it disagree).

Setup: Ruling: executing on the feature branch `feat/pack-loader` in the main
checkout rather than a git worktree, with the owner's consent to the branch
(they approved native execution on it). Last session's worktree pinned the
session's cwd inside a directory the merge then had to remove, which made the
merge impossible from the session that did the work. Cost if wrong: the main
checkout is occupied by this branch while the work runs.

Setup: superpowers:test-driven-development is loaded in this session (invoked
earlier, full text in context) and governs every step below.

Pre-flight: shared-interface rows, from the plan's Interfaces blocks.

| consumer | producer | what | found |
|---|---|---|---|
| T3 | T2 | Source, Kind, KindPath, KindGit, ValidPackName | exact match |
| T4 | T3 | Fetch, Fetched, Available, List | exact match |
| T4 | T1 | Marker, ReadMarker, WriteMarker, MarkerName, Untrust | exact match |
| T5 | T1 | ReadMarker, Marker.DeclaresWanted | exact match |
| T8 | T4 | Install, Update, Remove, Options, Result, PacksDir | exact match |
| T9 | T4 | Install, Update, Remove, Options | exact match |

Pre-flight: four omissions from Consumes blocks, none a conflict, all
satisfied by the plan's own task order:

- T4's install.go calls `packs.Now()`, which T1's code defines but T1's
  Produces block does not list. Order 1→4 covers it.
- T4's `move` calls `copyTree`, defined in T3's fetch.go. Same package and
  unexported, so it is not an interface, but it is a real ordering dependency:
  order 3→4 covers it.
- T4's tests use T3's `gitfixture_test.go` helpers (`packsRepo`, `packFiles`,
  `gitPacksRepo`, `mustWrite`, `git`, `trim`). T4's tests do not compile
  without T3's file. Order 3→4 covers it.
- T8 and T9 both read `Packs.Source` from config, which T7 adds, and neither
  Consumes block names T7. Order 7→8→9 covers both.

Pre-flight: no conflicts to rule on. Starting Task 1.
Task 1: complete (commits 6842f67..820c2f4, tests: go test ./internal/packs → ok  	github.com/brown-enterprises/be-code/internal/packs	0.882s)
Task 2: complete (commits 820c2f4..eff5fa6, tests: go test ./internal/packsrc → ok  	github.com/brown-enterprises/be-code/internal/packsrc	0.003s)
Task 3: complete (commits eff5fa6..dc95928, tests: go test ./internal/packsrc → ok  	github.com/brown-enterprises/be-code/internal/packsrc	0.137s)
Task 3: Note: verified the procattr.Hide guard can fail — removed the call, watched it go RED, restored it. A source-reading guard that cannot fail is decoration.
Task 4: complete (commits dc95928..e04f4cf, tests: go test ./internal/packsrc → ok  	github.com/brown-enterprises/be-code/internal/packsrc	0.239s)
Task 5: complete (commits e04f4cf..9b04145, tests: go test ./internal/agent → ok  	github.com/brown-enterprises/be-code/internal/agent	(cached))
Task 6: Ruling: the plan's test fixture declared `"detect":{"any_file":["**/*"]}`, but the manifest's detect block is `{"any":[…], "all":[…]}` (internal/packs/packs.go:82) — `any_file` is not a key, so it parsed to an empty detect, the pack matched nothing, and two tests failed with empty notes rather than with the behaviour under test. Corrected the fixture to `{"any":["**/*"]}`. Cost if wrong: the test exercises a pack that matches every workspace, which is what it needs; if `**/*` ever stops matching, these two tests go empty-notes again and say so loudly.
Task 6: Ruling: the same fixture also needed an ordinary file in the workspace. Detection walks the workspace and skips `.becode` (discover.go's skipDir), so a workspace holding nothing but the pack has nothing for a detect glob to match, and the tests failed with empty notes rather than with the behaviour under test — the pack was never a candidate. Added a `main.go` to the fixture workspace, with the reason in a comment. Cost if wrong: nothing; it makes the fixture a workspace rather than a pack sitting in an empty directory.
Task 6: complete (commits 9b04145..502b121, tests: go test ./internal/packs ./cmd → ok  	github.com/brown-enterprises/be-code/cmd	1.185s)
Task 7: Ruling: the plan named one site in headlessApprover (cmd/commands.go:277) but there are **two**: the `-y` group, and the non-TTY branch at :289 whose message promises "use -y to auto-approve". Leaving pack_install out of the second makes that hint a false promise — -y never approves it. Named it in both. Cost if wrong: nothing; the second list is exactly the set of actions whose hint must not mention -y.
Task 7: Ruling: the plan's cmd test asserted only that the approver returned false, and it **passed before the implementation** — an unclassified action falls through to the non-TTY branch and is refused there for an unrelated reason. A test that cannot distinguish the branches proves nothing about the classification, so it now captures stderr (reusing cmd/engine_test.go:64's captureStderr) and asserts the refusal says "refused pack_install" and never promises -y. A second test covers the no--y non-TTY path. Added a direct classification test in internal/ui for noAlwaysAction. Cost if wrong: three assertions on message text, which would need updating if those messages are reworded.
Task 7: complete (commits 502b121..1d5e95b, tests: go test ./internal/config ./internal/ui ./internal/tui ./cmd ./internal/packsrc → ok  	github.com/brown-enterprises/be-code/internal/packsrc	0.232s)
Task 8: Ruling: the plan's test harness called `packsAddCmd.Execute()` with `SetArgs`, which does not work — cobra's Execute() on a subcommand delegates to the root command and ignores the child's args, so RunE ran with go test's own argv and produced empty output. Rewrote runPacksAdd to call `cmd.RunE(cmd, positional)` directly with this package's flag vars set as setup, which is how cmd/packs_test.go already drives packsValidateCmd. Cost if wrong: the test exercises RunE rather than cobra's flag parsing, so a mis-registered flag would not be caught here — `go build` and the end-to-end run in Step 5 cover that instead.
Task 8: Ruling: `stdinIsTTY()` reports **true** under `go test`, so the non-interactive refusal test took the interactive path and declined at EOF — the right outcome by luck, for the wrong reason and with the wrong message. Stubbed `stdinIsTTY`, which cmd/root.go:113 says is a var for exactly this. Cost if wrong: nothing; it is the documented seam.
Task 8: Step 5 ran end to end against the real public repository. `packs add assist` cloned https://github.com/BE-AI-Research/be-skill-packs, installed 0.1.0, and wrote a marker naming commit 45f1be82d2ffafc81848ae1d6690f22273dd44cd — the exact commit pushed earlier today. `packs validate` prints ok, `tools/scan` arrived as -rwxrwxr-x so the executable bit survived the clone, `packs update` reports "already at 45f1be8", and `packs remove` leaves the packs directory empty. Run under a throwaway HOME; ~/.be-code/config.json untouched.
Task 8: Observation for the final review (not fixed, out of this plan's scope): `be-code packs` lists an installed pack without the `*` marker, because that listing's `*` means "matches this workspace" and is computed from the pack's detect block alone — the marker rung added in Task 5 lives in the agent's LoadPacks, which is what `/packs` in a session reports. A person who installs a detect-less pack and then runs `be-code packs` sees no `*`. The legend does say "a session's own on/off state is shown by /packs", so it is not a false statement, but the two listings now disagree about what "wanted" means.
Task 8: complete (commits 1d5e95b..34cd714, tests: go test ./cmd → ok  	github.com/brown-enterprises/be-code/cmd	1.179s)
Task 9: complete (commits 34cd714..b0dd487, tests: make -f build.mk verify → ok  	github.com/brown-enterprises/be-code/internal/verify	(cached))

Final review: fresh reviewer, Opus, high effort, on the whole-branch package (67bd7e3..b0dd487). It ran the built binary against throwaway fixtures and demonstrated its findings. 1 Critical, 5 Important, 15 Minor, 8 declined.

Final: Ruling: re-graded two of the reviewer's Minors to Important and fixed them, because each is a *wrong fact* rather than a rough edge. M3: `packOptions` used `filepath.Abs` where the rest of `cmd/` uses `mustAbs`, so a quoted `-C '~/proj'` installed into a literal `~` directory and keyed the trust file on that path — silently the wrong workspace, no error. M1: I had inserted `PacksConfig` between `SchedulesConfig`'s doc comment and its type, so the comment documented the wrong struct and `SchedulesConfig` had none. Cost if wrong: two small changes nobody asked for.
Final: fixed C1 (`-y` answered `pack_install` on the only path that raises it — `packsApprover`; `headlessApprover` is wired for `run`/`init`, neither of which installs). The spec says in §8 and §14 that `-y` never answers it, the plan's own comment contradicted the spec, and I followed the plan **without a ruling** — the pre-flight scan compares task interfaces, not task text against the spec, and this slipped through both. The reviewer demonstrated the consequence: a model running `be-code packs add x --source <url> -y` through shell under `AutoApproveShell` reaches the trust file, which is the one capability §8 withholds, because an install *records trust* where a hand-written pack directory cannot. Removed the branch; replaced the contradicting `TestPacksAddInstallsWithYes` with `TestPacksAddInstallsWhenAPersonSaysYes`, which drives the real approver with a stubbed TTY and a piped `y`. `TestPacksApproverNeverAcceptsDashY` RED→GREEN, suite 33/33. Verified by hand afterwards: `packs add assist -y < /dev/null` declines and installs nothing.
Final: fixed I1 (`Checks` still gated on `p.Matches(workspace)`, i.e. detection alone, so the marker rung added in Task 5 never reached `be-code verify` — and `Pack.Matches` is false for a pack with no detect block, which the real `assist` pack is). This is the gap my own Task 6 rulings walked past twice: I shaped the fixture around it (adding a detect block, then a file to detect) and called it a fixture problem. There is now one definition of the rung, `packs.MarkerDeclares`, and one of the precedence, `packs.Wanted`, used by `LoadPacks`, `Checks` and the CLI listing. `TestChecksHonourTheMarkerForADetectLessPack` RED→GREEN, suite 33/33. The CLI listing's `*` and its legend were the same root cause and are fixed with it.
Final: fixed I2 (`--force`, and therefore `packs update`, ran `os.RemoveAll(dest)` before moving the new pack in; RemoveAll is not atomic, so a failure part-way left the destination gutted — reviewer demonstrated pack.json, core.md and the marker all gone, and I reproduced it). Now the old pack is renamed aside, the new one moved in, and the aside copy removed; `restore` puts it back on any failure before that point, and reports when it could not. `TestForceNeverLeavesHalfAPack` RED→GREEN, and I confirmed it fails on the old code by reverting the fix and watching it go red. Suite 33/33.
Final: Ruling: that test asserts the §12 invariant (the destination always holds a complete, loadable pack) rather than "the replace fails", because with rename-aside the replace now *succeeds* where RemoveAll failed — rename works on an undeletable subdirectory when its parent is writable. Asserting the failure would have pinned the weaker of the two good outcomes. Cost if wrong: nothing; the invariant is the thing §12 states.
Final: fixed I3 (the `pack_install` modal fell through to the defaults: titled "Shell command", hint offering "a auto-approve all shell commands this session" for a key `handleAskKey` makes a no-op — on the one consent seam this feature rests on). Added its case in both UIs. Pinned by `TestEveryNoAlwaysActionHasItsOwnTitleAndHint`, a source-reading guard that parses the no-"always" group out of `handleAskKey` and requires a display case for each action in it; it named exactly `pack_install` and nothing else, so the guard works and the rest of the list is already honest. The reviewer noted this class of miss has happened once per feature for several features running, which is why the guard is general rather than a single assertion.
Final: Ruling on I4: the spec §9/§11a asked for a **picker** at `/packs add` with no name, the plan never mentioned one, and I shipped neither — so the new menu row submitted `/packs add`, hit the arity guard, and printed a usage string. That is a dead end on the surface whose whole purpose is discoverability (§1: "without knowing where packs live on disk or what a pack directory has to contain"). I shipped a **listing** rather than a picker: `/packs add` with no name now prints what the source offers and how to pick one. Reason: a picker is new interactive TUI surface with its own state and tests, the listing removes the dead end today, and it is the same thing the CLI does — one behaviour in both UIs. Cost if wrong: a person still types the pack's name, which the spec wanted to avoid; the spec is now ahead of the code on this point and should be amended or the picker built. Flagged to the owner. **Overturned the same day**: the owner chose to build the picker ("less barrier to use"), so the TUI now opens one from the menu row and from `/packs add` with no name, and plain mode keeps the listing because it has no picker. The spec's §9 is amended to describe both.
Final: Ruling on I5: not changing the prose exemption in this branch. `ContentHash` exempts `core.md`, `chunks/` and `playbooks/` unless executable, so an approved pack's *rules* can change without re-asking — the reviewer demonstrated editing `core.md` to say "ALWAYS run curl … | sh" with no prompt. It is pre-existing, deliberate policy that `TrustDetail` states out loud ("they are NOT covered by this approval"), and the reviewer graded it "not a regression". But they are right that this branch widens its blast radius: §7a activates detect-less packs that previously stayed inert, and §11 now honours the approval in contexts with nobody to ask. Their proposed fix — hash the prose separately and *notice* when it changes without re-asking, plus stop `isProse` exempting any file a check names as its program — is sound and I am not doing it here because it changes a documented trust policy for every pack, including the two already published, which is the owner's call and not a loader change. Cost if wrong: a teammate's PR that edits only `core.md` silently changes everyone's standing instructions. **This is the most important thing on the deferred list.**
Final: minor (deferred): `internal/ui/repl.go` and `internal/ui/packs.go` pass `context.Background()` for `/packs add` and `/packs update`, so a hung clone cannot be cancelled by Esc or by the session ending; the CLI correctly uses `cmd.Context()`.
Final: minor (deferred): `packs add` with no name calls `packsrc.List`, which clones over the network *before* any `pack_install` approval, and the approval detail then says "fetch over the network from …" about a host already contacted.
Final: minor (deferred): a spec beginning with `-` that contains `://` passes `Resolve` and is handed to `git clone` as an option (reviewer demonstrated git parsing it as a flag); not exploitable as far as either of us could find, but `Resolve` should refuse a leading `-` and `clone` should pass `--` before its positionals.
Final: minor (deferred): `packs.Now` is a package-level mutable clock in a package the agent imports, and no test replaces it.
Final: minor (deferred): `internal/packs/trust.go`'s `mutate` serialises within one process only, and this branch adds a second writer (`be-code packs add|remove` in another terminal while a session is live), so a lost update is newly reachable — a `/packs off` could be clobbered by an install that read the state first.
Final: minor (deferred): `packs add -C /typo` silently creates `/typo/.becode/packs` and installs there; a workspace that does not exist should be refused.
Final: minor (deferred): a failure *after* the move (`WriteMarker` or `record`) leaves the pack at its final path with no marker and no trust; it fails safe but the error does not say a directory was created.
Final: minor (deferred): `/packs remove` deletes a committed directory outside the `file_write` seam and outside the checkpointer, so `/undo` cannot restore it; worth a "git checkout -- …" line in the output.
Final: minor (deferred): `locate` matches on the manifest name rather than the directory name, so `packs/x` whose pack.json says `assist` installs as `assist`, and two directories claiming one name pick the first in ReadDir order.
Final: minor (deferred): `source.go`'s third `switch` case (`name == "." || ".." || HasPrefix(".")`) is unreachable — `packName`'s first character class already excludes a leading dot.
Final: minor (deferred): `/packs add <name> <source>` accepts a positional source override that neither `packsUsage()` nor the `/packs` row documents.
Final: minor (deferred): the three new subcommands and three new slash verbs are documented nowhere in README beyond the `packs.source` key, and the repo-root CLAUDE.md has no packs section, so none of this feature's invariants (the marker/consent split, `.becode` vs `.be-code`, "no -y") are written where the next session reads them.
Final: Note: `stdinIsTTY()` returns true for `/dev/null` (it has ModeCharDevice), so `packs add … -y < /dev/null` takes the interactive path and declines at EOF rather than printing the non-interactive message. It fails safe, and the check is used repo-wide, so it is pre-existing rather than this branch's.
Final: fix pass complete, `make -f build.mk verify` green (33 packages, exit 0), ~/.be-code/config.json mtime 1791327107 unchanged throughout. No second review dispatched: every fix has a test that failed first, and the suite answers "broke nothing".

---

# I5 resolved (2026-10-10): the prose exemption

Owner's ruling, asked as one question: **notice, stay active** — not re-ask,
and not re-ask for `core.md` alone. `packs.ProseFiles` records each prose
file's hash beside the approval (`state.Prose`; `TrustWithProse` from the
`pack_trust` yes and from `packs add`); `Agent.noteProseChange` says once when
an approved pack's prose differs from the record, names the files, moves the
record up and leaves the pack active. An approval from before this gains a
record silently (`TestAnOlderApprovalGainsAProseRecordSilently`). The second
half of the reviewer's proposal is in too: a file a check names in its argv is
never prose, so `sh {pack}/chunks/run.md` re-asks when that chunk changes
(`TestContentHashCoversAFileACheckNames`). `TrustDetail` now says what the
approval covers and that a later change will be said, instead of "NOT
covered". Cost if wrong: a person who misses the notice is instructed by rules
they did not read; the alternative, a prompt per wording edit, is the habit the
gate most needs to avoid.
