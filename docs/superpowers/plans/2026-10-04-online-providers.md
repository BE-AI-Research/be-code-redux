# Online Model Providers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an online OpenAI-compatible provider (OpenRouter first) be BE-Code's main model, with per-project and per-site consent, spend tracking with an optional cap, and local housekeeping through a `local_helper`.

**Architecture:** Presets are ordinary `providers` entries of `type: "openai"` plus `online: true`, corroborated by `config.LocalEndpoint`. The existing OpenAI-compatible client gains a typed HTTP error (status + `Retry-After`) and an OpenRouter-shaped `/models` parse (window, prices). The agent gains pricing/spend, an online-project startup gate, a `HelperFactory` that routes five housekeeping chores to a local model, and an outage fallback; the tools gain a `share_page` gate in front of browser and web results.

**Tech Stack:** Go (stdlib + existing deps), Bubble Tea TUI, existing `tools.Registry` approval seam.

**Spec:** `docs/superpowers/specs/2026-10-04-online-providers-design.md`

## Global Constraints

- All commands run from `be-code/` (module root `/home/sbrown/Documents/Development/BE-CodeRedux/be-code/be-code`), branch `feature/online-providers`. Commits are authored by the repo's local git config (Shayne G. Brown) — do not change it.
- Every commit message ends with exactly:
  ```
  Co-Authored-By: Claude Opus 5.5 (1M context) <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9
  ```
- Tests never touch the real `~/.be-code` (set a temp HOME; `stat -c %y ~/.be-code/config.json` must not change across test runs), never reach the network (httptest/scripted providers only), never run `pkill`, never run `./install.sh`. No git stash. Don't touch `BECODE.md`.
- `make -f build.mk verify` passes at the end of every task. The `internal/live` KeyPump stress test is a known flake under load: re-run that package alone if it alone fails.
- Version stays 1.2.0. API keys only from the environment variable named in `api_key_env`; never written to config or logs. No telemetry.
- Ollama and local servers behave exactly as today; every new behaviour applies only while the **main** model's provider is online.
- New approval actions `online_project`, `share_page`, `spend_cap`, `switch_to_local` have **no "always"**: the TUI `a` handler (`internal/tui/view.go` ~889–890) must name them, the REPL `approveCtx` no-always set (`internal/ui/repl.go` ~234–235) must include them, `headlessApprover` (`cmd/commands.go` ~218–245) refuses them (except `online_project` under `-y`, see Task 7). The TUI modal "not a diff" list (`view.go` ~778–780) includes them.
- Preset table (verbatim from the spec): openrouter `https://openrouter.ai/api/v1` `OPENROUTER_API_KEY`; openai `https://api.openai.com/v1` `OPENAI_API_KEY`; groq `https://api.groq.com/openai/v1` `GROQ_API_KEY`; deepseek `https://api.deepseek.com/v1` `DEEPSEEK_API_KEY`; mistral `https://api.mistral.ai/v1` `MISTRAL_API_KEY`; gemini `https://generativelanguage.googleapis.com/v1beta/openai` `GEMINI_API_KEY`; anthropic `https://api.anthropic.com/v1` `ANTHROPIC_API_KEY`.
- Exact user-visible strings (spec): `This project's files, command output and conversation will be sent to <Provider> (<model>). Allow for this project?`; `Send what the agent reads on <host> to <Provider>?`; `This session has spent $X of your $Y cap. Continue (raises the cap by $Y)?`; `spend is not tracked for this model`; `local helper unavailable; housekeeping uses the online model`; `rate-limited by <Provider>; retrying in Ns`; `<KEY_ENV> rejected by <Provider>`; status `online: <provider> · <model>`; REPL prompt suffix `(online)`.

## Review Focus

1. A model ID OpenRouter lists without `context_length` or pricing (common for new/free models) — window falls back to preset/config/32768 with the budget notice; spend shows "not tracked" once, never $0 forever (Task 4 test `TestOnlineWindowFallsBackWhenListingLacksIt`, Task 5 `TestUnpricedModelNoticeOnce`).
2. `Retry-After` given as an HTTP date rather than seconds, or absurdly large — parse both forms, clamp to the existing 30 s cap and the retry bound (Task 2 `TestRetryAfterParsesDateAndClamps`).
3. The person answers "no" to the project prompt with no `local_helper` — the session must stop cleanly before any request is sent, not send one first (Task 7 `TestOnlineProjectNoWithoutHelperSendsNothing`).
4. A page that redirects to another host after `share_page` was granted for the first — the second host's text must not ride on the first host's grant (Task 8 `TestSharePageJudgedOnFinalHost`).
5. Helper chores while the helper's server is down mid-session — one notice, chores fall back, the session never blocks on the helper (Task 6 `TestHelperDownFallsBackOnceWithNotice`).

---

## File structure

| File | Responsibility |
| --- | --- |
| `internal/config/config.go` | `ProviderConfig.Online`, `Config.LocalHelper`, `Config.MaxSpendUSD`, `ProviderIsOnline` |
| `internal/provider/presets.go` | preset table, `Preset`, `Presets()`, `PresetByName`, `PresetWindow/PresetPrices` |
| `internal/provider/openai.go`, `errors.go` | exported `HTTPError` (status, body, `RetryAfter`), OpenRouter `/models` parse |
| `internal/provider/provider.go` | `ModelInfo` gains `ContextLength`, `PromptPrice`, `CompletionPrice` |
| `internal/profiles/profiles.go` | online families |
| `internal/agent/online.go` | pricing, spend, cap, online state, project-consent gate, outage switch |
| `internal/agent/helper.go` | `HelperFactory`, `helperFor`, chore routing helper |
| `internal/agent/resilience.go` | Retry-After, 401/403 no-retry, outage hook |
| `internal/tools/share.go` | `share_page` gate (`Registry.SetShareGate`) |
| `internal/tools/browser.go`, `web.go` | consult the share gate before returning page text |
| `internal/setup/wizard.go` | online path |
| `cmd/root.go`, `cmd/commands.go`, `cmd/live.go` | wiring, doctor line, `/online` |
| `internal/tui/*`, `internal/ui/*` | badge, prompts, `/online forget`, `$` in status |

---

### Task 1: Config, presets, online corroboration

**Files:** Modify `internal/config/config.go` (ProviderConfig ~124, Config, Default); Create `internal/provider/presets.go`; Tests `internal/config/online_test.go`, `internal/provider/presets_test.go`; README config reference.

**Interfaces — Produces:**
- `ProviderConfig.Online bool \`json:"online,omitempty"\``
- `type LocalHelperConfig struct { Provider string \`json:"provider"\`; Model string \`json:"model"\` }`; `Config.LocalHelper LocalHelperConfig \`json:"local_helper"\``
- `Config.MaxSpendUSD float64 \`json:"max_spend_usd"\`` (default 0 = off)
- `func ProviderIsOnline(pc ProviderConfig) bool` — `pc.Online || !LocalEndpoint(pc.BaseURL)`
- `func (c *Config) OnlineWarnings() []string` — one `provider %q is at %s, not local; treating it as online (set "online": true)` per provider whose flag is false but endpoint is not local.
- `provider.Preset{Name, BaseURL, KeyEnv, Docs string; Window int; PromptPrice, CompletionPrice map[string]float64}` (prices per token keyed by model-ID substring), `provider.Presets() []Preset`, `provider.PresetByName(name string) (Preset, bool)`, `func (p Preset) ProviderConfig() config.ProviderConfig` (Type "openai", Online true).

- [ ] **Step 1: Failing tests**

```go
// internal/config/online_test.go
func TestProviderIsOnline(t *testing.T) {
	cases := map[string]struct{ pc ProviderConfig; want bool }{
		"local ollama":        {ProviderConfig{BaseURL: "http://192.168.1.150:11434/v1"}, false},
		"remote unflagged":    {ProviderConfig{BaseURL: "https://openrouter.ai/api/v1"}, true},
		"local but flagged":   {ProviderConfig{BaseURL: "http://localhost:8080/v1", Online: true}, true},
	}
	for name, c := range cases {
		if got := ProviderIsOnline(c.pc); got != c.want {
			t.Errorf("%s: %v", name, got)
		}
	}
}

func TestOnlineWarnings(t *testing.T) {
	c := Default()
	c.Providers["or"] = ProviderConfig{Type: "openai", BaseURL: "https://openrouter.ai/api/v1"}
	ws := c.OnlineWarnings()
	if len(ws) != 1 || !strings.Contains(ws[0], `"or"`) || !strings.Contains(ws[0], "treating it as online") {
		t.Fatalf("%v", ws)
	}
}

func TestOnlineConfigDefaults(t *testing.T) {
	c := Default()
	if c.MaxSpendUSD != 0 || c.LocalHelper.Provider != "" {
		t.Fatalf("defaults must be off: %+v %+v", c.MaxSpendUSD, c.LocalHelper)
	}
}
```

```go
// internal/provider/presets_test.go
func TestPresetsVerbatim(t *testing.T) {
	want := map[string][2]string{
		"openrouter": {"https://openrouter.ai/api/v1", "OPENROUTER_API_KEY"},
		"openai":     {"https://api.openai.com/v1", "OPENAI_API_KEY"},
		"groq":       {"https://api.groq.com/openai/v1", "GROQ_API_KEY"},
		"deepseek":   {"https://api.deepseek.com/v1", "DEEPSEEK_API_KEY"},
		"mistral":    {"https://api.mistral.ai/v1", "MISTRAL_API_KEY"},
		"gemini":     {"https://generativelanguage.googleapis.com/v1beta/openai", "GEMINI_API_KEY"},
		"anthropic":  {"https://api.anthropic.com/v1", "ANTHROPIC_API_KEY"},
	}
	if len(Presets()) != len(want) || Presets()[0].Name != "openrouter" {
		t.Fatalf("openrouter must lead: %+v", Presets())
	}
	for n, w := range want {
		p, ok := PresetByName(n)
		pc := p.ProviderConfig()
		if !ok || p.BaseURL != w[0] || p.KeyEnv != w[1] || pc.Type != "openai" || !pc.Online || pc.APIKeyEnv != w[1] {
			t.Errorf("%s: %+v %+v", n, p, pc)
		}
	}
}
```

- [ ] **Step 2: Run** `go test ./internal/config/ ./internal/provider/ -run 'Online|Preset' -count=1` → FAIL (undefined).
- [ ] **Step 3: Implement.** Add the fields/types above; `ProviderIsOnline` and `OnlineWarnings` next to `LocalEndpoint` (config.go ~742); `presets.go` with the table (Window/prices may be 0/empty where unknown — the comment says the `/models` listing wins). `cmd/root.go` prints `OnlineWarnings()` as `warn:` lines at the same place it prints co-worker warnings (~211–214). README config reference: `providers.<name>.online`, `local_helper`, `max_spend_usd`, and a short "Online providers" note listing presets.
- [ ] **Step 4: Run** the same → PASS; `make -f build.mk verify`.
- [ ] **Step 5: Commit** `config, provider: online providers — presets, online flag, local_helper, max_spend_usd`.

---

### Task 2: Typed HTTP errors, Retry-After, 401/403, OpenRouter `/models`

**Files:** Create `internal/provider/errors.go`; Modify `internal/provider/openai.go` (~157–159 non-2xx; ~297 ListModels), `internal/provider/provider.go` (ModelInfo ~176), `internal/agent/resilience.go` (~29 chatWithRetry, ~95 isRetryableBackendError); Tests `internal/provider/errors_test.go`, `internal/provider/openai_models_test.go`, `internal/agent/retry_online_test.go`.

**Interfaces — Produces:**
- `type HTTPError struct { Provider string; Code int; Body string; RetryAfter time.Duration }` with `Error()` keeping today's text `"%s: HTTP %d: %s"` (so existing string classification still works); `func ParseRetryAfter(h string, now time.Time) time.Duration` (seconds or HTTP-date; negative → 0).
- `ModelInfo` gains `ContextLength int; PromptPrice, CompletionPrice float64` (USD per token).
- `OpenAICompat.ListModels` parses OpenRouter shape: `{"data":[{"id","context_length","pricing":{"prompt":"0.000003","completion":"0.000015"}}]}` (prices are strings in OpenRouter; accept string or number); absent fields stay 0.
- In the agent: 429 → wait `RetryAfter` (clamped to the existing 30 s cap; still bounded by maxBackendRetries) with transient notice `rate-limited by <Provider>; retrying in Ns`; 401/403 → no retry, error `<KEY_ENV> rejected by <Provider>` (the agent needs the key env name: add `Agent.KeyEnv string` set by cmd from the provider config; empty → `API key rejected by <Provider>`).

- [ ] **Step 1: Failing tests** — `TestHTTPErrorKeepsText` (Error() == old format); `TestRetryAfterParsesDateAndClamps` (`"12"`→12s; an HTTP-date 20 s ahead → ~20s; `"-5"`→0; garbage→0); `TestOpenAIErrorCarriesStatusAndRetryAfter` (httptest returns 429 + `Retry-After: 7` → `errors.As(err, &*HTTPError)` with Code 429, RetryAfter 7s); `TestListModelsParsesOpenRouterShape` (two models, one with context_length 200000 and string prices, one bare → zeros); `TestChatWithRetryHonoursRetryAfter` (scripted provider: first call returns `&provider.HTTPError{Code:429, RetryAfter: 3*time.Second}`, second succeeds; set `a.retryBase` tiny; assert the transient notice text and that the wait used ≥ min(3s, cap) via an injectable sleep func — add `a.sleep func(ctx, d) error` defaulting to a ctx-aware sleep, and assert the requested duration rather than sleeping); `TestUnauthorizedNotRetried` (401 → one call, error text `OPENROUTER_API_KEY rejected by openrouter`).
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.** Return `&HTTPError{...}` from openai.go's non-2xx path with `RetryAfter: ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now())`. In `chatWithRetry`, before the string classifier: `var he *provider.HTTPError; if errors.As(err, &he)` → 401/403: return the rejected-key error immediately; 429: delay = clamp(he.RetryAfter or backoff, cap). Keep all other behaviour.
- [ ] **Step 4: Run** focused tests with `-race`; `make -f build.mk verify`.
- [ ] **Step 5: Commit** `provider, agent: typed HTTP errors, Retry-After, no retries on a rejected key, OpenRouter model listing`.

---

### Task 3: Online model profiles

**Files:** Modify `internal/profiles/profiles.go` (table); Test `internal/profiles/profiles_test.go` (append).

Add rows (Compat `"never"` — native tool calls; Temperature as listed; StripThink false unless the model emits `<think>` in content):
`gpt` (0.2), `o3` (-1, reasoning; Notes "reasoning model; temperature ignored"), `o4` (-1), `claude` (0.2), `gemini` (0.2), `deepseek-chat`/`deepseek-v3` (0.2), keep existing `deepseek-r1` (StripThink true), `llama-3` on Groq inherits `llama`.

- [ ] **Step 1: Failing test**

```go
func TestOnlineFamiliesFromVendorNames(t *testing.T) {
	cases := map[string]string{
		"openai/gpt-4.1":               "gpt",
		"anthropic/claude-sonnet-5":    "claude",
		"google/gemini-3.5-flash":      "gemini",
		"openai/o4-mini":               "o4",
		"deepseek/deepseek-chat":       "deepseek-chat",
		"deepseek/deepseek-r1":         "deepseek-r1",
		"hf.co/unsloth/Qwen3.8-27B":    "qwen3",
	}
	for id, fam := range cases {
		if p := Detect(id); p.Family != fam {
			t.Errorf("%s: %s", id, p.Family)
		}
	}
	if Detect("anthropic/claude-sonnet-5").Compat != "never" {
		t.Error("online families use native tool calls")
	}
}
```
(Check `Compat` legal values in profiles.go first; use the value that means "native, never compat" — if the table uses `"auto"`/`"always"` only, use `"auto"` and assert native in a separate check of whatever field drives it, documenting the choice.)
- [ ] **Step 2–4:** run → FAIL; add rows; run → PASS; verify.
- [ ] **Step 5: Commit** `profiles: online model families`.

---

### Task 4: Online window, prices, doctor line

**Files:** Modify `cmd/root.go` (`applyModelParams` — the non-Ollama early return; the provider construction at ~149), `cmd/commands.go` (doctor); Create `internal/agent/online.go` (pricing + online state); Tests `internal/agent/online_test.go`, `cmd/doctor_test.go` (append).

**Interfaces — Produces (agent):**
- `type Pricing struct { Prompt, Completion float64; Known bool }`
- `func (a *Agent) SetOnline(provider, keyEnv string, p Pricing)` — marks the main model online (zero value = local); `func (a *Agent) Online() (provider string, ok bool)`; `func (a *Agent) Pricing() Pricing`.
- `func OnlineWindow(listed provider.ModelInfo, preset provider.Preset, configured int) int` — first positive of `listed.ContextLength`, `preset.Window`, `configured`; else 0 (caller lets the existing budget path apply its 32768 default + notice).
- cmd: when the main provider is online, after building it: `ListModels` (5 s timeout; failure is a warning, not fatal), find the model → window via `OnlineWindow` → `ag.ApplyWindow(w)` when > 0; pricing from the listing or the preset's substring table → `ag.SetOnline(name, pc.APIKeyEnv, pricing)`; `ag.KeyEnv = pc.APIKeyEnv`. Loader consent/reload never runs for online providers (they are type openai, so `applyModelParams` already skips; assert it).
- doctor line `online: <provider> · <model> — key <KEY_ENV> set|missing · reachable|unreachable · window N · $p/$c per Mtok` (only when the default provider is online; never connects when the key is missing; uses ListModels with a 5 s timeout).

- [ ] **Step 1: Failing tests** — `TestOnlineWindowPrecedence`; `TestOnlineWindowFallsBackWhenListingLacksIt` (listing 0 → preset → config → 0); `TestApplyOnlineFromListing` in cmd (httptest `/models` with the model → agent window and pricing set; `Online()` true); `TestDoctorOnlineLine` (key missing → says missing and makes no request; key set → window and prices shown).
- [ ] **Step 2–4:** FAIL → implement → PASS; verify.
- [ ] **Step 5: Commit** `agent, cmd: learn an online model's window and prices; doctor online line`.

---

### Task 5: Spend tracking and the cap

**Files:** Modify `internal/agent/online.go`, `internal/agent/loop.go` (Stats ~60, `chatFiltered` ~1683–1702 where `used` is built, `StatsReport`), `internal/agent/stats.go`, `internal/tui/view.go` (`bottomLine` ~1248), `internal/ui/repl.go`; Tests `internal/agent/spend_test.go`, `internal/tui/online_test.go`.

**Interfaces — Produces:**
- `Stats.SpendUSD float64` (summed in `Stats.add`), `Stats.HelperPromptTokens, HelperCompletionTokens int` (Task 6 fills them).
- In `chatFiltered`, after `used` is built and only when `a.Online()`: `used.SpendUSD = float64(used.PromptTokens)*p.Prompt + float64(used.CompletionTokens)*p.Completion` when `p.Known`; when not known, notice once `spend is not tracked for this model`.
- `func (a *Agent) checkSpendCap(ctx context.Context) error` called at the top of every **primary** model call (the same place `awaitWindow` is called before `chatWithRetry`): if `Cfg.MaxSpendUSD > 0 && spent >= cap`: ask `spend_cap` with `This session has spent $%.2f of your $%.2f cap. Continue (raises the cap by $%.2f)?` through `Tools.ApproveCtx` (FiredAsk-marked during a fired turn so it carries the deadline); yes → cap += MaxSpendUSD (session-local field, config untouched); no / nil approver / fired turn refusal → return error `spend cap reached ($X of $Y); raise max_spend_usd or continue when asked`.
- `/stats` shows `spend $X.XX (est.)` or `spend not tracked` while online; TUI bottom line appends ` · $0.42` while online; REPL prints nothing extra per turn (only `/stats`).

- [ ] **Step 1: Failing tests** — `TestSpendAccumulatesFromUsage` (scripted provider usage 1000/500 at $3e-6/$15e-6 → $0.0105); `TestUnpricedModelNoticeOnce`; `TestLocalSessionNoSpend` (not online → SpendUSD stays 0, no notice); `TestSpendCapAsksAndRaises` (cap $0.01, spent past it → asks spend_cap once with the exact text; yes → next call proceeds; cap doubled); `TestSpendCapRefusesWithNobody` (nil approver → error text); `TestSpendCapInFiredTurnRefusesOnTimeout` (fired allowance with tiny ask_timeout and an ApproveCtx that waits → error); TUI `TestBottomLineShowsSpendWhenOnline`.
- [ ] **Step 2–4:** FAIL → implement → PASS (-race); verify.
- [ ] **Step 5: Commit** `agent, ui: spend tracking and the max_spend_usd cap`.

---

### Task 6: The local helper

**Files:** Create `internal/agent/helper.go`; Modify `internal/agent/loop.go` (Compact ~1739, chats at ~1846 and ~1905), `internal/agent/handoff.go` (modelHandoff ~92, chat ~135), `internal/agent/initproject.go` (~182), `internal/agent/extras.go` (GenerateCommit ~84, Review ~124/174), `internal/agent/loop.go` RunFull review branch (~2130), `cmd/root.go` (assign `HelperFactory` next to `ReviewerFactory` ~259); Tests `internal/agent/helper_test.go`.

**Interfaces — Produces:**
- `var HelperFactory func(ctx context.Context, cfg *config.Config) (provider.Provider, string, int, error)` (provider, model, resolved window); cmd builds it from `cfg.LocalHelper` via `provider.FromConfig(cfg, cfg.LocalHelper.Provider)` and `secondaryLoad` (no approver).
- `func (a *Agent) choreModel(ctx context.Context) (p provider.Provider, model string, window int, helper bool)` — when `a.Online()` and `cfg.LocalHelper.Model != ""` and `HelperFactory != nil`: build once (cache on the agent; a build or later chat failure marks the helper down for 60 s), return it with `helper=true`; otherwise `a.Provider, a.Model, a.Window(), false`. A failure → notice once `local helper unavailable; housekeeping uses the online model` and return the primary.
- The five chores use `choreModel`: Compact's summary and retry, modelHandoff, InitProject, GenerateCommit, and the review **only when** `Cfg.Reviewer.Model == ""` (then the helper is the reviewer; `RunFull`'s review condition becomes `ReviewOnDone && (Reviewer.Model != "" || helper available)`). Each chore's request keeps its existing reply cap (`harnessReplyTokens`, computed against the helper's window when `helper`), its lane is `a.laneFor(p, false)` for the helper's server.
- Oversized prompt for the helper: when the estimated summary prompt exceeds the helper window minus its reply cap, cut the transcript part from the oldest end until it fits (keep the task record/prior summary header); if still too big, fall through to `fromTaskRecord` (existing path).
- Usage from helper chores goes to `Stats.HelperPromptTokens/HelperCompletionTokens`, never to SpendUSD; `/stats` shows a `helper` line while online.
- A helper chat error mid-chore: notice once (same text), retry that chore once on the primary.

- [ ] **Step 1: Failing tests** — `TestChoresUseHelperWhileOnline` (scripted online primary + scripted helper via a test `HelperFactory`; run Compact, WriteHandoff(true), InitProject, GenerateCommit, and a RunFull with ReviewOnDone and no reviewer → each chore's request lands on the helper, none on the primary); `TestChoresUsePrimaryWhenLocal` (not online → helper never built); `TestHelperDownFallsBackOnceWithNotice` (factory error → two chores, one notice, both on the primary); `TestHelperChatErrorRetriesOnPrimary`; `TestHelperPromptTrimmedToWindow` (helper window 4096, long transcript → request fits, newest kept); `TestHelperUsageNotSpend`.
- [ ] **Step 2–4:** FAIL → implement → PASS (-race); verify.
- [ ] **Step 5: Commit** `agent: local_helper takes housekeeping while the main model is online`.

---

### Task 7: Per-project consent, badge, `/online`

**Files:** Modify `internal/agent/online.go`; `internal/agent/schedules.go` (runFired begin — refuse when unapproved); `cmd/root.go` (TUI ~790, plain ~752–755), `cmd/live.go` (~145), `cmd/commands.go` (headless run + headlessApprover); `internal/tui/view.go` (badge, a-handler, modal list), `internal/ui/repl.go` (prompt suffix, no-always set), `internal/ui/common.go` (`/online` in SlashCommandTable + busySafe); Tests `internal/agent/online_consent_test.go`, UI tests.

**Interfaces — Produces:**
- Store: `~/.be-code/engine/<engine.Key(root)>/online.json` = `{"<provider>": true}` (atomic write, 0600). `func (a *Agent) OnlineApproved() bool`, `func (a *Agent) ForgetOnline() error`.
- `func (a *Agent) StartOnlineGate() (ok bool)` — no-op true when not online or already approved; otherwise asks `online_project` with the exact text; yes → store + true; no → `switchToHelper("you declined sending this project to <Provider>")` (SetProvider + SetModelNow to the helper, `SetOnline` cleared, notice) and true if a helper exists, else false with notice `this project is not approved for <Provider> and no local_helper is configured; nothing was sent`; withdrawn → false, nothing stored. `func (a *Agent) StartOnlineGateAsync(done func(ok bool))`.
- **Nothing reaches the online model before the gate passes:** `a.onlineGate` state checked at the top of every primary call (next to `checkSpendCap`): not yet passed → error `waiting for your answer about sending this project to <Provider>` (UIs run the gate before accepting input; this is the backstop).
- Wiring: TUI and hosted call `StartOnlineGateAsync` **before** `StartSubAgentsAsync`/`StartSchedulesAsync`; plain mode calls `StartOnlineGate` first in `repl.OnStart`; headless `run`: `-y` → `a.ApproveOnlineForRun()` (in-memory only); no `-y` and not stored → exit with `this project is not approved for <Provider>; run interactively once, or pass -y for this run`. A gate that returns false in the TUI quits the session after showing the notice.
- Scheduled events: `begin` refuses with reason `this project is not approved for <Provider>` when online and not approved (no prompt).
- `/online` (busy-safe): shows provider, model, approval state, spend; `/online forget` → ForgetOnline (next session asks again).
- Badge: TUI bottom line ` · online: <provider> · <model>` (instead of the plain model segment) while online; REPL prompt `be-code (online)> `.

- [ ] **Step 1: Failing tests** — `TestOnlineProjectAskedOnceAndRemembered` (asked, yes, second agent on same workspace not asked; a different provider asked again); `TestOnlineProjectNoSwitchesToHelper`; `TestOnlineProjectNoWithoutHelperSendsNothing` (scripted online provider records zero requests after a "no"; RunFull returns the backstop error); `TestOnlineProjectYesFlagRunOnly` (-y path approves, nothing written to online.json); `TestScheduledEventRefusedWhenNotApproved`; `TestForgetOnline`; TUI `TestOnlineBadge` and a-key ignored on `online_project`; REPL prompt suffix and `[y/N]`.
- [ ] **Step 2–4:** FAIL → implement → PASS (-race); verify.
- [ ] **Step 5: Commit** `agent, ui, cmd: per-project consent for online models; online badge; /online`.

---

### Task 8: `share_page` — per-site consent for page text

**Files:** Create `internal/tools/share.go`; Modify `internal/tools/browser.go` (`finish` ~346, `withhold` ~379, `mayShow` ~466), `internal/tools/web.go` (search ~64, fetch ~171), `cmd/root.go` (wire when online); Tests `internal/tools/share_test.go`.

**Interfaces — Produces:**
- `type ShareGate struct { Provider string; MyChrome func() bool }`; `func (r *Registry) SetShareGate(g *ShareGate)`; `func (r *Registry) shareOK(ctx context.Context, host string, everyTime bool) bool` — nil gate → true (local main); allow-tier host → true (caller passes the tier check result); session grant for host → true unless `everyTime`; else `r.ask(ctx, "share_page", fmt.Sprintf("Send what the agent reads on %s to %s?", host, provider), false)`; yes → grant host for the session (not when `everyTime`). During a fired turn, session grants are ignored and every host asks through `r.ask` (FiredAsk + deadline), consistent with "a fired turn takes no session shortcut"; a grant made inside a fired turn is not kept.
- Browser `finish`: after the existing `mayShow`/`stillSeen` checks pass, call `shareOK(ctx, host, t.session.MyChrome())` on the **final** page host (after the post-read re-check); refused → `withhold(len(pageNotes))` with message `the page on <host> is not shown (not shared with <Provider>)`. Tab titles in the `tabs` action: shown only for hosts already shared or allow-tier; others show host only.
- `web_fetch`: judged on the final (post-redirect) host before returning content; refused → error result `not shared with <Provider>: <host>`; allow tier (existing consent) skips.
- `web_search`: one session grant keyed `"(web search results)"`, prompt `Send web search results to <Provider>?`.
- cmd wires `SetShareGate` only when the main model is online (after the gate in Task 7 passes) and clears it on a switch to the helper.

- [ ] **Step 1: Failing tests** — `TestSharePageAskedOncePerHost`; `TestSharePageEveryTimeInMyChrome`; `TestSharePageAllowTierSkips`; `TestSharePageRefusedWithholds` (snapshot text absent, message present); `TestSharePageJudgedOnFinalHost` (open a.example granted, page redirects to b.example → asks for b, refusal withholds); `TestWebFetchShareGate`; `TestWebSearchShareOncePerSession`; `TestShareGateNilWhenLocal` (no prompt at all); `TestSharePageFiredTurnIgnoresSessionGrant`.
- [ ] **Step 2–4:** FAIL → implement → PASS (-race); verify.
- [ ] **Step 5: Commit** `tools: share_page — page text reaches an online model only with consent`.

---

### Task 9: Outage fallback to the local helper

**Files:** Modify `internal/agent/resilience.go` (after `chatWithRetry` exhausts retries on a transient error), `internal/agent/online.go`; Tests `internal/agent/outage_test.go`.

**Interfaces — Produces:**
- When the main model is online, a primary call fails after the retry bound with a transient (non-auth) error, and a helper is configured and reachable: ask `switch_to_local` — `<Provider> is not responding. Switch this session to your local model (<helper model>)?`; yes → `switchToHelper("…")` (Task 7) and retry the call once on the helper; no / nil approver / fired turn → return the original error (a fired turn adds notice `<Provider> unreachable; the scheduled event stopped`).
- Never offered for 401/403 or a spend-cap refusal.

- [ ] **Step 1: Failing tests** — `TestOutageOffersSwitchAndRetriesOnHelper`; `TestOutageDeclinedReturnsError`; `TestOutageNotOfferedForAuthError`; `TestOutageInFiredTurnStops`.
- [ ] **Step 2–4:** FAIL → implement → PASS (-race); verify.
- [ ] **Step 5: Commit** `agent: offer the local helper when the online provider is down`.

---

### Task 10: Setup wizard online path, docs, live checklist

**Files:** Modify `internal/setup/wizard.go` (~19), `internal/setup/probe.go` (BuildConfig ~74 gains a variant); `README.md`, `CHANGELOG.md` (1.2.0), `docs/live-checklist.md`; root `/home/sbrown/Documents/Development/BE-CodeRedux/CLAUDE.md` (outside git — edit in place, never `git add`); Tests `internal/setup/wizard_test.go`.

**Interfaces — Produces:**
- Wizard: after listing local backends, an extra numbered choice `use an online provider` → pick a preset (OpenRouter first) → if `os.Getenv(KeyEnv)` is empty print `set <KEY_ENV> in your shell, then run be-code setup again` and save nothing → else ListModels (via an injectable lister for tests) → pick a model → `BuildOnlineConfig(preset, model, helper *Found)` writes the preset provider (online true), `default_provider`, `model`, and `local_helper` from the first local backend found (if any).
- Docs: README "Online providers" section (presets, keys in env, per-project prompt, per-site sharing, spend and `max_spend_usd`, `local_helper`, `/online`); CHANGELOG bullets; live checklist items for a real OpenRouter session, a deliberate cap hit, a helper-down run, a declined project prompt; CLAUDE.md section "Online providers (1.2.0)" summarising the seams (presets, ProviderIsOnline, HTTPError/Retry-After, OnlineWindow/Pricing, spend cap, HelperFactory/choreModel, online gate + online.json, ShareGate, outage switch, new no-always actions).

- [ ] **Step 1: Failing tests** — `TestWizardOnlinePathWritesPresetAndHelper` (scripted stdin, injected lister, env key set → config has openrouter provider online, model, local_helper = the found Ollama); `TestWizardOnlineMissingKeySavesNothing`.
- [ ] **Step 2–4:** FAIL → implement → PASS; `go test ./... -count=1`; `make -f build.mk verify`.
- [ ] **Step 5: Commit** `setup, docs: online providers in the wizard, README, CHANGELOG, live checklist`.
