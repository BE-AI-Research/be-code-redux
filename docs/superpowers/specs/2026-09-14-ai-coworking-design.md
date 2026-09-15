# AI Model Co-Working — Design

**Date:** 2026-09-14
**Status:** approved (design sections reviewed in conversation)
**Builds on:** the second-model reviewer (`agent.ReviewerFactory`, `Agent.Review`), plan mode's scratch agent (`agent.planAgent`, `Registry.Subset`), shared asks and per-terminal rendering (0.8.0)
**Target version:** 0.9.0

---

## Problem

The local model does the work, and the harness supplies the quality. But a
small local model sometimes lacks the knowledge or the reasoning for a step:
it hammers a wrong `edit_file` anchor, cannot see why a check keeps failing,
or is asked a design question beyond it. Today the only bigger model in the
loop is the reviewer, which critiques a finished diff after the fact.

The user's notes: "The local AI model is responsible for conducting and doing
the work. The other user-defined AI models are pinged if the primary needs
assistance, or runs into an issue. The main use case for this would be
off-loading complex tasks, local AI models do not know, to online or offline
frontier-class models. (Change font colour for co-work model.)"

Chosen approach (A): a `consult` tool plus harness triggers, each running a
bounded, read-only scratch agent on a co-working model, in process. The
primary keeps all write and shell access; the co-worker inspects and advises.

## Goals

- A primary model can ask a configured co-working model for help mid-task,
  and the harness asks on its behalf at the two points where it already knows
  the primary is stuck.
- The co-worker can read the repository itself (read-only tools), so the
  primary need not know what context to send.
- Code reaches an online model only after explicit, per-session consent.
- Co-worker answers are visibly a second voice on every terminal.
- An unreachable or failing co-worker never interrupts the run.
- Nothing changes for a configuration with no co-workers.

## Non-goals

- Co-workers editing files or running commands.
- Replacing the reviewer; a co-worker is not a reviewer.
- Co-workers as external MCP processes (approach B) or a temporary model swap
  (approach C).
- Deferred consultations: queuing a question for a co-worker that is offline
  and pinging the primary when an answer arrives. The design keeps the seam
  for it (`Consult` is one call with one result) but it is a later feature.

---

## 1. Configuration and selection

```json
"coworkers": [
  {"name": "claude", "provider": "anthropic", "model": "claude-opus-5",
   "skills": "deep reasoning, architecture, tricky bugs", "online": true},
  {"name": "big-qwen", "provider": "ollama", "model": "qwen3:32b",
   "skills": "long code reads on the LAN box"}
],
"cowork": {"auto": true, "max_consults_per_run": 3, "consult_turns": 12}
```

`config.Config` gains:

```go
type CoworkerConfig struct {
    Name     string `json:"name"`
    Provider string `json:"provider"` // key of Config.Providers; api_key_env stays there
    Model    string `json:"model"`
    Skills   string `json:"skills"`   // one sentence shown to the primary
    Online   bool   `json:"online"`   // consent required before the first consultation of a session
}
type CoworkConfig struct {
    Auto              bool `json:"auto"`                 // default true
    MaxConsultsPerRun int  `json:"max_consults_per_run"` // default 3
    ConsultTurns      int  `json:"consult_turns"`        // default 12
}
Coworkers []CoworkerConfig `json:"coworkers"`
Cowork    CoworkConfig     `json:"cowork"`
```

Order matters: the first entry is the default, used when the primary names
no co-worker and for every harness-triggered consultation. `cmd/root.go`
validates the list once at startup: a co-worker whose `provider` is not in
`providers`, or with an empty `name` or `model`, is dropped with
`warn: coworker %q: %s`. Duplicate names keep the first. With an empty list
the `consult` tool is not registered and nothing else changes.

Commands (both UIs, `ui.SlashCommandTable`; both busy-safe):

- `/coworkers` — lists `name  provider/model  skills`, marking `online` and
  those consulted this session with the count.
- `/consult [name] <question>` — a person asks a co-worker directly (the
  first one when `name` is not a configured co-worker). Skips consent (the
  person asking is the consent), does not count against the per-run cap,
  runs as a turn (busy state, Esc cancels) and appends the answer as a
  co-worker entry. The primary is not involved.

## 2. The `consult` tool and the scratch agent

`internal/tools/consult.go`: tool `consult`, registered in
`cmd/root.go:buildAgent` when `len(cfg.Coworkers) > 0`, and included in plan
mode's subset (`Registry.Subset` takes it alongside the read tools). Flat
schema:

- `question` (string, required) — what is wrong and what was tried;
- `who` (string, optional) — a co-worker name; unknown names error with the
  configured list;
- `files` (array of string, optional) — workspace paths to look at first.

Description (embedded in the schema and the compat catalog): the roster as
`name — skills` lines, and "Use when you have tried and failed, when a design
question is beyond you, or when you need knowledge you do not have. Say what
you tried. The co-worker can read the repository but cannot edit; you keep
doing the work."

The tool delegates to `agent.Consult`, injected like `ReviewerFactory`:

```go
// internal/agent/cowork.go
type ConsultRequest struct {
    Who      string   // "" = default
    Question string
    Files    []string
    Origin   string   // "tool" | "auto:verify" | "auto:tool" | "user:<label>"
}
type ConsultResult struct {
    Coworker string
    Answer   string
    Partial  bool   // an error cut it short; Answer is what there was
    Read     int    // files read
    Elapsed  time.Duration
}
func (a *Agent) Consult(ctx context.Context, req ConsultRequest) (ConsultResult, error)
var CoworkerFactory func(cfg *config.Config, name string) (provider.Provider, string, error) // set by cmd
```

`Consult`:

1. Resolves the co-worker (default = first). Applies the per-run cap
   (`a.consults` reset at the start of `RunFull`; `/consult` does not count):
   at the cap, returns the error `consultation limit reached for this run
   (N)`.
2. Consent (section 4). Declined → error `consultation declined`.
3. Builds a scratch agent exactly like `planAgent`: same registry root,
   `Registry.Subset` (`read_file`, `list_dir`, `search`), the co-worker's
   provider and model, its own `History` sized from that provider's window
   (the Ollama window probe applies when the provider is `ollama`), no MCP,
   no IDE tools, no checkpointer, `MaxTurns = cfg.Cowork.ConsultTurns`.
4. System prompt (`agent.ConsultFrame`, verbatim in code): the role ("you
   are advising a smaller local model that is doing the work and keeps the
   pen"), the rules (inspect what you need with the tools; never propose
   running commands as the answer; answer with concrete, minimal guidance and
   file:line references; if the question cannot be answered from the code,
   say so), followed by the project notes.
5. Seed message: the question; the named files (each read and included up
   to 8 KiB, rest as a pointer); the primary's recent context — the current
   user request, the primary's last reply, and the most recent failing tool
   output, capped at 6 KiB total; the git summary.
6. Runs the loop; ends when a reply has no tool calls, at the turn cap, on
   ctx cancel, or on a provider error. Provider errors after a partial
   answer return `Partial: true`; before any answer, an error.

Events: `Consult` emits `OnConsultStart(name, question, origin)`,
`OnConsultProgress(name, filesRead)` and `OnConsultEnd(ConsultResult, err)`
through `agent.Events`, which both UIs render (section 5).

The tool result handed to the primary is `co-worker <name> replied:\n\n` +
answer (+ `\n\n(the co-worker was cut short; this is what it had)` when
partial). Errors are plain tool errors; the primary carries on.

## 3. Harness triggers

With `cfg.Cowork.Auto`, `RunFull` consults the default co-worker at two
points, each at most once per run and within the per-run cap:

- **Verification exhausted.** After the repair loop has used `max_repairs`
  and the last check still fails: `Consult` with origin `auto:verify`, the
  question "the change still fails this check; what is wrong and what minimal
  edit fixes it", the failing check's condensed output, and the changed-files
  list from the checkpoint as `Files`. An answer grants one extra repair
  round whose prompt is `A co-worker (<name>) reviewed the failing check and
  advises:\n\n<answer>\n\nApply the minimal fix, then stop.`, followed by one
  more verification. No answer, no extra round; the report is unchanged.
- **Repeated tool failure.** In `run`'s tool loop, the same tool returning
  `IsError` three times in a row: `Consult` with origin `auto:tool`, the
  tool name, its last arguments and the three errors; the answer is injected
  before the next model call as a user-role note `A co-worker (<name>)
  looked at the repeated <tool> failure and advises:\n\n<answer>` — the same
  path `deliverInbox` uses. The counter resets on any successful tool call.

Both emit the transient notice `consulting <name>…` (`OnTransient`) so every
terminal sees why the run paused, and the ordinary consult events. The plan
agent has no automatic triggers. The reviewer path is unchanged.

## 4. Consent

`Agent.consentFor(cw) bool`, called by `Consult` for `Online` co-workers
only, before the scratch agent exists:

- Per session, per co-worker: `allowed`, `always`, or unset.
- Unset → `a.Tools.Approve("consult", detail)` — the same seam as shell and
  file-write approvals, so the TUI raises a shared ask (`askApproval` with
  action `consult`) on every terminal and plain mode prompts inline. Detail:
  co-worker name and model, the origin, the question, the files named, and
  the line "it may read other files in this workspace; nothing is edited".
  Keys: `y` this consultation, `n` decline (returns `consultation declined`,
  the primary continues alone), `a` allow this co-worker for the session.
- Headless `run -y` behaves as `a`; without `-y` an online consultation is
  declined, matching file writes (`ApproveFileWrites` semantics live in the
  same place).
- `/consult` from a terminal skips consent (the person asking consents).
- Local co-workers (`online: false`) never prompt.

The decision is a transcript verdict entry (`consult claude approved` /
`declined`, `entryVerdict`), visible on every terminal and saved with the
session. Nothing is sent before approval; files the co-worker reads
afterwards go to the provider just approved, with no per-file prompt.

## 5. Rendering

`Palette` gains `Cowork` (a distinct hue per theme, e.g. dark `141`, light
`91`, nord `#b48ead` shifted from User to a magenta/teal family per theme;
mono: bold), `styles` gains `Cowork`. Two entry kinds:

- `entryCoworkAsk` — `Label` = co-worker name, `Text` = question; rendered
  as `st.Cowork.Render(name+"? ") + text`, appended on `OnConsultStart`.
- `entryCowork` — `Label` = name, `Text` = Markdown answer; rendered as
  `st.Cowork.Render(name+"> ") + ui.RenderMarkdown(text, richText)`,
  appended on `OnConsultEnd` (partial answers get a dimmed `(partial)`
  suffix line).

`OnConsultProgress` updates the status note (`consulting claude · 3 files
read`); `OnConsultEnd` appends the dimmed line `claude read N files in Ns`.
Esc during a consultation cancels its ctx (the run's cancel function is the
parent), and the tool result is a cancellation error. `/coworkers` and the
consent modal use `st.Cowork` for names. Plain mode prints `<name>? ` and
`<name>> ` prefixes without colour.

## 6. Error handling

- Provider unreachable, timeout, budget exhausted, cancelled: tool error (or
  partial result), counted against the cap, the run continues. A consultation
  never aborts, blocks or fails the primary's run.
- Config: unknown provider / empty name or model → dropped at startup with a
  warning. Unknown `who` at call time → error naming the configured list.
- The scratch agent's registry is rooted at the workspace; path confinement
  applies as for the primary.
- Concurrency: one consultation at a time per agent (`a.consultMu`); a
  second request while one runs waits or, for the automatic triggers, is
  skipped.

## 7. Testing

`internal/agent` (scripted providers, no network):

- the tool registers only when co-workers exist; plan mode's subset carries it;
- `Consult` builds a read-only agent (a `write_file` call from the co-worker
  is refused), seeds the question, the named files and the recent context,
  respects `ConsultTurns`, returns the final answer, and returns `Partial` on
  a mid-way provider error;
- the per-run cap refuses the fourth consultation and resets on the next run;
  `/consult` does not count;
- `auto:verify` fires once after `max_repairs`, grants one extra round, and
  not at all with `Auto` off; `auto:tool` fires after the third consecutive
  error of one tool and resets on success;
- consent: declined returns `consultation declined` and the scripted
  co-worker provider records zero requests; `a` suppresses later prompts for
  that co-worker only; local co-workers never call `Approve`; headless `-y`
  and non-`-y` behave as specified.

`internal/tui`: the two entry kinds render with `st.Cowork` under two themes;
the consent ask shows on two views and resolves once with the verdict entry;
`/coworkers` and `/consult` output. `internal/ui`: the same commands in plain
mode. E2E: the mock backend on `:18111` plus a second scripted endpoint
acting as the co-worker, one task whose primary calls `consult` and finishes
with the advice in its tool results. Live checklist: item 15 — `/consult`
from the tablet shows the question and the answer on both terminals in the
co-worker colour, and a `consult` from the model during a run shows the
transient notice and the verdict line.

## 8. Docs

README: a "Co-working models" section (what, config, consent, commands) and
the config reference rows for `coworkers` and `cowork`; CHANGELOG 0.9.0;
`build.mk` VERSION 0.9.0; root `CLAUDE.md`: a "Co-working models" subsection
under Architecture (tool, scratch agent, triggers, consent seam, events).

## Future

- **Deferred consultations.** When a co-worker is unreachable, queue the
  question (session-scoped, persisted with the session) and retry in the
  background; when an answer arrives while the primary still needs it,
  deliver it through the inbox as an advice note and notify the terminals.
  `Consult`'s single-call shape and the advice-note injection path are the
  seams this needs.
