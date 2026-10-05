# Online model providers as the main agent — design

**Status:** approved in brainstorming, 2026-10-04. Target release: 1.2.0 (sub-project 1 of 2 for
the release's "splash"; WebMCP gets its own spec). Branch `feature/online-providers` off `main`.

## Purpose

Let people without a local GPU use BE-Code by running the main agent on an online model —
OpenRouter first — while BE-Code keeps leaning on its local strengths: housekeeping work goes to a
local model when one exists, and every piece of data that leaves the machine beyond what the person
chose to share is gated by their consent.

## Decisions taken in brainstorming

| Question | Decision | Why |
| --- | --- | --- |
| Which providers on day one? | **OpenAI-compatible presets only**, OpenRouter as the headline. | One wire format (the one BE-Code already speaks); OpenRouter is one key for hundreds of models. A native Anthropic client can follow later. |
| Browser page text with an online model? | **Per-site consent**: asked before a site's page text is sent; yes covers the site for the session; in the person's own Chrome every page asks. | The person makes that choice, site by site. |
| Consent to send the project at all? | **Once per project**, remembered per workspace and provider. | Some projects can stay local-only without nagging. What is sent after that is the conversation the person already shares. |
| Cost? | **Show spend, optional cap** (`max_spend_usd`). | Informed users; a cap is the only guard against an unattended loop. |
| Architecture | **Hybrid: online main, local housekeeping** (`local_helper`). | Online model does the reasoning; summaries, briefings, notes, commit messages and review stay local and free. |

## §1 Providers, setup, models

### 1.1 Presets
`internal/provider/presets.go`: a table of `{name, base_url, api_key_env, docs}`. A preset is an
ordinary `providers` entry of `type: "openai"` plus one new field `online: true`.

| Preset | Base URL | Key variable |
| --- | --- | --- |
| `openrouter` | `https://openrouter.ai/api/v1` | `OPENROUTER_API_KEY` |
| `openai` | `https://api.openai.com/v1` | `OPENAI_API_KEY` |
| `groq` | `https://api.groq.com/openai/v1` | `GROQ_API_KEY` |
| `deepseek` | `https://api.deepseek.com/v1` | `DEEPSEEK_API_KEY` |
| `mistral` | `https://api.mistral.ai/v1` | `MISTRAL_API_KEY` |
| `gemini` | `https://generativelanguage.googleapis.com/v1beta/openai` | `GEMINI_API_KEY` |
| `anthropic` | `https://api.anthropic.com/v1` | `ANTHROPIC_API_KEY` |

The Gemini and Anthropic compatibility endpoints are verified against their current docs during
implementation; a wrong URL is a doctor-visible failure, never a silent one.

`online` is corroborated exactly as for co-workers (`config.LocalEndpoint`): any provider whose
base URL is not local is treated as online regardless of the flag (with a warning when the flag
says otherwise). Keys come only from the environment variable named in `api_key_env`.

### 1.2 Setup and doctor
`be-code setup` probes local servers first (unchanged), then offers "or use an online provider":
pick a preset → check the key variable is set (say how to set it if not) → list models from the
provider's `/models` → choose one → save the provider, the model, and the local server it found (if
any) as `local_helper`. `doctor` gains an `online` line: key present, endpoint reachable, the window
and prices it learned.

### 1.3 Window and prices
No Ollama to ask. The window comes from the provider's `/models` listing where present
(OpenRouter: `context_length`; pricing per token), else the preset's built-in table, else the
config's `context_window`, else a conservative 32768 with the existing budget notice. The loader's
residency/reload consent does not apply to online endpoints (nothing to reload, nobody to evict).
The budget rules (reserve cap, harness reply caps, notices) apply unchanged.

### 1.4 Profiles
New families in `internal/profiles`: `gpt`, `o3`/`o4` (reasoning), `claude`, `gemini`, online
`deepseek-v3`/`deepseek-r1`, Groq-hosted `llama`. Matching is by substring, so OpenRouter's
`vendor/model` names match. These use native tool calling (compat off). `reasoning_effort` is sent
only to models that accept it. The append-only prompt layout already gives providers with automatic
prefix caching a growing prefix.

## §2 Consent and cost

### 2.1 Per-project consent (`online_project`)
Before the first request a session would send to an online main model, a startup gate (same shape
as the schedules startup prompt; raised once the UI's approvals are wired) asks: "This project's
files, command output and conversation will be sent to **<Provider>** (`<model>`). Allow for this
project?"
- Remembered per workspace and provider in `~/.be-code/engine/<key>/online.json`; a different
  online provider asks again; `/online forget` clears it.
- **No** → the session's main model becomes `local_helper` (notice); with no helper, the session
  stops with a clear message.
- Headless `-y` allows for that run only and remembers nothing (as `-y` already covers online
  co-workers); headless without `-y` and without a remembered yes refuses.
- A scheduled event never raises it: an unapproved project's event does not run (notice).
- The status line shows `online: <provider> · <model>` while the main model is online; the REPL
  prompt shows `(online)`.

### 2.2 Per-site page consent (`share_page`)
Only while the main model is online. Before page text, page notes or tab titles from a site reach
the model: "Send what the agent reads on <host> to <Provider>?"
- Normal browser modes: yes covers the host for the session. The person's own Chrome
  (my-Chrome mode): every page asks, no "always".
- No / no answer → withheld through the existing withholding path (the model is told the page on
  that host is not shown).
- `web_fetch` follows the same per-host rule; `web_search` results ask once per session.
- Hosts in the browser `allow` tier do not ask.
- In a scheduled event it is a deadline prompt with no shortcuts (`FiredAsk`).
- `share_page` and `online_project` have no "always" key in the TUI (the `a` handler names them)
  and are never approved by `-y` except as stated in 2.1.

### 2.3 Cost
Tokens are counted from each response's `usage`; spend is priced per model from the provider's
listing (OpenRouter) or the preset table. A model with no known price gets one notice ("spend is
not tracked for this model"). `/stats` shows tokens and estimated spend; the status line shows the
running total (`$0.42`). `max_spend_usd` (session; default 0 = off): before a request that would
start beyond the cap, BE-Code asks "This session has spent $X of your $Y cap. Continue (raises the
cap by $Y)?"; with nobody to answer (a scheduled event, headless) it refuses with the reason.
`local_helper` and local co-workers are free and not counted.

## §3 The local helper

```json
"local_helper": { "provider": "ollama", "model": "<model>" }
```
Used only while the main model is online; with a local main model everything is as today. Chores it
takes, through one `HelperFactory` injected from `cmd` like `ReviewerFactory`:
- compaction summaries (`Compact`),
- the resume briefing (`WriteHandoff`),
- `/init` notes (`InitProject`),
- commit messages (`GenerateCommit`),
- the second-model review when `reviewer` is not configured separately.

The helper's window comes from the loader with no approver (as co-workers and reviewers already do);
the existing reply caps apply (summary 2048, notes 4096, thinking headroom where `NoThink` is not
honoured). The online model keeps the actual work (tool calls, edits, answers, repair rounds).
Co-workers and sub-agents are unchanged.

**Fallbacks.** Helper unreachable or unset → the chore goes to the online main model with one notice
("local helper unavailable; housekeeping uses the online model") — nothing new leaves the machine,
the online model already holds the conversation. A transcript larger than the helper's window → the
summary prompt is cut to fit, newest first; still failing → compaction continues from the task
record (existing `fromTaskRecord`). The helper runs in its own server's lane; `/stats` lists its
tokens separately at no cost.

## §4 Errors, limits, unchanged, testing

**Errors.** 429 → honour `Retry-After` via `chatWithRetry`, notice "rate-limited by <Provider>;
retrying in Ns", give up after the existing bound with a clear message. 401/403 → no retries,
"<KEY_ENV> rejected by <Provider>" (doctor says the same). Sustained outage → offer to switch the
session's main model to `local_helper` (TUI yes/no; a scheduled event stops with a notice instead).
Context overflow → the existing overflow recovery. Unknown model window → config `context_window` or
32768 with the budget notice.

**Unchanged.** Ollama/local servers exactly as today, local remains the default; every existing
consent rule (shell, writes, browser tiers, my-Chrome "everything asks", schedules' allowances and
fired-turn gate, untrusted-web flag); online co-worker consent; keys only from the environment; no
telemetry.

**Testing** (scripted fakes, no network). Presets build working providers; `/models` parsing
(OpenRouter shape: `context_length`, pricing); 429 with `Retry-After`, 401, outage; per-project
consent asked once, remembered per workspace+provider, "no" → helper main, `-y` remembers nothing,
a scheduled event refuses an unapproved project; `share_page` per host, every page in my-Chrome,
allow tier skips, `web_fetch`/`web_search` covered, refusal withholds; spend totals, cap prompt,
refusal with nobody to answer, unpriced-model notice; each chore routed to the helper while online,
fallback notice when it is down, oversized transcript trimmed; profile detection from
`vendor/model` names. Live checklist: a real OpenRouter session with a key, a deliberate cap hit, a
helper-down run.

## §5 Out of scope

A native Anthropic Messages client; automatic per-request local/online routing; provider-side web
search integration (OpenRouter `:online`, OpenAI search models) beyond passing the model name
through; spend caps per day/month; storing keys anywhere but the environment.
