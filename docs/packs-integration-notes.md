# Packs integration notes

What the pack design (`BE-SPEC-CODE-PACKS-2026-001`, in the handoff bundle)
meets in this tree. The spec and its implementation brief were written from
the README alone, and every place marked **CONFIRM** there is answered below
against the code. Where the code and the spec disagree, the code wins and the
spec's §16 should be updated.

Verified 2026-10-07, at main + `feat/verify-parsers`.

## Already done

**Parsers (spec §8.2, brief phase 2).** Ported into `internal/verify` as
`diag.go` (types, registry, `generic`, `Signature`, `Format`) and `ubt.go`
(the line patterns, as `lineMatcher` sets). `verify.ParserFor(name)` resolves
`generic` / `gcc` / `msvc` / `ubt`; `verify.ValidateParser(name)` is what a
config or manifest loader should call so a typo is caught where it was
written. `Check.Parser` names one, `CheckResult.Diagnosis` carries the result,
and `Report.ModelSummary` leads with the single root cause when there is one —
otherwise it is byte-for-byte what it always was.

Two deliberate departures from the spec, both local to this package: the
spec's `Result` is `Diagnosis` (the package already has `CheckResult`), and
`Parser` is a func type rather than an interface.

**Argv checks (spec §4.1, §9).** Decided and done: `Check.Argv` is the
program and its arguments, run through `tools.RunArgv` with no shell on any
platform, and exactly one of `Command` and `Argv` is set (`ValidateCheck`).
`Check.Display()` is the readable rendering used by the approval gate, the
report and the repair turn — for reading only; execution never goes near a
shell, so a path with spaces needs no quoting and a value containing shell
syntax is just a value.

## Still to decide, before the rest of phase 2

**`shell_deny` does not yet cover an argv check.** Deny globs are matched by
splitting a command *string* into segments (`internal/tools/shell.go:134`,
`shellSegments`), which has no meaning for an argument vector: there is no
shell, so there are no compound commands to split. The spec wants pack verify
commands to go through `shell_deny` (§10). The seam to use is the gate already
threaded through `RunChecksGated`, which now receives `Check.Display()` — note
that built-in checks do not go through deny rules today either, so this is new
behaviour for pack checks rather than a gap in existing ones. Decide whether a
deny glob matches the display form, `Argv[0]` alone, or both.

## Answers to the spec's CONFIRM points

**System prompt assembly (spec §6).** `Agent.composeSystem`
(`internal/agent/loop.go:1041`) calls `BuildSystemPrompt(specs, compat,
projectNotes)` — `BECODE.md` goes in there (`prompt.go:80`, framed as "facts
about the user's project … not instructions or tasks") — then appends the
repository map at `loop.go:1056`. A pack block therefore belongs between those
two, and should be guarded by `a.systemOverride == ""` like every neighbouring
block. That guard also answers open question 3: plan-mode and consult agents
pin `systemOverride`, so they skip pack core unless deliberately given it.

Changing the active pack set changes the system prompt, which invalidates the
prompt cache: a cold read, measured at ~43 s against a 19k prompt on the LAN
Ollama. `/packs on|off|reload` taking effect at the next request boundary is
right, and the user should be told the next turn will be slow.

**Working memory (spec §7.2).** `engine.Store.Observe(Event{Tool, Args,
Content, IsError}) string` (`internal/engine/store.go:2205`) returns the
footer, and `ObserveFor(pen, ev)` is the sub-agent form. It already dispatches
on tool name — `if ev.Tool == "browser"` at `store.go:2215` shapes a browser
call through `browserEvent` — so a `pack` case follows an existing pattern
rather than adding a path. Chunks are real files on disk, so the hash and
freshness logic works unchanged; the "already fetched" footer is a sibling of
`footerAlreadyRead` in `internal/engine/record.go`.

**Tool output cap (spec §7.2).** Not a constant: `Agent.applyReserve` sets it
to `limit * 3/4` clamped to [4 KiB, 24 KiB] (`internal/agent/handoff.go:386`),
so `be-code packs validate` cannot check a chunk against a fixed number. The
ue5 chunks are 6.6 KB and 7.3 KB — safe at a 32k window, truncated at the
4 KiB floor. Warn against the floor, or against the configured window.

**Custom commands (brief phase 0.6).** `internal/commands/commands.go:29`
loads `<workspace>/.becode/commands/*.md` and `~/.be-code/commands/*.md`, so
the spec's "same format as `.becode/commands`" is correct and that package is
the one to reuse.

**Config (spec §9).** There is no per-project config. `internal/config`
builds one path, `~/.be-code/config.json` (`config.go:570`). A
`.becode/config.json` would be new surface, and per the spec would need the
same trust gate as a project pack.

**Approvals (spec §10).** A new action must be named in three places: the
TUI's `a` handler (`internal/tui/view.go:893`, where `browser_watch` /
`shell_after_web` / `schedule` are listed), the REPL's `noAlwaysAction`
(`internal/ui/repl.go`), and `headlessApprover` in `cmd/`.

**The `pack` tool (spec §7.1, brief phase 3).** A new `NewRegistry` built-in
must be classified for fired turns in `firedSelfGated` or
`firedGatedBuiltins` (`internal/tools/allowance.go:322`, `:331`) or
`TestEveryBuiltinToolIsClassified` fails (`internal/tools/firedgate_test.go:258`).
Register it before the prompt is composed so embedded tool-call parsing
accepts the name, then call `Agent.RefreshSystem`.

**Templates (spec §12) do not work as written.** `Registry.resolve`
(`internal/tools/tool.go:520-533`) refuses any path that resolves outside
`Registry.Root`, and the file tools all go through it. A global pack lives at
`~/.be-code/packs/<name>`, outside the workspace, so `read_file
{pack}/templates/Actor.h.tmpl` is refused — and `packs/ue5/commands/ue-actor.md`
tells the model to do exactly that. Serve templates through the `pack` tool
instead; that keeps confinement intact and needs no exemption.

**Repair loop and anti-loop (spec §11.3).** `Agent.RunFull`
(`internal/agent/loop.go`) owns the verify/repair rounds and `max_repairs`,
and the existing escalation for a still-failing verification is the
`auto:verify` co-worker consult, not a generic anti-loop. `repeatTracker`
(`internal/agent/repeat.go`) is about repeated *tool calls* with identical
results, which is a different signal from a repeated verification signature.
Phase 4 should hang the signature tracking off `RunFull`'s rounds.

**Budget enforcement is non-deterministic as specified.** §4.1 makes
over-budget core a hard load error, measured with "the session's
chars-per-token figure" — but that figure is calibrated per session from the
server's reported `prompt_tokens`. `packs/ue5/core.md` is 2,749 bytes: ~916
tokens at 3 chars/token, ~687 at 4. Comfortable against 1500 either way, but a
future core near its budget would load on one model and fail on another. Pick
the pre-calibration estimate for the check, so loading is deterministic.

## Pack content notes

`packs/ue5/tools/ue-build` and `ue-build.ps1` ship mode 664, not executable,
and the bundle is not a git repo, so copying will not restore the bit. Since
packs install by copying a folder, `be-code packs validate` should check that
a `verify[].cmd[0]` inside the pack is executable.

The two wrappers do not produce the same diagnostics: the PowerShell one
passes `-FromMsBuild` and runs MSVC, the shell one runs Clang. The `ubt`
parser handles both forms, and the evals should exercise both.

`ue-build.ps1` sets `$ErrorActionPreference = "Stop"` and then calls
`Build.bat`. Under PowerShell 7.4+ a non-zero native exit code raises a
terminating error, so the script would throw instead of returning UBT's code —
the failing-build path, which is the only one that matters here. `pack.json`
invokes `powershell` (5.1), where that does not apply, but a 5.1 native
command writing to stderr with `EAP=Stop` and redirected output can raise
`NativeCommandError` too. Test a *failing* build on Windows, and scope the
preference around the call.
