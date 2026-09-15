# AI Model Co-Working Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The primary local model can consult a configured co-working model mid-task (by calling a `consult` tool, or when the harness sees it is stuck); the co-worker inspects the repository with read-only tools and answers; the answer shows on every terminal as a second voice; online co-workers need per-session consent; an unreachable co-worker never interrupts the run.

**Architecture:** `agent.Consult` runs a bounded read-only scratch agent (the `planAgent` shape) on the co-worker's provider, gated by the existing `Tools.Approve` seam, and reports through three new `agent.Events`. A built-in `consult` tool and two harness triggers in `RunFull`/`run` call it. Both UIs render two new entry kinds in a per-theme `Cowork` colour and add `/coworkers` and `/consult`.

**Tech Stack:** Go 1.22, existing `internal/agent`, `internal/tools`, `internal/config`, `internal/tui` (Session/View), `internal/ui` (plain REPL), the Python mock backend in `test/e2e`.

**Spec:** `docs/superpowers/specs/2026-09-14-ai-coworking-design.md`

## Global Constraints

- Target version **0.9.0** (`build.mk` VERSION, CHANGELOG heading, root `CLAUDE.md`).
- **Every task leaves `make -f build.mk verify` green.** Every test package that can reach `~/.be-code` already runs under a `TestMain` HOME guard; keep it so (never remove `testmain_test.go`).
- **A consultation never aborts, blocks or fails the primary's run.** Every failure path in `Consult` returns an error or a partial result to its caller and counts against the cap; `RunFull`/`run` treat a failed consultation as "no advice".
- **The co-worker is read-only:** its registry is `Registry.Subset("read_file", "list_dir", "search")` rooted at the workspace; never `write_file`, `edit_file`, `shell`, `process`, MCP or IDE tools.
- **Consent for `online: true` co-workers** goes through `a.Tools.Approve("consult", detail)` before any scratch agent exists; local co-workers never prompt; `/consult` from a terminal never prompts; headless `-y` allows, headless without `-y` declines (error `consultation declined`).
- Exact strings: tool result prefix `co-worker <name> replied:\n\n`; partial suffix `\n\n(the co-worker was cut short; this is what it had)`; errors `consultation limit reached for this run (N)`, `consultation declined`, `unknown co-worker %q (configured: a, b)`; transient notice `consulting <name>…`; startup warning `warn: coworker %q: %s`; status `consulting <name> · N files read`; summary line `<name> read N files in Ns`; transcript prefixes `<name>? ` (question) and `<name>> ` (answer).
- Exact trigger prompts (spec §3): verify — question `the change still fails this check; what is wrong and what minimal edit fixes it`, extra repair prompt `A co-worker (<name>) reviewed the failing check and advises:\n\n<answer>\n\nApply the minimal fix, then stop.`; tool — advice note `A co-worker (<name>) looked at the repeated <tool> failure and advises:\n\n<answer>`.
- Config defaults: `cowork.auto` true, `max_consults_per_run` 3, `consult_turns` 12; `coworkers` empty. With an empty list the `consult` tool is not registered and nothing else changes.
- `agent` must not import the provider registry: `agent.CoworkerFactory` is injected from `cmd/root.go` exactly like `ReviewerFactory`.
- Commit messages end with:
  ```
  Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
  Claude-Session: https://claude.ai/code/session_01BZcp8PaqA96XDvu2GeDcAb
  ```
- Never touch the user's real `~/.be-code`; tests use the package `TestMain` HOME guard or `tempHome(t)`.

---

## File structure

| File | Responsibility |
|---|---|
| `internal/config/config.go` | `CoworkerConfig`, `CoworkConfig`, fields `Coworkers`, `Cowork`, defaults, `ValidCoworkers()` (validation without provider construction). |
| `internal/agent/cowork.go` | `ConsultRequest`, `ConsultResult`, `CoworkerFactory`, `ConsultFrame`, `Agent.Consult`, `Agent.AllowCoworker`, `Agent.Coworkers()`, `Agent.ConsultsThisRun()`, consent, cap, the scratch agent, seed building, progress events. |
| `internal/agent/loop.go` | `Events.OnConsultStart/OnConsultProgress/OnConsultEnd`; `RunFull` resets the cap and adds the verify trigger; `run` adds the repeated-tool-failure trigger. |
| `internal/agent/extras.go` | `planAgent` includes `consult` in its subset. |
| `internal/tools/consult.go` | The `consult` tool: schema, description with the roster, delegating to a `ConsultFunc` supplied by cmd. |
| `cmd/root.go` | `CoworkerFactory`, startup validation warnings, tool registration, headless consent (`-y`). |
| `internal/ui/common.go` | `/coworkers` and `/consult` rows (busy-safe). |
| `internal/ui/repl.go` | Plain-mode commands and event printing. |
| `internal/tui/theme.go`, `entry.go`, `session.go`, `view.go`, `palette.go` | `Cowork` colour, entry kinds, event handlers, `/coworkers`, `/consult` as a turn, consent `a` key. |
| `test/e2e/mock_server.py`, `run_e2e.sh` | A second scripted endpoint as the co-worker; one consult scenario. |
| `README.md`, `CHANGELOG.md`, `build.mk`, `docs/live-checklist.md`, root `CLAUDE.md` | Docs, version. |

Task order: 1 config → 2 agent core → 3 tool → 4 triggers → 5 cmd + plain mode → 6 TUI → 7 E2E + docs.

---

### Task 1: Configuration

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces:

```go
type CoworkerConfig struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Skills   string `json:"skills,omitempty"`
	Online   bool   `json:"online,omitempty"`
}

type CoworkConfig struct {
	Auto              bool `json:"auto"`
	MaxConsultsPerRun int  `json:"max_consults_per_run"`
	ConsultTurns      int  `json:"consult_turns"`
}

// on Config:
Coworkers []CoworkerConfig `json:"coworkers"`
Cowork    CoworkConfig     `json:"cowork"`

// ValidCoworkers returns the usable co-workers in configured order and one
// warning per dropped entry (formatted "coworker %q: %s").
func (c *Config) ValidCoworkers() ([]CoworkerConfig, []string)
```

- Defaults in `Default()`: `Cowork: CoworkConfig{Auto: true, MaxConsultsPerRun: 3, ConsultTurns: 12}`, `Coworkers: nil`. `Load()` fills a zero `Cowork` from an older file with the defaults (a file that has `"cowork": {}` must not end with `consult_turns: 0`: treat 0 as default for the two ints; `Auto` keeps the JSON value only when the `cowork` key was present — simplest: decode into a shadow struct with `*bool`; if nil, set true).

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`:

```go
func TestCoworkDefaults(t *testing.T) {
	c := Default()
	if !c.Cowork.Auto || c.Cowork.MaxConsultsPerRun != 3 || c.Cowork.ConsultTurns != 12 {
		t.Fatalf("cowork defaults = %+v", c.Cowork)
	}
	if len(c.Coworkers) != 0 {
		t.Fatalf("default coworkers = %+v", c.Coworkers)
	}
}

func TestValidCoworkersDropsBrokenEntries(t *testing.T) {
	c := Default()
	c.Coworkers = []CoworkerConfig{
		{Name: "claude", Provider: "anthropic", Model: "claude-opus-5", Online: true}, // provider not configured
		{Name: "big", Provider: "ollama", Model: "qwen3:32b", Skills: "long reads"},
		{Name: "", Provider: "ollama", Model: "x"},
		{Name: "big", Provider: "ollama", Model: "dup"},
		{Name: "nomodel", Provider: "ollama"},
	}
	ok, warns := c.ValidCoworkers()
	if len(ok) != 1 || ok[0].Name != "big" || ok[0].Model != "qwen3:32b" {
		t.Fatalf("valid = %+v", ok)
	}
	want := []string{
		`coworker "claude": provider "anthropic" is not configured`,
		`coworker "": name is empty`,
		`coworker "big": duplicate name`,
		`coworker "nomodel": model is empty`,
	}
	if len(warns) != len(want) {
		t.Fatalf("warnings = %q", warns)
	}
	for i := range want {
		if warns[i] != want[i] {
			t.Errorf("warning %d = %q, want %q", i, warns[i], want[i])
		}
	}
}

func TestCoworkLoadFillsMissingValues(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	p, _ := Path()
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte(`{"theme":"dark","cowork":{"max_consults_per_run":5}}`), 0o600)
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if !c.Cowork.Auto || c.Cowork.MaxConsultsPerRun != 5 || c.Cowork.ConsultTurns != 12 {
		t.Fatalf("loaded cowork = %+v", c.Cowork)
	}
	os.WriteFile(p, []byte(`{"cowork":{"auto":false}}`), 0o600)
	c, _ = Load()
	if c.Cowork.Auto {
		t.Fatal("explicit auto:false was overridden")
	}
}
```

(Add `"os"` and `"path/filepath"` imports if missing.)

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config/ -run 'TestCowork|TestValidCoworkers' 2>&1 | head`
Expected: build failure `undefined: CoworkerConfig`.

- [ ] **Step 3: Implement**

In `internal/config/config.go`: add the two types after `ReviewerConfig`; add the fields to `Config` after `Reviewer` with doc comments (`// Coworkers are models the primary can consult mid-task (see README "Co-working models"); order matters: the first is the default.` / `// Cowork tunes consultations: Auto enables the harness's own triggers, MaxConsultsPerRun caps consultations per request, ConsultTurns caps a co-worker's tool loop.`); set the defaults in `Default()`.

`Load()`: after `json.Unmarshal(data, cfg)`, add

```go
	// An older file, or one written by hand without the cowork block, must
	// not zero the tuning: 0 means "default" for the two counts, and auto
	// stays on unless the file says otherwise.
	if cfg.Cowork.MaxConsultsPerRun == 0 {
		cfg.Cowork.MaxConsultsPerRun = 3
	}
	if cfg.Cowork.ConsultTurns == 0 {
		cfg.Cowork.ConsultTurns = 12
	}
	var probe struct {
		Cowork *struct {
			Auto *bool `json:"auto"`
		} `json:"cowork"`
	}
	if json.Unmarshal(data, &probe) == nil && (probe.Cowork == nil || probe.Cowork.Auto == nil) {
		cfg.Cowork.Auto = true
	}
```

`ValidCoworkers`:

```go
// ValidCoworkers is the configured co-workers that can actually be used,
// in order, plus one warning per entry dropped: an empty name or model, a
// provider that is not in Providers, or a name already taken.
func (c *Config) ValidCoworkers() ([]CoworkerConfig, []string) {
	var ok []CoworkerConfig
	var warns []string
	seen := map[string]bool{}
	for _, cw := range c.Coworkers {
		switch {
		case cw.Name == "":
			warns = append(warns, fmt.Sprintf("coworker %q: name is empty", cw.Name))
		case cw.Model == "":
			warns = append(warns, fmt.Sprintf("coworker %q: model is empty", cw.Name))
		case seen[cw.Name]:
			warns = append(warns, fmt.Sprintf("coworker %q: duplicate name", cw.Name))
		default:
			if _, found := c.Providers[cw.Provider]; !found {
				warns = append(warns, fmt.Sprintf("coworker %q: provider %q is not configured", cw.Name, cw.Provider))
				continue
			}
			seen[cw.Name] = true
			ok = append(ok, cw)
		}
	}
	return ok, warns
}
```

Note the check order in the test: the empty-name entry is reported before its (also missing) provider check, and the duplicate before the model check does not matter since "big"/"dup" has a model. Keep the `switch` order name → model → duplicate → provider.

- [ ] **Step 4: Run the package**

Run: `go test ./internal/config/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/config && git commit -m "config: coworkers and cowork settings with validation"
```

---

### Task 2: `agent.Consult` — the scratch co-worker agent, consent and cap

**Files:**
- Create: `internal/agent/cowork.go`
- Modify: `internal/agent/loop.go` (`Events`, `Agent` fields, `RunFull` cap reset)
- Test: `internal/agent/cowork_test.go`

**Interfaces:**
- Consumes: `config.CoworkerConfig`, `config.CoworkConfig`, `(*config.Config).ValidCoworkers()`; `planAgent`'s construction shape; `Tools.Approve`.
- Produces:

```go
// internal/agent/cowork.go
type ConsultRequest struct {
	Who      string   // co-worker name; "" = the first configured
	Question string
	Files    []string // workspace-relative paths to read first
	Origin   string   // "tool" | "auto:verify" | "auto:tool" | "user:<label>"
	Recent   string   // caller-supplied recent context (RunFull/run fill it; /consult leaves it "")
}

type ConsultResult struct {
	Coworker string
	Answer   string
	Partial  bool
	Read     int
	Elapsed  time.Duration
}

// CoworkerFactory builds the provider for a co-worker; injected by cmd.
var CoworkerFactory func(cfg *config.Config, cw config.CoworkerConfig) (provider.Provider, error)

// ConsultFrame is the co-worker's system prompt (project notes appended).
const ConsultFrame = `You are a co-working model advising a smaller local model that is doing the work and keeps the pen. ...`

func (a *Agent) Coworkers() []config.CoworkerConfig            // ValidCoworkers, cached at New
func (a *Agent) Consult(ctx context.Context, req ConsultRequest) (ConsultResult, error)
func (a *Agent) AllowCoworker(name string)                     // session-wide consent (the approval modal's `a`)
func (a *Agent) ConsultsThisRun() int
func (a *Agent) ConsultCount(name string) int                  // per session, for /coworkers

// Events (loop.go):
OnConsultStart    func(name, question, origin string)
OnConsultProgress func(name string, filesRead int)
OnConsultEnd      func(res ConsultResult, err error)
```

- Semantics (spec §2, §4, §6):
  - `Consult` resolves `req.Who` ("" → first; unknown → `fmt.Errorf("unknown co-worker %q (configured: %s)", who, strings.Join(names, ", "))`).
  - Cap: `a.consults` (int, reset to 0 at the top of `RunFull`) is incremented for origins other than `user:*`; at `>= a.Cfg.Cowork.MaxConsultsPerRun` returns `fmt.Errorf("consultation limit reached for this run (%d)", max)` **before** incrementing. `/consult` (`user:*`) never counts.
  - Consent: for `cw.Online` and origin not `user:*`: if `a.coworkAllowed[name]` → go; else if `a.Cfg.AutoApproveShell` (what `-y` sets) → allowed for the session; else if `a.Tools.Approve == nil` → `errors.New("consultation declined")`; else `ok := a.Tools.Approve("consult", detail)` with `detail` = lines `coworker: <name> (<provider>/<model>)`, `origin: <origin>`, `question: <question>`, `files: <comma list or none>`, and `it may read other files in this workspace; nothing is edited` — `false` → `consultation declined`. (`AllowCoworker` is what the UI calls when the person pressed `a`; a plain `y` allows this consultation only.)
  - Serialisation: `a.consultMu` (a `sync.Mutex`) held for the whole call; a concurrent automatic trigger that finds it locked (`TryLock`) returns `errors.New("a consultation is already running")`.
  - Scratch agent: copy `planAgent` but with `readOnly := a.Tools.Subset("read_file", "list_dir", "search")`, `Provider: cp` from `CoworkerFactory`, `Model: cw.Model`, `Profile: profiles.Detect(cw.Model)`, `compat` from the profile, `Events` limited to a private progress counter (wrap `OnToolEnd` to count `read_file` calls and emit `OnConsultProgress`), `systemOverride = ConsultFrame + "\n\n" + projectNotes` (+ the compat tool-format tail as `planAgent` does), `History = NewHistory(sys, a.History.Budget)` with the same `Reserve`/`CharsPerToken`, `Cfg` a shallow copy of `a.Cfg` with `MaxTurns = cfg.Cowork.ConsultTurns`, `VerifyOnDone=false`, `ReviewOnDone=false`. Stats of the scratch agent are folded into `a.Stats` as `Plan` does.
  - Seed: `buildConsultSeed(req, files)` → the question; then `## Files` with each named file's content (`os.ReadFile` under the registry root via `a.Tools` confinement — use the read tool through `readOnly.Dispatch` to keep confinement) capped at 8 KiB each; then `## Recent context` = `req.Recent` capped at 6 KiB; then `## Git` = `gitctx.Summary`.
  - Run: `answer, err := scratch.Run(ctx, seed)`; `Read` = counted `read_file` successes; `Elapsed` measured. If `err != nil` and `scratch.lastAssistantText()` (the newest assistant message content in its History) is non-empty → return `ConsultResult{Partial: true, Answer: that}` and `nil` error; else the error. Cancelled ctx → `ctx.Err()`.
  - Events: `OnConsultStart(name, question, origin)` right after consent; `OnConsultEnd(res, err)` always (deferred).

- [ ] **Step 1: Write the failing tests**

`internal/agent/cowork_test.go`:

```go
package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

func withCoworkers(names ...string) func(*config.Config) {
	return func(c *config.Config) {
		for i, n := range names {
			online := strings.HasPrefix(n, "online-")
			c.Coworkers = append(c.Coworkers, config.CoworkerConfig{Name: n, Provider: "ollama", Model: "cw-model-" + n, Skills: "skill " + n, Online: online})
			_ = i
		}
	}
}

// coworkerStub scripts the co-worker's replies and records every request.
func coworkerStub(t *testing.T, responses ...provider.ChatResponse) *scriptedProvider {
	t.Helper()
	p := &scriptedProvider{responses: responses}
	CoworkerFactory = func(cfg *config.Config, cw config.CoworkerConfig) (provider.Provider, error) { return p, nil }
	t.Cleanup(func() { CoworkerFactory = nil })
	return p
}

func TestConsultRunsAReadOnlyScratchAgentAndReturnsTheAnswer(t *testing.T) {
	cw := coworkerStub(t,
		provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "1", Name: "read_file", Arguments: `{"path":"main.go"}`}}},
		provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "2", Name: "write_file", Arguments: `{"path":"main.go","content":"x"}`}}},
		provider.ChatResponse{Content: "Use strings.Builder in main.go:12."},
	)
	ag, dir := newTestAgent(t, &scriptedProvider{}, withCoworkers("big"))
	os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644)
	var started, ended int
	ag.Events.OnConsultStart = func(name, q, origin string) { started++ }
	ag.Events.OnConsultEnd = func(res ConsultResult, err error) { ended++ }
	res, err := ag.Consult(context.Background(), ConsultRequest{Question: "why does main.go not compile?", Files: []string{"main.go"}, Origin: "tool"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Coworker != "big" || res.Answer != "Use strings.Builder in main.go:12." || res.Partial || res.Read != 1 {
		t.Fatalf("result = %+v", res)
	}
	if started != 1 || ended != 1 {
		t.Fatalf("events start=%d end=%d", started, ended)
	}
	// The write attempt was refused: the file is untouched and the model was told.
	if b, _ := os.ReadFile(filepath.Join(dir, "main.go")); string(b) != "package main\n" {
		t.Fatal("the co-worker edited a file")
	}
	seed := cw.lastReq.Messages[1].Content // [0] system, [1] seed (the tool result turns follow)
	if !strings.Contains(cw.lastReq.Messages[0].Content, "keeps the pen") {
		t.Fatalf("system prompt is not the consult frame: %q", cw.lastReq.Messages[0].Content[:80])
	}
	_ = seed
	first := cw.lastReq.Messages[1].Content
	if !strings.Contains(first, "why does main.go not compile?") || !strings.Contains(first, "package main") {
		t.Fatalf("seed lacks the question or the named file:\n%s", first)
	}
	for _, spec := range cw.lastReq.Tools {
		if spec.Name == "write_file" || spec.Name == "shell" {
			t.Fatalf("co-worker was offered %s", spec.Name)
		}
	}
	if ag.ConsultsThisRun() != 1 || ag.ConsultCount("big") != 1 {
		t.Fatalf("counters: run=%d big=%d", ag.ConsultsThisRun(), ag.ConsultCount("big"))
	}
}

func TestConsultCapAndUnknownName(t *testing.T) {
	coworkerStub(t, provider.ChatResponse{Content: "ok"})
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { withCoworkers("a", "b")(c); c.Cowork.MaxConsultsPerRun = 2 })
	for i := 0; i < 2; i++ {
		if _, err := ag.Consult(context.Background(), ConsultRequest{Question: "q", Origin: "tool"}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := ag.Consult(context.Background(), ConsultRequest{Question: "q", Origin: "tool"})
	if err == nil || err.Error() != "consultation limit reached for this run (2)" {
		t.Fatalf("cap error = %v", err)
	}
	// /consult never counts.
	if _, err := ag.Consult(context.Background(), ConsultRequest{Question: "q", Origin: "user:desk"}); err != nil {
		t.Fatalf("user consult blocked by the cap: %v", err)
	}
	_, err = ag.Consult(context.Background(), ConsultRequest{Who: "nobody", Question: "q", Origin: "user:desk"})
	if err == nil || err.Error() != `unknown co-worker "nobody" (configured: a, b)` {
		t.Fatalf("unknown error = %v", err)
	}
	// RunFull resets the run counter.
	ag.RunFull(context.Background(), "hi")
	if ag.ConsultsThisRun() != 0 {
		t.Fatal("cap not reset by RunFull")
	}
}

func TestConsultConsentForOnlineCoworkers(t *testing.T) {
	cw := coworkerStub(t, provider.ChatResponse{Content: "advice"})
	ag, _ := newTestAgent(t, &scriptedProvider{}, withCoworkers("online-claude", "local"))
	var asked []string
	answer := false
	ag.Tools.Approve = func(action, detail string) bool { asked = append(asked, action+"|"+detail); return answer }
	if _, err := ag.Consult(context.Background(), ConsultRequest{Question: "secret", Origin: "tool"}); err == nil || err.Error() != "consultation declined" {
		t.Fatalf("declined error = %v", err)
	}
	if len(cw.lastReq.Messages) != 0 {
		t.Fatal("something was sent to the online co-worker before consent")
	}
	if len(asked) != 1 || !strings.HasPrefix(asked[0], "consult|coworker: online-claude") || !strings.Contains(asked[0], "question: secret") {
		t.Fatalf("approval detail: %q", asked)
	}
	answer = true
	if _, err := ag.Consult(context.Background(), ConsultRequest{Question: "q2", Origin: "tool"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ag.Consult(context.Background(), ConsultRequest{Question: "q3", Origin: "tool"}); err != nil || len(asked) != 3 {
		t.Fatalf("a plain yes must allow one consultation only: err=%v asked=%d", err, len(asked))
	}
	ag.AllowCoworker("online-claude")
	ag.Consult(context.Background(), ConsultRequest{Question: "q4", Origin: "auto:verify"})
	if len(asked) != 3 {
		t.Fatal("AllowCoworker did not suppress the prompt")
	}
	// Local co-workers never prompt; /consult never prompts.
	ag.Consult(context.Background(), ConsultRequest{Who: "local", Question: "q", Origin: "tool"})
	ag.Consult(context.Background(), ConsultRequest{Who: "online-claude", Question: "q", Origin: "user:desk"})
	if len(asked) != 3 {
		t.Fatalf("local or user consult prompted: %d", len(asked))
	}
	// No approver at all (a non-interactive run without -y) declines.
	ag.Tools.Approve = nil
	ag2, _ := newTestAgent(t, &scriptedProvider{}, withCoworkers("online-x"))
	ag2.Tools.Approve = nil
	if _, err := ag2.Consult(context.Background(), ConsultRequest{Question: "q", Origin: "tool"}); err == nil || err.Error() != "consultation declined" {
		t.Fatalf("headless without -y: %v", err)
	}
	// -y (AutoApproveShell) allows without a prompt.
	ag3, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { withCoworkers("online-y")(c); c.AutoApproveShell = true })
	ag3.Tools.Approve = nil
	if _, err := ag3.Consult(context.Background(), ConsultRequest{Question: "q", Origin: "tool"}); err != nil {
		t.Fatalf("-y: %v", err)
	}
}

func TestConsultReturnsPartialOnMidwayError(t *testing.T) {
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls == 1 {
			return &provider.ChatResponse{Content: "First thought.", ToolCalls: []provider.ToolCall{{ID: "1", Name: "list_dir", Arguments: `{}`}}}, nil
		}
		return nil, errors.New("connection reset")
	}}
	CoworkerFactory = func(cfg *config.Config, cw config.CoworkerConfig) (provider.Provider, error) { return p, nil }
	t.Cleanup(func() { CoworkerFactory = nil })
	ag, _ := newTestAgent(t, &scriptedProvider{}, withCoworkers("flaky"))
	res, err := ag.Consult(context.Background(), ConsultRequest{Question: "q", Origin: "tool"})
	if err != nil || !res.Partial || res.Answer != "First thought." {
		t.Fatalf("partial: res=%+v err=%v", res, err)
	}
	// An error before any answer is an error, and still counts.
	p.fn = func(req provider.ChatRequest) (*provider.ChatResponse, error) { return nil, errors.New("down") }
	if _, err := ag.Consult(context.Background(), ConsultRequest{Question: "q", Origin: "tool"}); err == nil {
		t.Fatal("expected an error")
	}
	if ag.ConsultsThisRun() != 2 {
		t.Fatalf("failed consultations must count: %d", ag.ConsultsThisRun())
	}
}

func TestConsultTurnCap(t *testing.T) {
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "1", Name: "list_dir", Arguments: `{}`}}}, nil
	}}
	CoworkerFactory = func(cfg *config.Config, cw config.CoworkerConfig) (provider.Provider, error) { return p, nil }
	t.Cleanup(func() { CoworkerFactory = nil })
	ag, _ := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) { withCoworkers("loop")(c); c.Cowork.ConsultTurns = 3 })
	ag.Consult(context.Background(), ConsultRequest{Question: "q", Origin: "tool"})
	if len(p.reqs) != 3 {
		t.Fatalf("co-worker ran %d turns, want 3", len(p.reqs))
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/agent/ -run 'TestConsult' 2>&1 | head -5`
Expected: build failure `undefined: ConsultRequest`.

- [ ] **Step 3: Implement**

`internal/agent/loop.go`: add to `Events`:

```go
	// Co-working (see cowork.go): a consultation starting, the co-worker
	// reading files, and its result. All optional.
	OnConsultStart    func(name, question, origin string)
	OnConsultProgress func(name string, filesRead int)
	OnConsultEnd      func(res ConsultResult, err error)
```

Add to `Agent`: `coworkers []config.CoworkerConfig`, `consults int`, `consultCount map[string]int`, `coworkAllowed map[string]bool`, `consultMu sync.Mutex`. In `New`, after the existing setup: `a.coworkers, _ = cfg.ValidCoworkers(); a.consultCount = map[string]int{}; a.coworkAllowed = map[string]bool{}` (the warnings are printed by cmd, which calls `ValidCoworkers` itself). At the top of `RunFull`: `a.consults = 0`.

`internal/agent/cowork.go` — full file:

```go
package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/gitctx"
	"github.com/brown-enterprises/be-code/internal/profiles"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// A co-working model is a second, usually stronger, model the primary can
// consult mid-task. It inspects the repository with read-only tools and
// answers; the primary keeps every write and shell capability. Consult is
// the one entry point: the consult tool, the harness's own triggers in
// RunFull/run, and the /consult command all come through here.

type ConsultRequest struct {
	Who      string   // co-worker name; "" = the first configured
	Question string
	Files    []string // workspace-relative paths to read first
	Origin   string   // "tool" | "auto:verify" | "auto:tool" | "user:<label>"
	Recent   string   // the caller's recent context, already condensed
}

type ConsultResult struct {
	Coworker string
	Answer   string
	Partial  bool
	Read     int
	Elapsed  time.Duration
}

// CoworkerFactory builds a co-worker's provider. Injected by cmd, like
// ReviewerFactory, so this package never imports the provider registry.
var CoworkerFactory func(cfg *config.Config, cw config.CoworkerConfig) (provider.Provider, error)

// ConsultFrame is the co-worker's system prompt. The project notes are
// appended after it.
const ConsultFrame = `You are a co-working model advising a smaller local model that is doing the work and keeps the pen. It has asked for help because it is stuck or out of its depth.

Rules:
- Inspect what you need with the read-only tools (read_file, list_dir, search) before answering. You cannot edit files or run commands, and you must not present running a command as the answer.
- Answer with concrete, minimal guidance: what is wrong, and the exact change to make, with file:line references. Prefer the smallest change that fixes the problem.
- If the question cannot be answered from the code, say so plainly and say what would be needed.
- Be brief. The reader is a small model: short numbered steps beat prose.`

const (
	consultFileCap   = 8 * 1024
	consultRecentCap = 6 * 1024
)

// Coworkers is the usable co-worker list, in configured order.
func (a *Agent) Coworkers() []config.CoworkerConfig { return a.coworkers }

// ConsultsThisRun is how many consultations the current RunFull has used
// (tool-initiated and automatic together; /consult never counts).
func (a *Agent) ConsultsThisRun() int { return a.consults }

// ConsultCount is how many times a co-worker has been consulted this session.
func (a *Agent) ConsultCount(name string) int { return a.consultCount[name] }

// AllowCoworker records session-wide consent for an online co-worker: the
// approval modal's "a".
func (a *Agent) AllowCoworker(name string) { a.coworkAllowed[name] = true }

func (a *Agent) coworkerByName(who string) (config.CoworkerConfig, error) {
	if len(a.coworkers) == 0 {
		return config.CoworkerConfig{}, errors.New("no co-working models are configured")
	}
	if who == "" {
		return a.coworkers[0], nil
	}
	names := make([]string, 0, len(a.coworkers))
	for _, cw := range a.coworkers {
		if cw.Name == who {
			return cw, nil
		}
		names = append(names, cw.Name)
	}
	return config.CoworkerConfig{}, fmt.Errorf("unknown co-worker %q (configured: %s)", who, strings.Join(names, ", "))
}

// consent decides whether code may be sent to cw for this request. Local
// co-workers never ask; a person's own /consult never asks; -y allows;
// otherwise the approval seam is asked once, and "a" (AllowCoworker) or a
// previous session-wide yes skips it.
func (a *Agent) consent(cw config.CoworkerConfig, req ConsultRequest) bool {
	if !cw.Online || strings.HasPrefix(req.Origin, "user:") || a.coworkAllowed[cw.Name] {
		return true
	}
	if a.Cfg.AutoApproveShell { // what -y sets
		a.coworkAllowed[cw.Name] = true
		return true
	}
	if a.Tools.Approve == nil {
		return false
	}
	files := "none"
	if len(req.Files) > 0 {
		files = strings.Join(req.Files, ", ")
	}
	detail := fmt.Sprintf("coworker: %s (%s/%s)\norigin: %s\nquestion: %s\nfiles: %s\nit may read other files in this workspace; nothing is edited",
		cw.Name, cw.Provider, cw.Model, req.Origin, req.Question, files)
	return a.Tools.Approve("consult", detail)
}

// Consult runs one consultation. It never affects the primary's own
// history; its outcome is returned to the caller, who decides what to do
// with it. Every failure is an error (or a partial result), never a panic
// or a hang: the primary's run must go on without the co-worker.
func (a *Agent) Consult(ctx context.Context, req ConsultRequest) (res ConsultResult, err error) {
	if !a.consultMu.TryLock() {
		return res, errors.New("a consultation is already running")
	}
	defer a.consultMu.Unlock()

	cw, err := a.coworkerByName(req.Who)
	if err != nil {
		return res, err
	}
	res.Coworker = cw.Name
	counts := !strings.HasPrefix(req.Origin, "user:")
	if counts && a.consults >= a.Cfg.Cowork.MaxConsultsPerRun {
		return res, fmt.Errorf("consultation limit reached for this run (%d)", a.Cfg.Cowork.MaxConsultsPerRun)
	}
	if !a.consent(cw, req) {
		return res, errors.New("consultation declined")
	}
	if counts {
		a.consults++
	}
	a.consultCount[cw.Name]++
	if CoworkerFactory == nil {
		return res, errors.New("co-working is not wired in this build")
	}
	cp, err := CoworkerFactory(a.Cfg, cw)
	if err != nil {
		return res, fmt.Errorf("co-worker %s unavailable: %w", cw.Name, err)
	}

	if a.Events.OnConsultStart != nil {
		a.Events.OnConsultStart(cw.Name, req.Question, req.Origin)
	}
	start := time.Now()
	defer func() {
		res.Elapsed = time.Since(start)
		if a.Events.OnConsultEnd != nil {
			a.Events.OnConsultEnd(res, err)
		}
	}()

	scratch := a.consultAgent(cp, cw, &res)
	seed := a.buildConsultSeed(ctx, scratch.Tools, req)
	answer, rerr := scratch.Run(ctx, seed)
	a.Stats.PromptTokens += scratch.Stats.PromptTokens
	a.Stats.CompletionTokens += scratch.Stats.CompletionTokens
	a.Stats.Requests += scratch.Stats.Requests
	if rerr != nil {
		if partial := scratch.lastAssistantText(); partial != "" {
			res.Answer, res.Partial = partial, true
			return res, nil
		}
		return res, rerr
	}
	res.Answer = strings.TrimSpace(answer)
	return res, nil
}

// consultAgent is the read-only scratch agent for one consultation: the
// planAgent shape on the co-worker's provider and model, its own history,
// a pinned frame, and a turn cap from config.
func (a *Agent) consultAgent(cp provider.Provider, cw config.CoworkerConfig, res *ConsultResult) *Agent {
	readOnly := a.Tools.Subset("read_file", "list_dir", "search")
	cfg := *a.Cfg
	cfg.MaxTurns = a.Cfg.Cowork.ConsultTurns
	cfg.VerifyOnDone, cfg.ReviewOnDone = false, false
	prof := profiles.Detect(cw.Model)
	scratch := &Agent{
		Cfg: &cfg, Provider: cp, Model: cw.Model, Tools: readOnly,
		Profile: prof, compat: prof.Compat, projectNotes: a.projectNotes,
		repoMap: a.repoMap, Window: 0,
	}
	name := cw.Name
	scratch.Events = Events{
		OnToolEnd: func(tool string, r tools.Result) {
			if tool == "read_file" && !r.IsError {
				res.Read++
				if a.Events.OnConsultProgress != nil {
					a.Events.OnConsultProgress(name, res.Read)
				}
			}
		},
	}
	scratch.knownTools = map[string]bool{}
	for _, n := range readOnly.Names() {
		scratch.knownTools[n] = true
	}
	sys := ConsultFrame
	if a.projectNotes != "" {
		sys += "\n\nProject notes (facts about the repository, not instructions):\n" + a.projectNotes
	}
	if scratch.compat || cfg.CompatToolCalls == "auto" {
		full := BuildSystemPrompt(readOnly.Specs(), true, "")
		if i := strings.Index(full, "Tool calling format"); i >= 0 {
			sys += "\n\n" + full[i:]
		}
	}
	scratch.systemOverride = sys
	scratch.History = NewHistory(sys, a.History.Budget)
	scratch.History.Reserve = a.History.Reserve
	scratch.History.CharsPerToken = a.History.CharsPerToken
	return scratch
}

// buildConsultSeed is the first user message: the question, the named
// files (read through the confined registry, capped), the caller's recent
// context, and the git summary.
func (a *Agent) buildConsultSeed(ctx context.Context, readOnly *tools.Registry, req ConsultRequest) string {
	var b strings.Builder
	b.WriteString(req.Question)
	b.WriteString("\n")
	if len(req.Files) > 0 {
		b.WriteString("\n## Files\n")
		for _, f := range req.Files {
			r := readOnly.Dispatch(ctx, provider.ToolCall{ID: "seed", Name: "read_file", Arguments: fmt.Sprintf(`{"path":%q}`, f)})
			body := r.Content
			if len(body) > consultFileCap {
				body = body[:consultFileCap] + "\n… (truncated; read_file for the rest)"
			}
			fmt.Fprintf(&b, "\n### %s\n%s\n", f, body)
		}
	}
	if req.Recent != "" {
		recent := req.Recent
		if len(recent) > consultRecentCap {
			recent = recent[len(recent)-consultRecentCap:]
		}
		b.WriteString("\n## Recent context\n" + recent + "\n")
	}
	if g := gitctx.Summary(ctx, a.Tools.Root); g != "" {
		b.WriteString("\n## Git\n" + g + "\n")
	}
	return b.String()
}

// lastAssistantText is the newest assistant message content, for a
// consultation cut short after it had started answering.
func (a *Agent) lastAssistantText() string {
	for i := len(a.History.Messages) - 1; i >= 0; i-- {
		m := a.History.Messages[i]
		if m.Role == provider.RoleAssistant && strings.TrimSpace(m.Content) != "" {
			return strings.TrimSpace(m.Content)
		}
	}
	return ""
}
```

Check `profiles.Detect`'s return type and the `Profile` field's type in `Agent` (`Profile profiles.Profile`; the compat flag is `prof.Compat` or similar — read `internal/profiles` and `New` to use the same names). `sync.Mutex.TryLock` needs Go 1.18+ (fine). `History.Messages` is the field `Prompt()` builds from — confirm the name in `history.go`.

- [ ] **Step 4: Run the package**

Run: `go test -race ./internal/agent/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agent && git commit -m "agent: Consult — a read-only scratch agent on a co-working model, with consent and a per-run cap"
```

---

### Task 3: The `consult` tool

**Files:**
- Create: `internal/tools/consult.go`
- Modify: `internal/agent/extras.go:31` (`planAgent` subset gains `"consult"`)
- Test: `internal/tools/consult_test.go`, `internal/agent/cowork_test.go` (one test appended)

**Interfaces:**
- Produces:

```go
// internal/tools/consult.go
type ConsultArgs struct {
	Question string
	Who      string
	Files    []string
}
// ConsultFunc runs one consultation and returns the text for the model.
type ConsultFunc func(ctx context.Context, args ConsultArgs) (string, error)
type CoworkerInfo struct{ Name, Skills string }
func NewConsult(roster []CoworkerInfo, run ConsultFunc) Tool
```

- The tool's `Name()` is `consult`; `Description()` is the roster (`name — skills` lines under `Co-working models:`) followed by the sentence from the spec §2; `Schema()` has `question` (required string), `who` (string), `files` (array of string); `Run` reads `question` via `argString(args, "question", "q")`, `who` via `argString(args, "who", "name", "coworker")`, `files` from `args["files"]` as `[]any` of strings (also accept a single string), errors `consult needs a question` when empty, and returns `Result{Content: text}` or `Result{IsError: true, Content: err.Error()}`.
- The agent side supplies the `ConsultFunc` (Task 5 wires it in cmd): it calls `ag.Consult(ctx, ConsultRequest{Who, Question, Files, Origin: "tool", Recent: ag.RecentContext()})` and formats `co-worker <name> replied:\n\n<answer>` + the partial suffix. `Agent.RecentContext()` (add in this task, `cowork.go`): the current user request (`a.History`'s first user message of the run — track `a.lastUserInput` set in `run`), the newest assistant reply and the newest tool result whose `IsError` is set (walk `History.Messages` backwards), joined under headings `User request`, `Your last reply`, `Failing tool output`, whole thing capped at 6 KiB.

- [ ] **Step 1: Write the failing tests**

`internal/tools/consult_test.go`:

```go
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestConsultToolDescribesTheRosterAndDelegates(t *testing.T) {
	var got ConsultArgs
	tool := NewConsult([]CoworkerInfo{{"claude", "deep reasoning"}, {"big", "long reads"}},
		func(ctx context.Context, a ConsultArgs) (string, error) { got = a; return "co-worker claude replied:\n\nanswer", nil })
	if tool.Name() != "consult" {
		t.Fatal(tool.Name())
	}
	d := tool.Description()
	for _, want := range []string{"claude — deep reasoning", "big — long reads", "keep doing the work"} {
		if !strings.Contains(d, want) {
			t.Fatalf("description lacks %q:\n%s", want, d)
		}
	}
	var schema map[string]any
	if err := json.Unmarshal(tool.Schema(), &schema); err != nil {
		t.Fatal(err)
	}
	props := schema["properties"].(map[string]any)
	for _, k := range []string{"question", "who", "files"} {
		if _, ok := props[k]; !ok {
			t.Fatalf("schema lacks %s", k)
		}
	}
	res := tool.Run(context.Background(), map[string]any{"question": "why?", "who": "big", "files": []any{"a.go", "b.go"}})
	if res.IsError || res.Content != "co-worker claude replied:\n\nanswer" {
		t.Fatalf("result = %+v", res)
	}
	if got.Question != "why?" || got.Who != "big" || len(got.Files) != 2 || got.Files[1] != "b.go" {
		t.Fatalf("args = %+v", got)
	}
	if res := tool.Run(context.Background(), map[string]any{}); !res.IsError || res.Content != "consult needs a question" {
		t.Fatalf("empty question: %+v", res)
	}
	tool = NewConsult(nil, func(context.Context, ConsultArgs) (string, error) { return "", errors.New("consultation declined") })
	if res := tool.Run(context.Background(), map[string]any{"question": "q"}); !res.IsError || res.Content != "consultation declined" {
		t.Fatalf("error passthrough: %+v", res)
	}
}
```

Append to `internal/agent/cowork_test.go`:

```go
func TestPlanAgentOffersConsult(t *testing.T) {
	ag, _ := newTestAgent(t, &scriptedProvider{}, withCoworkers("big"))
	ag.Tools.AddTool(tools.NewConsult(nil, func(context.Context, tools.ConsultArgs) (string, error) { return "", nil }))
	names := ag.planAgent().Tools.Names()
	found := false
	for _, n := range names {
		if n == "consult" {
			found = true
		}
		if n == "write_file" || n == "shell" {
			t.Fatalf("plan agent offers %s", n)
		}
	}
	if !found {
		t.Fatalf("plan agent lacks consult: %v", names)
	}
}

func TestRecentContextCarriesRequestReplyAndFailingTool(t *testing.T) {
	p := &scriptedProvider{responses: []provider.ChatResponse{
		{ToolCalls: []provider.ToolCall{{ID: "1", Name: "read_file", Arguments: `{"path":"missing.go"}`}}},
		{Content: "I could not read it."},
	}}
	ag, _ := newTestAgent(t, p, nil)
	ag.Run(context.Background(), "please fix missing.go")
	rc := ag.RecentContext()
	for _, want := range []string{"User request", "please fix missing.go", "Your last reply", "I could not read it.", "Failing tool output", "missing.go"} {
		if !strings.Contains(rc, want) {
			t.Fatalf("recent context lacks %q:\n%s", want, rc)
		}
	}
}
```

(Add `"github.com/brown-enterprises/be-code/internal/tools"` to the test imports.)

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/tools/ -run TestConsultTool 2>&1 | head -3; go test ./internal/agent/ -run 'TestPlanAgentOffersConsult|TestRecentContext' 2>&1 | head -3`
Expected: build failures (`undefined: NewConsult`, `ag.RecentContext undefined`).

- [ ] **Step 3: Implement**

`internal/tools/consult.go`:

```go
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// The consult tool lets the primary model ask a configured co-working
// model for help. The tool itself knows nothing about providers: cmd hands
// it the roster to describe and a function that runs the consultation
// (agent.Consult behind it).

type ConsultArgs struct {
	Question string
	Who      string
	Files    []string
}

// ConsultFunc runs one consultation and returns the text handed back to
// the model, or an error the model is told about.
type ConsultFunc func(ctx context.Context, args ConsultArgs) (string, error)

// CoworkerInfo is what the tool description shows for one co-worker.
type CoworkerInfo struct{ Name, Skills string }

type consultTool struct {
	roster []CoworkerInfo
	run    ConsultFunc
}

// NewConsult builds the tool. Register it only when roster is non-empty.
func NewConsult(roster []CoworkerInfo, run ConsultFunc) Tool {
	return &consultTool{roster: roster, run: run}
}

func (t *consultTool) Name() string { return "consult" }

func (t *consultTool) Description() string {
	var b strings.Builder
	b.WriteString("Ask a co-working model for help. Co-working models:\n")
	for _, cw := range t.roster {
		fmt.Fprintf(&b, "- %s — %s\n", cw.Name, cw.Skills)
	}
	b.WriteString("Use when you have tried and failed, when a design question is beyond you, or when you need knowledge you do not have. Say what you tried. The co-worker can read the repository but cannot edit; you keep doing the work.")
	return b.String()
}

func (t *consultTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"question":{"type":"string","description":"What is wrong and what you tried"},
		"who":{"type":"string","description":"Co-worker name (default: the first one)"},
		"files":{"type":"array","items":{"type":"string"},"description":"Workspace paths the co-worker should look at first"}},
		"required":["question"]}`)
}

func (t *consultTool) Run(ctx context.Context, args map[string]any) Result {
	q := strings.TrimSpace(argString(args, "question", "q"))
	if q == "" {
		return Result{IsError: true, Content: "consult needs a question"}
	}
	a := ConsultArgs{Question: q, Who: strings.TrimSpace(argString(args, "who", "name", "coworker"))}
	switch v := args["files"].(type) {
	case []any:
		for _, f := range v {
			if s, ok := f.(string); ok && strings.TrimSpace(s) != "" {
				a.Files = append(a.Files, strings.TrimSpace(s))
			}
		}
	case string:
		if strings.TrimSpace(v) != "" {
			a.Files = append(a.Files, strings.TrimSpace(v))
		}
	}
	out, err := t.run(ctx, a)
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	return Result{Content: out}
}
```

`internal/agent/extras.go`: `readOnly := a.Tools.Subset("read_file", "list_dir", "search", "web_search", "web_fetch", "consult")` (Subset ignores names that are not registered, so plan mode is unchanged without co-workers).

`internal/agent/cowork.go`: add

```go
// RecentContext condenses what the primary was doing for a co-worker: the
// current request, the primary's newest reply, and the newest failing tool
// output. Capped at consultRecentCap.
func (a *Agent) RecentContext() string {
	var b strings.Builder
	if a.lastUserInput != "" {
		b.WriteString("User request:\n" + a.lastUserInput + "\n\n")
	}
	var reply, failing string
	for i := len(a.History.Messages) - 1; i >= 0 && (reply == "" || failing == ""); i-- {
		m := a.History.Messages[i]
		switch {
		case reply == "" && m.Role == provider.RoleAssistant && strings.TrimSpace(m.Content) != "":
			reply = strings.TrimSpace(m.Content)
		case failing == "" && m.Role == provider.RoleTool && strings.Contains(strings.ToLower(m.Content), "error"):
			failing = m.Content
		case failing == "" && m.Role == provider.RoleUser && strings.Contains(m.Content, `status="error"`):
			failing = m.Content
		}
	}
	if reply != "" {
		b.WriteString("Your last reply:\n" + reply + "\n\n")
	}
	if failing != "" {
		b.WriteString("Failing tool output:\n" + failing + "\n")
	}
	s := b.String()
	if len(s) > consultRecentCap {
		s = s[len(s)-consultRecentCap:]
	}
	return s
}
```

and in `loop.go`'s `run`, when `newTurn` is true, set `a.lastUserInput = userInput` (add the field). The native tool result for `read_file` on a missing file contains the error text (check what `readFileTool` returns for a missing path — `Content` holds the error message and `IsError` is true; the History message keeps only `Content`, so the substring `error`/the path is what the test relies on; if the content does not contain "error", match on `IsError` by recording the last failing tool result in `dispatch` into `a.lastFailingTool string` instead, and use that in `RecentContext`). Prefer the `dispatch` recording: it is exact. Then `failing` is simply `a.lastFailingTool` (`"<tool>: <content>"`), cleared at the start of each `run`.

- [ ] **Step 4: Run both packages**

Run: `go test -race ./internal/tools/ ./internal/agent/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tools/consult.go internal/tools/consult_test.go internal/agent && git commit -m "tools: consult tool; plan mode offers it; agent.RecentContext"
```

---

### Task 4: Harness triggers

**Files:**
- Modify: `internal/agent/loop.go` (`RunFull` verify trigger; `run`/`dispatch` repeated-failure trigger)
- Test: `internal/agent/cowork_test.go` (append)

**Interfaces:**
- Consumes: `Consult`, `RecentContext`, `Checkpoints.ChangedLast()`.
- Produces: no new exported names. Internal: `a.toolFailStreak struct{ name string; n int; last []string }` reset per `run`; helper `a.autoConsult(ctx, origin, question string, files []string) (string, string, bool)` returning `(coworkerName, answer, ok)` — runs `Consult` with `Recent: a.RecentContext()`, emits `OnTransient("consulting <name>…")` first, and returns `ok=false` on any error (after `a.notice("co-worker unavailable: %v", err)` when `err` is not the cap or a decline — those two are silent).

- Verify trigger (in `RunFull`, after the repair loop, before `return answer, rep, nil // don't review broken code`):

```go
			if rep.Verify != nil && !rep.Verify.Passed() && a.Cfg.Cowork.Auto && len(a.coworkers) > 0 && !a.autoVerifyUsed {
				a.autoVerifyUsed = true
				files := a.Checkpoints.ChangedLast() // nil-safe when Checkpoints is nil: guard
				if name, advice, ok := a.autoConsult(ctx, "auto:verify",
					"the change still fails this check; what is wrong and what minimal edit fixes it\n\n"+rep.Verify.ModelSummary(), files); ok {
					a.notice("co-worker %s advised; one more repair round", name)
					answer, err = a.run(ctx, fmt.Sprintf("A co-worker (%s) reviewed the failing check and advises:\n\n%s\n\nApply the minimal fix, then stop.", name, advice), false)
					if err != nil {
						return "", rep, err
					}
					rep.Verify = verify.RunChecks(ctx, a.Tools.Root, proj)
				}
			}
```

`a.autoVerifyUsed` resets at the top of `RunFull` with `a.consults`. `Checkpoints` may be nil in tests: use a small `a.changedFiles()` that returns nil when `a.Checkpoints == nil`.

- Tool trigger (in `dispatch`, after `res := a.Tools.Dispatch(...)`):

```go
	if res.IsError {
		if a.toolFailStreak.name == call.Name {
			a.toolFailStreak.n++
		} else {
			a.toolFailStreak = toolFailStreak{name: call.Name, n: 1}
		}
		a.toolFailStreak.last = append(a.toolFailStreak.last, res.Content)
		if len(a.toolFailStreak.last) > 3 {
			a.toolFailStreak.last = a.toolFailStreak.last[1:]
		}
		if a.toolFailStreak.n == 3 && a.Cfg.Cowork.Auto && len(a.coworkers) > 0 && call.Name != "consult" {
			q := fmt.Sprintf("the tool %s has failed three times in a row with these arguments:\n%s\n\nerrors:\n- %s", call.Name, call.Arguments, strings.Join(a.toolFailStreak.last, "\n- "))
			if name, advice, ok := a.autoConsult(ctx, "auto:tool", q, nil); ok {
				a.pendingAdvice = fmt.Sprintf("A co-worker (%s) looked at the repeated %s failure and advises:\n\n%s", name, call.Name, advice)
			}
		}
	} else {
		a.toolFailStreak = toolFailStreak{}
	}
```

and in `run`'s loop, right after `a.deliverInbox()`: `if a.pendingAdvice != "" { a.History.Add(provider.Message{Role: provider.RoleUser, Content: a.pendingAdvice}); a.pendingAdvice = "" }`. Reset the streak and `pendingAdvice` at the start of `run`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/agent/cowork_test.go`:

```go
func TestAutoToolTriggerAfterThreeConsecutiveFailures(t *testing.T) {
	cw := coworkerStub(t, provider.ChatResponse{Content: "The path is wrong: use src/main.go."})
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls <= 3 {
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: fmt.Sprint(calls), Name: "read_file", Arguments: `{"path":"nope.go"}`}}}, nil
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}}
	ag, _ := newTestAgent(t, p, withCoworkers("big"))
	var transient []string
	ag.Events.OnTransient = func(s string) { transient = append(transient, s) }
	if _, err := ag.Run(context.Background(), "read nope.go"); err != nil {
		t.Fatal(err)
	}
	if len(cw.lastReq.Messages) == 0 {
		t.Fatal("the co-worker was never consulted")
	}
	if !strings.Contains(cw.lastReq.Messages[1].Content, "failed three times") {
		t.Fatalf("auto:tool question:\n%s", cw.lastReq.Messages[1].Content)
	}
	// The advice reached the primary as a user note before its next call.
	last := p.reqs[len(p.reqs)-1].Messages
	found := false
	for _, m := range last {
		if m.Role == provider.RoleUser && strings.Contains(m.Content, "A co-worker (big) looked at the repeated read_file failure and advises:") && strings.Contains(m.Content, "src/main.go") {
			found = true
		}
	}
	if !found {
		t.Fatal("advice note not delivered to the primary")
	}
	if len(transient) == 0 || transient[0] != "consulting big…" {
		t.Fatalf("transient notices: %v", transient)
	}
	// Once per streak: three more failures of the same tool do not re-fire (the cap would allow it).
	if ag.ConsultsThisRun() != 1 {
		t.Fatalf("consults = %d", ag.ConsultsThisRun())
	}
}

func TestAutoToolTriggerRespectsAutoOffAndSuccessReset(t *testing.T) {
	cw := coworkerStub(t, provider.ChatResponse{Content: "advice"})
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		switch {
		case calls == 1 || calls == 2 || calls == 4 || calls == 5:
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: fmt.Sprint(calls), Name: "read_file", Arguments: `{"path":"nope.go"}`}}}, nil
		case calls == 3:
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "ok", Name: "list_dir", Arguments: `{}`}}}, nil
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}}
	ag, _ := newTestAgent(t, p, withCoworkers("big"))
	ag.Run(context.Background(), "x")
	if len(cw.lastReq.Messages) != 0 {
		t.Fatal("a success in between must reset the streak")
	}
	calls = 0
	p.fn = func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls <= 3 {
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: fmt.Sprint(calls), Name: "read_file", Arguments: `{"path":"nope.go"}`}}}, nil
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}
	ag.Cfg.Cowork.Auto = false
	ag.Run(context.Background(), "x")
	if len(cw.lastReq.Messages) != 0 {
		t.Fatal("auto off must not consult")
	}
}

func TestAutoVerifyTriggerGrantsOneExtraRepairRound(t *testing.T) {
	cw := coworkerStub(t, provider.ChatResponse{Content: "The test expects 3; return 3."})
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n\nfunc F() int { return 2 }\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "x_test.go"), []byte("package x\n\nimport \"testing\"\n\nfunc TestF(t *testing.T) { if F() != 3 { t.Fatal(F()) } }\n"), 0o644)
	reg, _ := tools.NewRegistry(dir, func(a, d string) bool { return true })
	cfg := config.Default()
	cfg.VerifyOnDone, cfg.MaxRepairs, cfg.CompatToolCalls, cfg.RepoMap = true, 1, "never", false
	withCoworkers("big")(cfg)
	calls := 0
	p := &funcProvider{fn: func(req provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		last := req.Messages[len(req.Messages)-1].Content
		if strings.Contains(last, "A co-worker (big) reviewed the failing check") {
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "fix", Name: "write_file", Arguments: `{"path":"x.go","content":"package x\n\nfunc F() int { return 3 }\n"}`}}}, nil
		}
		if calls == 1 {
			return &provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "w", Name: "write_file", Arguments: `{"path":"x.go","content":"package x\n\nfunc F() int { return 2 }\n"}`}}}, nil
		}
		return &provider.ChatResponse{Content: "done"}, nil
	}}
	ag := New(cfg, p, "test-model", reg, "")
	_, rep, err := ag.RunFull(context.Background(), "make F return the right value")
	if err != nil {
		t.Fatal(err)
	}
	if len(cw.lastReq.Messages) == 0 || !strings.Contains(cw.lastReq.Messages[1].Content, "still fails this check") {
		t.Fatal("verify trigger did not consult")
	}
	if rep.Verify == nil || !rep.Verify.Passed() {
		t.Fatalf("the extra repair round did not fix the check: %+v", rep.Verify)
	}
}
```

(This last test runs the real Go toolchain in a temp dir like the `verify` package tests; add `"fmt"` to the imports.)

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/agent/ -run 'TestAutoTool|TestAutoVerify' 2>&1 | grep -E "^(---|FAIL|ok)" | head`
Expected: FAIL (`the co-worker was never consulted`, `verify trigger did not consult`).

- [ ] **Step 3: Implement** as in Interfaces (`autoConsult`, the streak, `pendingAdvice`, the verify block, the resets).

- [ ] **Step 4: Run the package**

Run: `go test -race ./internal/agent/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agent && git commit -m "agent: consult a co-worker when verification is exhausted or a tool keeps failing"
```

---

### Task 5: cmd wiring and plain mode

**Files:**
- Modify: `cmd/root.go` (`CoworkerFactory`, warnings, tool registration), `internal/ui/common.go` (`/coworkers`, `/consult` rows; both busy-safe), `internal/ui/repl.go` (commands; event printing)
- Test: `internal/ui/repl_test.go` (append), `cmd/root_test.go` if a cmd test file exists (else skip)

**Interfaces:**
- `cmd/root.go`, next to `ReviewerFactory`:

```go
	agent.CoworkerFactory = func(c *config.Config, cw config.CoworkerConfig) (provider.Provider, error) {
		return provider.FromConfig(c, cw.Provider)
	}
```

and in `buildAgent`, after `ag := agent.New(...)`:

```go
	if cws, warns := cfg.ValidCoworkers(); len(cws) > 0 || len(warns) > 0 {
		for _, w := range warns {
			fmt.Fprintf(os.Stderr, "warn: %s\n", w)
		}
		if len(cws) > 0 {
			roster := make([]tools.CoworkerInfo, 0, len(cws))
			for _, cw := range cws {
				roster = append(roster, tools.CoworkerInfo{Name: cw.Name, Skills: cw.Skills})
			}
			reg.AddTool(tools.NewConsult(roster, func(ctx context.Context, a tools.ConsultArgs) (string, error) {
				res, err := ag.Consult(ctx, agent.ConsultRequest{Who: a.Who, Question: a.Question, Files: a.Files, Origin: "tool", Recent: ag.RecentContext()})
				if err != nil {
					return "", err
				}
				out := "co-worker " + res.Coworker + " replied:\n\n" + res.Answer
				if res.Partial {
					out += "\n\n(the co-worker was cut short; this is what it had)"
				}
				return out, nil
			}))
			ag.RefreshSystem() // the known-tool list and the prompt must see consult
		}
	}
```

(`RefreshSystem` exists — the IDE bridge uses it; if it must run after project notes, keep the order `agent.New` → add tool → `RefreshSystem`, before `attachIDE`.)

- `internal/ui/common.go`: rows `{"/coworkers", "list co-working models and how often each was consulted", false}` and `{"/consult", "ask a co-working model directly: /consult [name] <question>", true}` after `/review`; both in `busySafe`.
- `internal/ui/repl.go`:
  - `/coworkers`: for each `r.Agent.Coworkers()`: `  name  provider/model  skills` + ` (online)` + ` · consulted N` when `N > 0`; none → `no co-working models configured (see README "Co-working models")`.
  - `/consult`: `fields[1]` is a co-worker name if it matches one, else part of the question; empty question → `usage: /consult [name] <question>`; runs `r.runBusy(ctx, func(ctx) { res, err := r.Agent.Consult(ctx, agent.ConsultRequest{Who: who, Question: q, Origin: "user:local"}); ... })` printing the events (below) and, on error, `error> <err>`.
  - `Events()`: `OnConsultStart: func(name, q, origin string) { endThinking(); fmt.Printf("%s %s\n", cyan(name+"?"), q) }`; `OnConsultProgress`: nothing (status only in the TUI); `OnConsultEnd: func(res, err) { if err != nil { fmt.Printf("%s %s: %v\n", yell("note>"), res.Coworker, err); return }; fmt.Printf("%s %s\n", cyan(res.Coworker+">"), res.Answer); if res.Partial { fmt.Println(dim("(partial)")) }; fmt.Println(dim(fmt.Sprintf("%s read %d files in %s", res.Coworker, res.Read, res.Elapsed.Round(time.Second)))) }` — use an existing colour helper (`cyan` if present, else `yell`).

- [ ] **Step 1: Write the failing tests**

Append to `internal/ui/repl_test.go`:

```go
func TestPlainCoworkersAndConsult(t *testing.T) {
	r := newTestREPL(t)
	out := capture(t, func() { r.command(context.Background(), "/coworkers") })
	if !strings.Contains(out, "no co-working models configured") {
		t.Fatalf("empty list:\n%s", out)
	}
	r.Cfg.Coworkers = []config.CoworkerConfig{{Name: "big", Provider: "ollama", Model: "qwen3:32b", Skills: "long reads"}}
	r.Agent = agent.New(r.Cfg, r.Provider, "m", r.Agent.Tools, "")
	r.Agent.Events = Events()
	agent.CoworkerFactory = func(c *config.Config, cw config.CoworkerConfig) (provider.Provider, error) {
		return scriptedProvider(func(provider.ChatRequest) string { return "Try the other branch." }), nil
	}
	t.Cleanup(func() { agent.CoworkerFactory = nil })
	out = capture(t, func() { r.command(context.Background(), "/coworkers") })
	if !strings.Contains(out, "big") || !strings.Contains(out, "long reads") {
		t.Fatalf("list:\n%s", out)
	}
	out = capture(t, func() { r.command(context.Background(), "/consult big which branch?") })
	for _, want := range []string{"big? which branch?", "big> Try the other branch.", "read 0 files"} {
		if !strings.Contains(out, want) {
			t.Fatalf("consult output lacks %q:\n%s", want, out)
		}
	}
	if !strings.Contains(capture(t, func() { r.command(context.Background(), "/consult") }), "usage: /consult") {
		t.Fatal("no usage line")
	}
}
```

(`scriptedProvider` here is `internal/ui`'s helper from `initflow_test.go`; adjust names to what exists. `newTestREPL` builds an agent with a `nullProvider`; the co-worker replies through `CoworkerFactory`.)

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/ui/ -run TestPlainCoworkersAndConsult 2>&1 | grep -E "^(---|FAIL|ok|.*repl_test)" | head`
Expected: FAIL (`empty list` output missing).

- [ ] **Step 3: Implement** as in Interfaces.

- [ ] **Step 4: Verify**

Run: `go build ./... && go test ./internal/ui/ ./cmd/... && make -f build.mk verify 2>&1 | tail -2`
Expected: ok. Then a smoke: `go run . -C /tmp/some-go-project --plain` with a config that lists one local co-worker pointing at the LAN Ollama (`HOME=/tmp/bch`, add a `coworkers` entry to its config): `/coworkers` lists it, `/consult which files define the CLI?` prints `name? …`, then `name> …` with a real answer. Note the observation in the commit message.

- [ ] **Step 5: Commit**

```bash
git add cmd internal/ui && git commit -m "cmd/ui: wire co-workers — factory, consult tool, /coworkers and /consult in plain mode"
```

---

### Task 6: TUI rendering, consent and commands

**Files:**
- Modify: `internal/tui/theme.go` (`Palette.Cowork`, `styles.Cowork`), `internal/tui/entry.go` (`entryCoworkAsk`, `entryCowork`), `internal/tui/session.go` (event handlers, `/consult` turn), `internal/tui/view.go` (`/coworkers`, `/consult`, the consent modal title/hint, `a` key for action `consult`), `internal/tui/ask.go` if the approval modal needs a title for action `consult`
- Test: `internal/tui/cowork_test.go` (new)

**Interfaces:**
- `Palette.Cowork` per theme: dark `141`, light `91`, mono "" (bold via `st.Cowork = lipgloss.NewStyle().Bold(true)`), dracula `#8be9fd`… pick one visibly different hue from `User` and `Tool` per theme (list them in the diff); `styles.Cowork` built like the others.
- `entry.go`: `entryCoworkAsk` (Label = name, Text = question) → `st.Cowork.Render(e.Label+"? ") + e.Text`; `entryCowork` (Label = name, Text = Markdown answer) → `st.Cowork.Render(e.Label+"> ") + ui.RenderMarkdown(e.Text, richText)`.
- `session.go`: `onConsultStart(name, q, origin)` → lock, flush, append `entryCoworkAsk`, `statusNote = "consulting " + name`, broadcast status; `onConsultProgress(name, n)` → `setStatus(fmt.Sprintf("consulting %s · %d files read", name, n))`; `onConsultEnd(res, err)` → lock, flush; `err != nil` → `entryDim` `<name>: <err>` (a declined or capped consultation is dim, not an error); else append `entryCowork`, plus `entryDim` `(partial)` when partial, plus `entryDim` `<name> read N files in Ns`; `statusNote = "thinking"`; broadcast status. Wire the three in `NewSession`'s `agent.Events`.
- `/coworkers` (view-local note lines, `st.Cowork` for names) and `/consult [name] <question>`: a shared turn — `m.setRunStateLocked(true, "consulting")`, `ctx := m.runContextLocked()`, goroutine `sess.ag.Consult(ctx, ConsultRequest{Who, Question, Origin: "user:" + label})` then `sess.finishTurn(nil, nil)`; the events render the entries. Both busy-safe (the table already says so).
- Consent modal: `viewAsk` for `askApproval` with `Action == "consult"`: title `Co-working model — approval required`, hint `y allow this · n decline · a allow <name> for the session · ↑↓ scroll`; the `a` key for action `consult` parses the co-worker name from the detail's first line (`coworker: <name> (…)`) and calls `m.ag.AllowCoworker(name)` before answering `askAnswer{OK: true, Note: "co-worker <name> allowed for this session"}`. The verdict line is `consult approved`/`denied` via the existing `entryVerdict` path.

- [ ] **Step 1: Write the failing tests**

`internal/tui/cowork_test.go`:

```go
package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

func TestCoworkEntriesRenderInTheCoworkColourPerTheme(t *testing.T) {
	defer pinColorProfile()()
	dark, nord := stylesOr("dark"), stylesOr("nord")
	ask := entry{Kind: entryCoworkAsk, Label: "claude", Text: "why?"}
	ans := entry{Kind: entryCowork, Label: "claude", Text: "Because **x**."}
	if renderEntry(ask, dark, 80, false, true) == renderEntry(ask, nord, 80, false, true) {
		t.Fatal("cowork ask renders identically under two themes")
	}
	if got := renderEntry(ans, dark, 80, false, true); !strings.Contains(got, dark.Cowork.Render("claude> ")) || !strings.Contains(got, "Because") {
		t.Fatalf("cowork answer: %q", got)
	}
	if dark.Cowork.Render("x") == dark.User.Render("x") || dark.Cowork.Render("x") == dark.Tool.Render("x") {
		t.Fatal("cowork colour must differ from the user and tool colours")
	}
}

func TestConsultEventsBecomeSharedEntriesAndStatus(t *testing.T) {
	s, a, b := twoViews(t)
	s.ag.Events.OnConsultStart("claude", "why does it fail?", "tool")
	s.ag.Events.OnConsultProgress("claude", 2)
	s.ag.Events.OnConsultEnd(agent.ConsultResult{Coworker: "claude", Answer: "Line 12 is wrong.", Read: 2, Elapsed: 3 * time.Second}, nil)
	flush(a, b)
	for _, v := range []*View{a, b} {
		for _, want := range []string{"claude? why does it fail?", "claude> Line 12 is wrong.", "claude read 2 files in 3s"} {
			if !strings.Contains(v.wrapped, want) {
				t.Fatalf("view %d lacks %q:\n%s", v.id, want, v.wrapped)
			}
		}
	}
	s.ag.Events.OnConsultEnd(agent.ConsultResult{Coworker: "claude"}, errors.New("consultation declined"))
	flush(a, b)
	if !strings.Contains(a.wrapped, "claude: consultation declined") {
		t.Fatalf("declined note missing:\n%s", a.wrapped)
	}
}

func TestConsultConsentModalAndSessionAllow(t *testing.T) {
	s, a, b := twoViews(t)
	s.cfg.Coworkers = []config.CoworkerConfig{{Name: "claude", Provider: "ollama", Model: "opus", Online: true}}
	s.ag = agent.New(s.cfg, nullProvider{}, "m", s.ag.Tools, "")
	s.ag.Tools.Approve = s.approveFromAgent
	agent.CoworkerFactory = func(c *config.Config, cw config.CoworkerConfig) (provider.Provider, error) {
		return scriptedProvider(func(provider.ChatRequest) string { return "advice" }), nil
	}
	t.Cleanup(func() { agent.CoworkerFactory = nil })
	done := make(chan error, 1)
	go func() { _, err := s.ag.Consult(context.Background(), agent.ConsultRequest{Question: "q", Origin: "tool"}); done <- err }()
	waitFor(t, func() bool { flush(a, b); return a.mode == modeAsk && b.mode == modeAsk })
	if !strings.Contains(a.View(), "Co-working model") || !strings.Contains(a.View(), "coworker: claude") {
		t.Fatalf("consent modal:\n%s", a.View())
	}
	b.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	flush(a, b)
	if !strings.Contains(a.wrapped, "consult approved") || !strings.Contains(a.wrapped, "co-worker claude allowed for this session") {
		t.Fatalf("verdict lines:\n%s", a.wrapped)
	}
	// The second consultation must not prompt.
	if _, err := s.ag.Consult(context.Background(), agent.ConsultRequest{Question: "q2", Origin: "tool"}); err != nil {
		t.Fatal(err)
	}
	if a.mode == modeAsk || b.mode == modeAsk {
		t.Fatal("prompted again after a session allow")
	}
}

func TestConsultCommandRunsAsATurn(t *testing.T) {
	s, a, b := twoViews(t)
	s.cfg.Coworkers = []config.CoworkerConfig{{Name: "big", Provider: "ollama", Model: "qwen3:32b", Skills: "long reads"}}
	s.ag = agent.New(s.cfg, nullProvider{}, "m", s.ag.Tools, "")
	wireEvents(s) // the helper NewSession uses to install Events on an agent; extract it in this task
	agent.CoworkerFactory = func(c *config.Config, cw config.CoworkerConfig) (provider.Provider, error) {
		return scriptedProvider(func(provider.ChatRequest) string { return "Try the other branch." }), nil
	}
	t.Cleanup(func() { agent.CoworkerFactory = nil })
	a.slashCommand("/coworkers")
	if !strings.Contains(a.wrapped, "big") || !strings.Contains(a.wrapped, "long reads") {
		t.Fatalf("/coworkers:\n%s", a.wrapped)
	}
	a.slashCommand("/consult big which branch?")
	waitFor(t, func() bool { flush(a, b); return strings.Contains(b.wrapped, "big> Try the other branch.") })
	if !strings.Contains(b.wrapped, "big? which branch?") {
		t.Fatalf("question missing on the other terminal:\n%s", b.wrapped)
	}
	waitFor(t, func() bool { flush(a, b); return !a.running })
}
```

(`scriptedProvider`/`nullProvider` names: reuse what `internal/tui` tests already define, or add a tiny one in this file.)

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/tui/ -run 'TestCowork|TestConsult' 2>&1 | head -5`
Expected: build failure (`entryCoworkAsk` undefined).

- [ ] **Step 3: Implement** as in Interfaces; extract `wireEvents(s *Session)` from `NewSession` so tests can rewire a replaced agent.

- [ ] **Step 4: Run the package**

Run: `go test -race ./internal/tui/ -count=2`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/tui && git commit -m "tui: co-worker entries in a Cowork colour, consent modal with session allow, /coworkers and /consult"
```

---

### Task 7: E2E, docs, version

**Files:**
- Modify: `test/e2e/mock_server.py` (a second port `18112` scripted as the co-worker, or a path-keyed script on the same server), `test/e2e/run_e2e.sh` (one scenario: config with a co-worker on the mock, a task whose primary calls `consult`, assert the primary's final answer contains the co-worker's advice text and the run output shows `<name>>`)
- Modify: `README.md` ("Co-working models" section after "Themes": what, config example, consent, `/coworkers`, `/consult`, `cowork.*` in the config reference), `CHANGELOG.md` (`## v0.9.0 — co-working models`), `build.mk` (`VERSION := 0.9.0`), `docs/live-checklist.md` (item 15), root `CLAUDE.md` (a "Co-working models" subsection under Architecture: `Consult`, the scratch agent, `CoworkerFactory`, the two triggers, the consent seam, the events and entry kinds; version line 0.9.0)

- [ ] **Step 1: E2E scenario**

In `mock_server.py`, route by the request's `model`: the primary model script (existing) and a `coworker-model` script that answers `{"content": "Add the missing return at the end of add.go."}` on its first call. In `run_e2e.sh`, write a config whose `coworkers` has `{"name":"mock-cw","provider":"mock","model":"coworker-model"}` (provider `mock` pointing at `:18111`), script the primary to call `consult` once (`{"tool_calls":[{"name":"consult","arguments":"{\"question\":\"why does add.go not compile?\"}"}]}`) and then reply with the advice quoted; assert the run's stdout contains `mock-cw> Add the missing return` and `[PASS] consult`.

- [ ] **Step 2: Docs and version** as listed. CHANGELOG entry:

```markdown
## v0.9.0 — co-working models

- **Co-working models.** `coworkers` in config names other models (local or
  online) the primary can consult mid-task. The primary calls the new
  `consult` tool when it is stuck; with `cowork.auto` on, the harness also
  consults the first co-worker when verification is still failing after the
  last repair round (one extra round with the advice) or a tool has failed
  three times running (the advice is delivered as a note). A consultation is
  a read-only scratch agent on the co-worker's model — it reads the
  repository, never edits — capped by `cowork.consult_turns`, at most
  `cowork.max_consults_per_run` per request. Answers appear on every terminal
  as `<name>? question` and `<name>> answer` in the theme's co-worker colour.
  An `online: true` co-worker asks once per session before any code is sent
  (`a` allows it for the session); `-y` allows, headless without `-y`
  declines. `/coworkers` lists them; `/consult [name] <question>` asks one
  directly. An unreachable co-worker never interrupts the run.
```

- [ ] **Step 3: Verify and commit**

Run: `make -f build.mk verify 2>&1 | tail -2 && sh test/e2e/run_e2e.sh | tail -3 && grep VERSION build.mk`
Expected: ok; `E2E PASS`; `VERSION := 0.9.0`.

```bash
git add -A && git commit -m "docs/e2e: co-working models (0.9.0)"
```

---

## Self-review notes

- Spec §1 → Task 1 (config), Task 5 (`/coworkers`, `/consult`, warnings). §2 → Tasks 2, 3. §3 → Task 4. §4 → Task 2 (consent), Task 6 (modal, `a`). §5 → Task 6, Task 5 (plain). §6 → Tasks 2, 4 (errors never abort), Task 1 (validation). §7 → tests in each task, E2E in Task 7. §8 → Task 7. Future (deferred queue) → out of scope, seams noted in the spec.
- Names carried across tasks: `CoworkerConfig`/`CoworkConfig`/`ValidCoworkers` (T1) → T2, T5, T6; `ConsultRequest`/`ConsultResult`/`CoworkerFactory`/`Consult`/`AllowCoworker`/`Coworkers`/`ConsultsThisRun`/`ConsultCount`/`RecentContext`/`Events.OnConsult*` (T2, T3) → T4, T5, T6; `tools.NewConsult`/`ConsultArgs`/`ConsultFunc`/`CoworkerInfo` (T3) → T5; `entryCoworkAsk`/`entryCowork`/`styles.Cowork`/`wireEvents` (T6).
- Two places the implementer must confirm against the code before writing: the `profiles.Detect` return and the `Agent.Profile`/`compat` field names (T2), and how `dispatch` should record the last failing tool result for `RecentContext` (T3, prefer recording in `dispatch`).
