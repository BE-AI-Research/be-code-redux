# Sub-Agents on the Task Engine Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A co-worker marked `sub_agent: true` can own a subtree of the task tree — reading anywhere, writing inside its scope, running the project's checks — dispatched by a per-server scheduler, reporting back into the main model's queue, approved on the main model's own schema.

**Architecture:** `internal/subagent` is a leaf package (records, path scope helpers, the ready rule, per-server lanes) that `engine`, `tools` and `agent` may all import. `engine` gains owner/scope fields on nodes and one doing node per dispatched subtree. `tools` gains a scoped registry and `ask_main`. `agent/subagents.go` builds a scratch agent per dispatch (the `consultAgent` pattern), runs it on the session's root context, and hands back through `Enqueue`. The UIs add three `/task` verbs, `/agents`, transcript lines in the co-worker voice and a bottom-line indicator.

**Tech Stack:** Go 1.25, existing packages only (no new dependencies). `context.AfterFunc` (Go 1.21+) is used for cancellable condition waits.

**Spec:** `docs/superpowers/specs/2026-09-22-sub-agent-task-engine-design.md`

## Global Constraints

- Nothing changes for a configuration with no `sub_agent: true` co-worker: no new tool, no new prompt text, no scheduler goroutine.
- A sub-agent can never write outside its scope, run a command that is not exactly one of `verify.Detect`'s check commands, or reach `process`, `consult`, `web_*`, `ide_*` or MCP tools. Refusals are tool errors, never prompts.
- Sub-agent file writes go through the main registry's own `Approve`, `ApproveWrites`, `OnBeforeWrite`, `ReviewWrite` and `ReviewInvolvesEditor` — the same seam, the same mode.
- One lane per server: model calls to one `base_url` (scheme, host, port) are serialised; when both wait, the primary goes first.
- Verification stays the main model's: no sub-agent runs `RunFull`'s verify loop.
- The prompt-cache rule of 0.14.0 holds for every agent: each agent's own history stays append-only; no new per-turn text enters the primary's system prompt.
- `taskGuidance` and `pacingGuidance` stay verbatim; `subAgentGuidance` is a separate constant appended after them only when sub-agents are enabled.
- Every child process goes through `internal/procattr.Hide` (existing guard test); no new `exec.Command` outside `tools`.
- Tests must not touch `~/.be-code` (each package's `TestMain` redirects HOME; keep it that way).
- Version 1.1.0: `build.mk` VERSION, `CHANGELOG.md` entry, README status line.
- Commit trailers on every commit:
  `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`
  `Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9`
- Run `go test ./...` from `be-code/be-code` before every commit; `make -f build.mk verify` before the branch is called done.

## File map

| File | Responsibility |
|---|---|
| `internal/config/config.go` | `CoworkerConfig.SubAgent/MaxScope`, `SubAgentsConfig`, defaults, `max_scope` cleaning in `ValidCoworkers` |
| `internal/subagent/records.go` (new) | `Dispatch`, `HandBack`, `Ask`, `Card`, `Step`, `Candidate` |
| `internal/subagent/scope.go` (new) | `CleanScope`, `InScope`, `Within`, `Overlap`, `LaneKey` |
| `internal/subagent/ready.go` (new) | `Ready(steps, cards, running)` — the six rules |
| `internal/subagent/lanes.go` (new) | `Lanes.Acquire(ctx, server, primary)` with primary priority |
| `internal/engine/node.go` | node fields, `Tree.OwnerOf`, `Tree.DoingUnder`, dispatched set, `SetStatus` per pen |
| `internal/engine/markdown.go` | trailing fields parse/render, child-owner warning |
| `internal/engine/report.go` | `statusLine` owner/done-by suffix |
| `internal/engine/store.go` | `SetCards`, `SetOwner`, `SetScope`, `SetDispatched`, `CloseAs`, `Interrupt`, `Steps`, `ObserveFor`, `DispatchContext`, per-pen unfiled/adoption, load warnings |
| `internal/tools/tool.go`, `fs.go`, `shell.go` | `Registry.Scoped`, scope check on writes, exact-check shell |
| `internal/tools/askmain.go` (new) | `ask_main` tool |
| `internal/tools/task.go` | `owner`/`scope`/`reply` actions, `TaskLedger` additions, `NewTaskUnder` |
| `internal/agent/subagents.go` (new) | enable, schedule, dispatch, runner, ask/reply, stop, interrupt, resume, states, hand-back |
| `internal/agent/subprompt.go` (new) | `SubAgentFrame`, `subAgentGuidance`, dispatch rendering |
| `internal/agent/loop.go`, `resilience.go`, `enginefence.go`, `prompt.go` | lane hook, guidance, events, scheduler triggers, owned-path footer |
| `internal/ui/common.go`, `repl.go`, `task.go` | command rows, plain-mode handlers and event printing |
| `internal/tui/session.go`, `view.go`, `entry.go` | events, `/task` verbs, `/agents`, bottom line, quit |
| `cmd/root.go`, `cmd/commands.go` | wiring, `run --json` field |
| `README.md`, `docs/task-format.md`, `docs/live-checklist.md`, `CHANGELOG.md`, `CLAUDE.md` (root), `build.mk` | docs and version |
| `test/e2e/mock_server.py`, `run_e2e.sh` | `sub-agent` scenario |

---

### Task 1: Configuration

**Files:**
- Modify: `internal/config/config.go:28-35` (`CoworkerConfig`), `:104-278` (`Config`), `:304-376` (`Default`), `:464-487` (`Load` legacy rescue), `:496-529` (`ValidCoworkers`)
- Test: `internal/config/config_test.go`
- Modify: `README.md:820-824` (config reference bullet)

**Interfaces:**
- Produces: `CoworkerConfig.SubAgent bool`, `CoworkerConfig.MaxScope []string`; `SubAgentsConfig{MaxConcurrent, MaxTurns, AskTimeout int}`; `Config.SubAgents SubAgentsConfig`; `ValidCoworkers` cleans `MaxScope` (slash-separated, relative, no `..`) and warns on an entry that escapes.

- [ ] **Step 1: Write the failing tests**

Append to `internal/config/config_test.go`:

```go
func TestSubAgentsDefaults(t *testing.T) {
	cfg := Default()
	if cfg.SubAgents.MaxConcurrent != 2 || cfg.SubAgents.MaxTurns != 40 || cfg.SubAgents.AskTimeout != 600 {
		t.Fatalf("defaults: %+v", cfg.SubAgents)
	}
	cfg.SubAgents = SubAgentsConfig{}
	cfg.rescueZeroValues()
	if cfg.SubAgents.MaxConcurrent != 2 || cfg.SubAgents.MaxTurns != 40 || cfg.SubAgents.AskTimeout != 600 {
		t.Fatalf("zero values not rescued: %+v", cfg.SubAgents)
	}
	cfg.SubAgents.MaxConcurrent = -3
	cfg.rescueZeroValues()
	if cfg.SubAgents.MaxConcurrent != 1 {
		t.Fatalf("max_concurrent below 1 must be 1, got %d", cfg.SubAgents.MaxConcurrent)
	}
}

func TestValidCoworkersCleansMaxScope(t *testing.T) {
	cfg := Default()
	cfg.Coworkers = []CoworkerConfig{
		{Name: "big", Provider: "ollama", Model: "m", SubAgent: true,
			MaxScope: []string{"docs/", "./internal/scan", "../secrets", "/etc"}},
	}
	cws, warns := cfg.ValidCoworkers()
	if len(cws) != 1 {
		t.Fatalf("want one co-worker, got %d (%v)", len(cws), warns)
	}
	got := cws[0].MaxScope
	if len(got) != 2 || got[0] != "docs" || got[1] != "internal/scan" {
		t.Fatalf("max_scope cleaned wrong: %q", got)
	}
	if len(warns) != 2 {
		t.Fatalf("want two warnings for the escaping entries, got %q", warns)
	}
	for _, w := range warns {
		if !strings.Contains(w, "escapes the workspace") {
			t.Fatalf("warning text: %q", w)
		}
	}
}
```

Make sure `strings` is imported in the test file.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/config -run 'TestSubAgentsDefaults|TestValidCoworkersCleansMaxScope' -v`
Expected: compile failure — `SubAgents`, `SubAgentsConfig`, `rescueZeroValues`, `SubAgent`, `MaxScope` undefined.

- [ ] **Step 3: Add the fields, the block, the defaults**

In `internal/config/config.go`, extend `CoworkerConfig`:

```go
type CoworkerConfig struct {
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Skills   string `json:"skills,omitempty"`
	Online   bool   `json:"online,omitempty"`
	// SubAgent makes the co-worker assignable to a step of the task tree
	// (spec 2026-09-22 §1.4). MaxScope is the widest set of workspace paths
	// it may ever be given; empty means the whole workspace.
	SubAgent bool     `json:"sub_agent,omitempty"`
	MaxScope []string `json:"max_scope,omitempty"`
}

// SubAgentsConfig tunes sub-agent dispatch: how many run at once across
// every server, how many turns one gets, and how long an ask_main waits.
type SubAgentsConfig struct {
	MaxConcurrent int `json:"max_concurrent"`
	MaxTurns      int `json:"max_turns"`
	// AskTimeout is in seconds.
	AskTimeout int `json:"ask_timeout"`
}
```

Add to `Config` right after `Cowork`:

```go
	// SubAgents tunes sub-agents (co-workers with sub_agent: true).
	SubAgents SubAgentsConfig `json:"sub_agents"`
```

In `Default()` after the `Cowork:` line:

```go
		SubAgents:        SubAgentsConfig{MaxConcurrent: 2, MaxTurns: 40, AskTimeout: 600},
```

Extract the existing zero-value rescue in `Load` (the `if cfg.Cowork.MaxConsultsPerRun == 0 {…}` block and the `Engine` ones that follow it, lines 464-487) into a method and call it from `Load` where the block was:

```go
// rescueZeroValues fills fields an older config file leaves at zero with
// their defaults. Load calls it after decoding.
func (cfg *Config) rescueZeroValues() {
	// … the existing Cowork and Engine rescues, moved verbatim …
	if cfg.SubAgents.MaxTurns == 0 {
		cfg.SubAgents.MaxTurns = 40
	}
	if cfg.SubAgents.AskTimeout == 0 {
		cfg.SubAgents.AskTimeout = 600
	}
	if cfg.SubAgents.MaxConcurrent == 0 {
		cfg.SubAgents.MaxConcurrent = 2
	}
	if cfg.SubAgents.MaxConcurrent < 1 {
		// An assigned step must be able to run: the operator assigned it.
		cfg.SubAgents.MaxConcurrent = 1
	}
}
```

In `ValidCoworkers`, inside the `default:` case after the `Online` corroboration and before `seen[cw.Name] = true`:

```go
			if len(cw.MaxScope) > 0 {
				var kept []string
				for _, p := range cw.MaxScope {
					c := filepath.ToSlash(filepath.Clean(strings.TrimSpace(p)))
					if c == "." || c == "" || filepath.IsAbs(p) || c == ".." || strings.HasPrefix(c, "../") {
						warns = append(warns, fmt.Sprintf("coworker %q: max_scope %q escapes the workspace; ignored", cw.Name, p))
						continue
					}
					kept = append(kept, c)
				}
				cw.MaxScope = kept
			}
```

Add `path/filepath` and `strings` to the imports if absent.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/config -v`
Expected: PASS, including the pre-existing tests.

- [ ] **Step 5: Document the keys**

In `README.md` after the `coworkers`/`cowork.*` bullet (line ~824), add:

```markdown
- `coworkers[].sub_agent` (false) — the co-worker may own a step of the task tree
  (see "Sub-agents"); `coworkers[].max_scope` (`[]`, whole workspace) — the widest
  set of workspace paths it may ever be given as a scope
- `sub_agents.max_concurrent` (2) — sub-agents running at once across every server;
  `sub_agents.max_turns` (40) — turns one sub-agent gets on its step;
  `sub_agents.ask_timeout` (600, seconds) — how long an `ask_main` waits for an
  answer before the step is marked blocked
```

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go README.md
git commit -m "config: sub_agent, max_scope and the sub_agents block

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9"
```

---

### Task 2: `internal/subagent` — records, scope helpers, the ready rule

**Files:**
- Create: `internal/subagent/records.go`, `internal/subagent/scope.go`, `internal/subagent/ready.go`
- Test: `internal/subagent/scope_test.go`, `internal/subagent/ready_test.go`

**Interfaces:**
- Produces (all in package `subagent`, no imports beyond the standard library):
  - `type Card struct { Name, Provider, Server string; Online, SubAgent bool; MaxScope []string }`
  - `type Dispatch struct { Session, Node, Owner, Text string; Children []string; Scope, Checks []string; Context string; MaxTurns int; Interrupted bool; Touched []string }`
  - `type HandBack struct { Node, Owner, Status, Reason, Summary string; Files []string; Elapsed time.Duration; Calls int }`
  - `type Ask struct { Node, Owner, Question string }`
  - `type Step struct { ID, Text, Status, Owner string; Scope, After []string; Interrupted bool; Touched []string; Children []Step }`
  - `type Candidate struct { ID, Owner, Reason string }` — `Reason == ""` means ready
  - `func CleanScope(paths []string) ([]string, error)`; `func InScope(scope []string, rel string) bool`; `func Within(scope, max []string) bool`; `func Overlap(a, b []string) bool`; `func LaneKey(baseURL string) string`
  - `func Ready(roots []Step, cards map[string]Card, running map[string]bool) (ready, waiting []Candidate)`

- [ ] **Step 1: Write the failing scope tests**

`internal/subagent/scope_test.go`:

```go
package subagent

import "testing"

func TestCleanScope(t *testing.T) {
	got, err := CleanScope([]string{" internal/scan/ ", "./docs", "a b/c.go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != "internal/scan" || got[1] != "docs" || got[2] != "a b/c.go" {
		t.Fatalf("got %q", got)
	}
	for _, bad := range [][]string{{"../x"}, {"/etc"}, {"."}, {""}, {"a  b"}} {
		if _, err := CleanScope(bad); err == nil {
			t.Fatalf("%q should be refused", bad)
		}
	}
}

func TestInScopeWithinOverlap(t *testing.T) {
	scope := []string{"internal/scan", "README.md"}
	if !InScope(scope, "internal/scan/token.go") || !InScope(scope, "README.md") {
		t.Fatal("inside paths refused")
	}
	if InScope(scope, "internal/scanner/x.go") || InScope(scope, "README.md.bak") || InScope(scope, "cmd/x.go") {
		t.Fatal("outside paths accepted")
	}
	if !Within([]string{"docs/api"}, []string{"docs"}) || Within([]string{"docs"}, []string{"docs/api"}) {
		t.Fatal("Within wrong")
	}
	if !Within([]string{"anything"}, nil) {
		t.Fatal("empty max_scope means the whole workspace")
	}
	if !Overlap([]string{"internal"}, []string{"internal/scan"}) || Overlap([]string{"internal/a"}, []string{"internal/b"}) {
		t.Fatal("Overlap wrong")
	}
}

func TestLaneKey(t *testing.T) {
	if LaneKey("http://192.168.1.150:11434/v1") != "http://192.168.1.150:11434" {
		t.Fatal(LaneKey("http://192.168.1.150:11434/v1"))
	}
	if LaneKey("HTTP://Host:11434") != "http://host:11434" {
		t.Fatal("case folding")
	}
	if LaneKey("not a url") != "not a url" {
		t.Fatal("unparseable keeps the string")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/subagent/ -run 'TestCleanScope|TestInScopeWithinOverlap|TestLaneKey' -v`
Expected: build fails (package does not exist).

- [ ] **Step 3: Write records.go and scope.go**

`internal/subagent/records.go`:

```go
// Package subagent holds what the sub-agent design shares between the task
// engine, the tools and the agent: the records a dispatch and a hand-back
// are made of, the path-scope arithmetic, the ready rule and the per-server
// lanes. It imports nothing of the harness, so a later transport can carry
// its records to another host without dragging the agent along.
package subagent

import "time"

// Card is what the scheduler knows about one co-worker.
type Card struct {
	Name     string
	Provider string
	Server   string // LaneKey of the provider's base_url
	Online   bool
	SubAgent bool
	MaxScope []string
}

// Dispatch is one step handed to a sub-agent (spec §1.6).
type Dispatch struct {
	Session     string
	Node        string
	Owner       string
	Text        string
	Children    []string
	Scope       []string
	Checks      []string
	Context     string
	MaxTurns    int
	Interrupted bool
	Touched     []string
}

// HandBack is what comes back when the sub-agent's run ends.
type HandBack struct {
	Node    string
	Owner   string
	Status  string // "done" | "blocked"
	Reason  string
	Summary string
	Files   []string
	Elapsed time.Duration
	Calls   int
}

// Ask is one question from a sub-agent to the main model.
type Ask struct {
	Node, Owner, Question string
}

// Step is the ready rule's view of a tree node.
type Step struct {
	ID          string
	Text        string
	Status      string // todo | doing | done | blocked | dropped
	Owner       string // nearest owner up the tree; "" is the main model
	Scope       []string
	After       []string
	Interrupted bool
	Touched     []string
	Children    []Step
}

// Candidate is an assigned step and why it is not ready yet (Reason == ""
// means it is).
type Candidate struct {
	ID     string
	Owner  string
	Reason string
}
```

`internal/subagent/scope.go`:

```go
package subagent

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strings"
)

// CleanScope normalises workspace-relative paths: trimmed, slash-separated,
// cleaned, no absolute paths, nothing that climbs out, no run of two spaces
// (the task document separates trailing fields with two spaces).
func CleanScope(paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		s := strings.TrimSpace(p)
		if s == "" {
			return nil, errors.New("empty scope entry")
		}
		if strings.Contains(s, "  ") {
			return nil, fmt.Errorf("scope %q contains two consecutive spaces", s)
		}
		if filepath.IsAbs(s) || strings.HasPrefix(s, "/") {
			return nil, fmt.Errorf("scope %q is absolute; use a workspace-relative path", s)
		}
		c := path.Clean(filepath.ToSlash(s))
		if c == "." || c == ".." || strings.HasPrefix(c, "../") {
			return nil, fmt.Errorf("scope %q escapes the workspace", s)
		}
		out = append(out, c)
	}
	return out, nil
}

// InScope reports whether rel (slash-separated, workspace-relative) is one
// of the scope entries or under one of them.
func InScope(scope []string, rel string) bool {
	rel = path.Clean(filepath.ToSlash(rel))
	for _, s := range scope {
		if rel == s || strings.HasPrefix(rel, s+"/") {
			return true
		}
	}
	return false
}

// Within reports whether every entry of scope lies inside max. An empty
// max is the whole workspace.
func Within(scope, max []string) bool {
	if len(max) == 0 {
		return true
	}
	for _, s := range scope {
		if !InScope(max, s) {
			return false
		}
	}
	return true
}

// Overlap reports whether any entry of a contains or is contained by an
// entry of b.
func Overlap(a, b []string) bool {
	for _, x := range a {
		if InScope(b, x) {
			return true
		}
	}
	for _, y := range b {
		if InScope(a, y) {
			return true
		}
	}
	return false
}

// LaneKey is the server a base_url names: scheme, host and port, folded to
// lower case. Two provider entries on the same server share a lane.
func LaneKey(baseURL string) string {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" {
		return baseURL
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}
```

- [ ] **Step 4: Run the scope tests**

Run: `go test ./internal/subagent/ -run 'TestCleanScope|TestInScopeWithinOverlap|TestLaneKey' -v`
Expected: PASS.

- [ ] **Step 5: Write the failing ready-rule tests**

`internal/subagent/ready_test.go`:

```go
package subagent

import "testing"

func cards() map[string]Card {
	return map[string]Card{
		"big":    {Name: "big", SubAgent: true},
		"claude": {Name: "claude", SubAgent: true, Online: true, MaxScope: []string{"docs"}},
		"talk":   {Name: "talk", SubAgent: false},
	}
}

func tree() []Step {
	return []Step{{ID: "3", Text: "port", Status: "doing", Children: []Step{
		{ID: "3.1", Text: "list", Status: "done"},
		{ID: "3.2", Text: "port scan", Status: "todo", Owner: "big", Scope: []string{"internal/scan"},
			Children: []Step{{ID: "3.2.1", Status: "todo", Owner: "big", Scope: []string{"internal/scan"}}}},
		{ID: "3.3", Text: "note", Status: "todo", Owner: "claude", Scope: []string{"docs/scanner.md"}, After: []string{"3.1"}},
		{ID: "3.4", Text: "no scope", Status: "todo", Owner: "big"},
		{ID: "3.5", Text: "not a sub-agent", Status: "todo", Owner: "talk", Scope: []string{"x"}},
		{ID: "3.6", Text: "overlaps 3.2", Status: "todo", Owner: "big", Scope: []string{"internal"}},
		{ID: "3.7", Text: "outside max_scope", Status: "todo", Owner: "claude", Scope: []string{"cmd"}},
	}}}
}

func find(cs []Candidate, id string) *Candidate {
	for i := range cs {
		if cs[i].ID == id {
			return &cs[i]
		}
	}
	return nil
}

func TestReadyRules(t *testing.T) {
	ready, waiting := Ready(tree(), cards(), nil)
	if len(ready) != 2 || ready[0].ID != "3.2" || ready[1].ID != "3.3" {
		t.Fatalf("ready: %+v", ready)
	}
	want := map[string]string{
		"3.4": "no scope",
		"3.5": "talk is not a sub-agent",
		"3.6": "scope overlaps 3.2",
		"3.7": "scope is outside claude's max_scope (docs)",
	}
	for id, reason := range want {
		c := find(waiting, id)
		if c == nil || c.Reason != reason {
			t.Fatalf("%s: got %+v, want reason %q", id, c, reason)
		}
	}
	if find(waiting, "3.2.1") != nil || find(ready, "3.2.1") != nil {
		t.Fatal("a child of an assigned subtree is never a candidate")
	}
}

func TestReadyOrderAndRunning(t *testing.T) {
	roots := tree()
	// 3.3 positional rule: without after:, an earlier open sibling blocks it.
	roots[0].Children[2].After = nil
	ready, waiting := Ready(roots, cards(), nil)
	if find(ready, "3.3") != nil {
		t.Fatal("3.3 should wait for 3.2 without after:")
	}
	if c := find(waiting, "3.3"); c == nil || c.Reason != "waiting for 3.2" {
		t.Fatalf("3.3: %+v", c)
	}
	// A running root is not re-dispatched, and its scope still blocks 3.6.
	ready, waiting = Ready(tree(), cards(), map[string]bool{"3.2": true})
	if find(ready, "3.2") != nil {
		t.Fatal("running node offered again")
	}
	if c := find(waiting, "3.6"); c == nil || c.Reason != "scope overlaps 3.2" {
		t.Fatalf("3.6: %+v", c)
	}
}

func TestReadyIgnoresClosedAndMain(t *testing.T) {
	roots := []Step{{ID: "1", Status: "todo", Children: []Step{
		{ID: "1.1", Status: "done", Owner: "big", Scope: []string{"a"}},
		{ID: "1.2", Status: "todo", Scope: []string{"b"}},
		{ID: "1.3", Status: "todo", Owner: "nobody", Scope: []string{"c"}},
	}}}
	ready, waiting := Ready(roots, cards(), nil)
	if len(ready) != 0 {
		t.Fatalf("ready: %+v", ready)
	}
	if c := find(waiting, "1.3"); c == nil || c.Reason != "nobody is not in coworkers" {
		t.Fatalf("1.3: %+v", c)
	}
	if find(waiting, "1.1") != nil || find(waiting, "1.2") != nil {
		t.Fatal("closed or main-owned nodes are not candidates")
	}
}
```

- [ ] **Step 6: Run them to verify they fail**

Run: `go test ./internal/subagent/ -run TestReady -v`
Expected: `Ready` undefined.

- [ ] **Step 7: Write ready.go**

```go
package subagent

import (
	"fmt"
	"strings"
)

// Ready applies the six rules of spec §2.1 to a tree and returns, in id
// order, the assigned steps that may be dispatched now and the assigned
// steps that may not, each with the reason /agents shows. Only the top of
// an assigned subtree is a candidate; running names roots already
// dispatched, which are skipped but still hold their scope.
func Ready(roots []Step, cards map[string]Card, running map[string]bool) (ready, waiting []Candidate) {
	closed := map[string]bool{}
	var index func(s Step)
	index = func(s Step) {
		closed[s.ID] = s.Status == "done" || s.Status == "dropped"
		for _, c := range s.Children {
			index(c)
		}
	}
	for _, r := range roots {
		index(r)
	}
	// Scopes that are taken: every running root's, then every candidate
	// earlier in document order that is itself ready or running.
	taken := map[string][]string{}
	var walk func(s Step, siblings []Step, i int, inherited string)
	walk = func(s Step, siblings []Step, i int, inherited string) {
		if inherited != "" {
			return // inside an assigned subtree: the sub-agent's own steps
		}
		if s.Owner == "" {
			for j, c := range s.Children {
				walk(c, s.Children, j, "")
			}
			return
		}
		if closed[s.ID] || s.Status == "blocked" {
			return
		}
		if running[s.ID] {
			taken[s.ID] = s.Scope
			return
		}
		c := Candidate{ID: s.ID, Owner: s.Owner}
		card, known := cards[s.Owner]
		switch {
		case !known:
			c.Reason = s.Owner + " is not in coworkers"
		case !card.SubAgent:
			c.Reason = s.Owner + " is not a sub-agent"
		case len(s.Scope) == 0:
			c.Reason = "no scope"
		case !Within(s.Scope, card.MaxScope):
			c.Reason = fmt.Sprintf("scope is outside %s's max_scope (%s)", s.Owner, strings.Join(card.MaxScope, ", "))
		case s.Status != "todo":
			c.Reason = "status is " + s.Status
		}
		if c.Reason == "" {
			for id, sc := range taken {
				if Overlap(s.Scope, sc) {
					c.Reason = "scope overlaps " + id
					break
				}
			}
		}
		if c.Reason == "" {
			if len(s.After) > 0 {
				for _, dep := range s.After {
					if !closed[dep] {
						c.Reason = "waiting for " + dep
						break
					}
				}
			} else {
				for j := 0; j < i; j++ {
					if !closed[siblings[j].ID] {
						c.Reason = "waiting for " + siblings[j].ID
						break
					}
				}
			}
		}
		if c.Reason == "" {
			ready = append(ready, c)
			taken[s.ID] = s.Scope
		} else {
			waiting = append(waiting, c)
		}
	}
	for i, r := range roots {
		walk(r, roots, i, "")
	}
	return ready, waiting
}
```

Note `walk` is called with the child's own `Owner` decided by the caller: `Step.Owner` on a child inside an assigned subtree is the inherited owner (the engine fills it in `Steps()`), which is why `walk` returns early when the *parent* had an owner — pass `inherited = s.Owner` in the recursion for owned nodes. Adjust the recursion accordingly:

```go
		// (replace the two `walk(c, s.Children, j, "")` loops above with)
		for j, c := range s.Children {
			walk(c, s.Children, j, s.Owner)
		}
```

placed once, after the owned-node handling, so children of an owned node are skipped and children of a main-owned node are examined.

- [ ] **Step 8: Run all package tests**

Run: `go test ./internal/subagent/ -v`
Expected: PASS. If `TestReadyRules` reports 3.3 as waiting, the `taken` map is being consulted for 3.3 against 3.2's scope — `docs/scanner.md` does not overlap `internal/scan`, so check `Overlap`'s arguments.

- [ ] **Step 9: Commit**

```bash
git add internal/subagent
git commit -m "subagent: records, scope arithmetic and the ready rule

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9"
```

---

### Task 3: `internal/subagent` — lanes

**Files:**
- Create: `internal/subagent/lanes.go`
- Test: `internal/subagent/lanes_test.go`

**Interfaces:**
- Produces: `type Lanes struct`; `func NewLanes() *Lanes`; `func (l *Lanes) Acquire(ctx context.Context, server string, primary bool) (release func(), err error)`; `func (l *Lanes) Busy(server string) bool`.

- [ ] **Step 1: Write the failing tests**

`internal/subagent/lanes_test.go`:

```go
package subagent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLaneSerialisesOneServer(t *testing.T) {
	l := NewLanes()
	var inside, maxInside int32
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, err := l.Acquire(context.Background(), "http://a:1", false)
			if err != nil {
				t.Error(err)
				return
			}
			n := atomic.AddInt32(&inside, 1)
			for {
				m := atomic.LoadInt32(&maxInside)
				if n <= m || atomic.CompareAndSwapInt32(&maxInside, m, n) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt32(&inside, -1)
			rel()
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Fatalf("lane admitted %d at once", maxInside)
	}
}

func TestLanesRunTwoServersInParallel(t *testing.T) {
	l := NewLanes()
	relA, _ := l.Acquire(context.Background(), "http://a:1", false)
	defer relA()
	done := make(chan struct{})
	go func() {
		relB, err := l.Acquire(context.Background(), "http://b:1", false)
		if err == nil {
			relB()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a second server waited on the first's lane")
	}
}

func TestPrimaryGoesFirst(t *testing.T) {
	l := NewLanes()
	rel, _ := l.Acquire(context.Background(), "s", false)
	var order []string
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := func(name string, primary bool) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := l.Acquire(context.Background(), "s", primary)
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			r()
		}()
	}
	start("sub", false)
	time.Sleep(20 * time.Millisecond)
	start("primary", true)
	time.Sleep(20 * time.Millisecond)
	rel()
	wg.Wait()
	if len(order) != 2 || order[0] != "primary" {
		t.Fatalf("order %v", order)
	}
}

func TestAcquireHonoursCancel(t *testing.T) {
	l := NewLanes()
	rel, _ := l.Acquire(context.Background(), "s", false)
	defer rel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := l.Acquire(ctx, "s", false); err == nil {
		t.Fatal("expected a cancelled acquire to fail")
	}
	if !l.Busy("s") {
		t.Fatal("lane should still be held")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/subagent/ -run 'TestLane|TestPrimary|TestAcquire' -v`
Expected: `NewLanes` undefined.

- [ ] **Step 3: Write lanes.go**

```go
package subagent

import (
	"context"
	"sync"
)

// Lanes serialises model calls per server (spec §2.2). One lane is one
// LaneKey; the primary's own calls take its lane with priority, so a
// sub-agent sharing the primary's server never makes the person wait.
type Lanes struct {
	mu    sync.Mutex
	lanes map[string]*lane
}

type lane struct {
	mu             sync.Mutex
	cond           *sync.Cond
	held           bool
	primaryWaiting int
}

func NewLanes() *Lanes { return &Lanes{lanes: map[string]*lane{}} }

func (l *Lanes) laneFor(server string) *lane {
	l.mu.Lock()
	defer l.mu.Unlock()
	ln, ok := l.lanes[server]
	if !ok {
		ln = &lane{}
		ln.cond = sync.NewCond(&ln.mu)
		l.lanes[server] = ln
	}
	return ln
}

// Acquire blocks until the server's lane is free (and, for a sub-agent,
// until no primary call is waiting), or ctx ends. The returned release
// must be called exactly once.
func (l *Lanes) Acquire(ctx context.Context, server string, primary bool) (func(), error) {
	ln := l.laneFor(server)
	stop := context.AfterFunc(ctx, func() {
		ln.mu.Lock()
		ln.cond.Broadcast()
		ln.mu.Unlock()
	})
	defer stop()
	ln.mu.Lock()
	defer ln.mu.Unlock()
	if primary {
		ln.primaryWaiting++
		defer func() { ln.primaryWaiting-- }()
	}
	for ln.held || (!primary && ln.primaryWaiting > 0) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ln.cond.Wait()
	}
	ln.held = true
	var once sync.Once
	return func() {
		once.Do(func() {
			ln.mu.Lock()
			ln.held = false
			ln.cond.Broadcast()
			ln.mu.Unlock()
		})
	}, nil
}

// Busy reports whether the server's lane is currently held.
func (l *Lanes) Busy(server string) bool {
	ln := l.laneFor(server)
	ln.mu.Lock()
	defer ln.mu.Unlock()
	return ln.held
}
```

- [ ] **Step 4: Run the tests with the race detector**

Run: `go test ./internal/subagent/ -race -v`
Expected: PASS, no races.

- [ ] **Step 5: Commit**

```bash
git add internal/subagent/lanes.go internal/subagent/lanes_test.go
git commit -m "subagent: per-server lanes with primary priority

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9"
```

---

### Task 4: Engine — owner, scope and `after:` on a node; document round trip

**Files:**
- Modify: `internal/engine/node.go:26-41` (`Node`), `internal/engine/markdown.go:83-122` (`renderNode`), `:264-345` (`ParseDoc` node branch), `internal/engine/report.go:142-151` (`statusLine`)
- Test: `internal/engine/markdown_test.go`, `internal/engine/report_test.go`

**Interfaces:**
- Produces: `Node.Owner string`, `Node.OwnerPinned bool`, `Node.Scope []string`, `Node.After []string`, `Node.DoneBy string`; `Node.Dispatched bool` (`json:"-"`, transient, set by the store); `func (t Tree) OwnerOf(id string) string` (nearest ancestor-or-self owner); `splitFields(text string) (string, nodeFields)`; `renderFields(n *Node) string`; `statusLine` suffixes.

- [ ] **Step 1: Write the failing round-trip and statusLine tests**

Append to `internal/engine/markdown_test.go`:

```go
const ownedDoc = `# 003 — port the scanner

- [>] 3. port the scanner to the new tokenizer
  - [x] 3.1. list the call sites
  - [ ] 3.2. port internal/scan  @big  scope: internal/scan, internal/scan_test.go
    - [ ] 3.2.1. replace the token loop
    - [ ] 3.2.2. update the tests
  - [ ] 3.3. write the migration note  @claude!  scope: docs/scanner.md  after: 3.2
  - [ ] 3.4. email  @bob about the release
`

func TestOwnedRoundTrip(t *testing.T) {
	tr, _, err := ParseDoc(ownedDoc)
	if err != nil {
		t.Fatal(err)
	}
	r := tr.Roots[0]
	n32 := r.Children[1]
	if n32.Owner != "big" || n32.OwnerPinned || len(n32.Scope) != 2 || n32.Scope[1] != "internal/scan_test.go" {
		t.Fatalf("3.2 parsed as %+v", n32)
	}
	n33 := r.Children[2]
	if n33.Owner != "claude" || !n33.OwnerPinned || n33.Scope[0] != "docs/scanner.md" || len(n33.After) != 1 || n33.After[0] != "3.2" {
		t.Fatalf("3.3 parsed as %+v", n33)
	}
	if n34 := r.Children[3]; n34.Owner != "" || n34.Text != "email  @bob about the release" {
		t.Fatalf("an @ in the middle of the text is text: %+v", n34)
	}
	if tr.OwnerOf("3.2.1") != "big" || tr.OwnerOf("3.1") != "" {
		t.Fatal("OwnerOf must walk up to the nearest owner")
	}
	out := RenderDoc("003", "port the scanner", r)
	if out != ownedDoc {
		t.Fatalf("round trip differs:\n--- got ---\n%s\n--- want ---\n%s", out, ownedDoc)
	}
}

func TestChildOwnerIsIgnoredWithANote(t *testing.T) {
	doc := "# 001 — t\n\n- [ ] 1. t\n  - [ ] 1.1. a  @big  scope: x\n    - [ ] 1.1.1. b  @claude\n"
	tr, _, err := ParseDoc(doc)
	if err != nil {
		t.Fatal(err)
	}
	c := tr.Roots[0].Children[0].Children[0]
	if c.Owner != "" {
		t.Fatalf("child owner kept: %q", c.Owner)
	}
	if len(c.Evidence.Notes) != 1 || c.Evidence.Notes[0].Text != "owner @claude ignored: 1.1 is owned by big" {
		t.Fatalf("note: %+v", c.Evidence.Notes)
	}
}
```

Append to `internal/engine/report_test.go`:

```go
func TestStatusLineOwnerSuffixes(t *testing.T) {
	n := &Node{ID: "3.2", Text: "port", Status: StatusTodo, Owner: "big", Scope: []string{"internal/scan"}}
	if got := statusLine(n); got != "3.2. port — todo @big" {
		t.Fatalf("open assigned: %q", got)
	}
	n.OwnerPinned = true
	if got := statusLine(n); got != "3.2. port — todo @big!" {
		t.Fatalf("pinned: %q", got)
	}
	n.Dispatched, n.DispatchedAt, n.Calls = true, "3.2.1", 9
	if got := statusLine(n); got != "3.2. port — todo @big running (at 3.2.1, 9 tool calls)" {
		t.Fatalf("running: %q", got)
	}
	start := time.Now().Add(-14 * time.Minute)
	d := &Node{ID: "3.2", Text: "port", Status: StatusDone, DoneBy: "big", Started: start, Closed: time.Now(), Calls: 22}
	if got := statusLine(d); got != "3.2. port — done by big (14m, 22 tool calls)" {
		t.Fatalf("done by: %q", got)
	}
	b := &Node{ID: "3.2", Text: "port", Status: StatusBlocked, DoneBy: "big", Reason: "turn cap of 40 reached"}
	if got := statusLine(b); got != "3.2. port — blocked by big: turn cap of 40 reached" {
		t.Fatalf("blocked by: %q", got)
	}
}
```

(`time` must be imported in report_test.go.)

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/engine -run 'TestOwnedRoundTrip|TestChildOwnerIsIgnored|TestStatusLineOwnerSuffixes' -v`
Expected: compile errors on the new fields.

- [ ] **Step 3: Add the node fields and OwnerOf**

In `internal/engine/node.go`, extend `Node` after `Calls`:

```go
	// Owner is the co-worker that owns this step ("" is the main model);
	// OwnerPinned means the operator assigned it ("@name!" in the document)
	// and the model may not change it. Scope is what the owner may write;
	// After overrides the positional ready rule. DoneBy is stamped when a
	// sub-agent closes the node. Dispatched and DispatchedAt are transient
	// render state the store sets on its copy of the tree (spec §1.5).
	Owner        string   `json:"owner,omitempty"`
	OwnerPinned  bool     `json:"owner_pinned,omitempty"`
	Scope        []string `json:"scope,omitempty"`
	After        []string `json:"after,omitempty"`
	DoneBy       string   `json:"done_by,omitempty"`
	Dispatched   bool     `json:"-"`
	DispatchedAt string   `json:"-"`
```

Add after `Find`:

```go
// OwnerOf is the owner of the nearest node up the tree, id itself
// included, that has one; "" is the main model. Children of an assigned
// node carry no tag of their own (spec §1.1).
func (t Tree) OwnerOf(id string) string {
	parts := strings.Split(id, ".")
	for i := len(parts); i > 0; i-- {
		if n := t.Find(strings.Join(parts[:i], ".")); n != nil && n.Owner != "" {
			return n.Owner
		}
	}
	return ""
}
```

- [ ] **Step 4: Render and parse the trailing fields**

In `internal/engine/markdown.go`, add beside `nodeLine`:

```go
// trailingField matches one trailing field of a node line — "  @owner",
// "  @owner!", "  scope: a, b" or "  after: 3.1, 3.2" — separated from the
// text (and from each other) by two or more spaces. A value may contain
// single spaces but never two in a row, which is what lets one field end
// where the next begins. ParseDoc peels fields off the end until none
// match, so their order does not matter on read; renderFields writes them
// owner, scope, after.
var trailingField = regexp.MustCompile(`^(.*?)\s{2,}(?:@([A-Za-z0-9_.-]+)(!?)|scope:\s*((?:\S| \S)+?)|after:\s*((?:\S| \S)+?))\s*$`)

type nodeFields struct {
	owner  string
	pinned bool
	scope  []string
	after  []string
}

// splitFields separates a node's trailing fields from its text.
func splitFields(text string) (string, nodeFields) {
	var f nodeFields
	for {
		m := trailingField.FindStringSubmatch(text)
		if m == nil {
			return text, f
		}
		text = m[1]
		switch {
		case m[2] != "":
			f.owner, f.pinned = m[2], m[3] == "!"
		case m[4] != "":
			f.scope = splitList(m[4])
		case m[5] != "":
			f.after = splitList(m[5])
		}
	}
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// renderFields is the trailing fields of a node line, or "".
func renderFields(n *Node) string {
	var b strings.Builder
	if n.Owner != "" {
		b.WriteString("  @" + n.Owner)
		if n.OwnerPinned {
			b.WriteByte('!')
		}
	}
	if len(n.Scope) > 0 {
		b.WriteString("  scope: " + strings.Join(n.Scope, ", "))
	}
	if len(n.After) > 0 {
		b.WriteString("  after: " + strings.Join(n.After, ", "))
	}
	return b.String()
}
```

In `renderNode`, change the first `Fprintf` so the fields follow the text and precede the reason:

```go
	fmt.Fprintf(b, "%s- [%s] %s. %s%s", ind, mark, n.ID, n.Text, renderFields(n))
```

In `ParseDoc`'s node branch, replace `text, reason := splitReason(strings.TrimSpace(m[4]))` with:

```go
			text, reason := splitReason(strings.TrimSpace(m[4]))
			text, fields := splitFields(text)
```

and after `n := &Node{…}`:

```go
			n.Owner, n.OwnerPinned, n.Scope, n.After = fields.owner, fields.pinned, fields.scope, fields.after
```

Then, after the child is appended to its parent (`p.Children = append(p.Children, n)` and `want = …`), enforce one pen per subtree:

```go
				if inherited := ownerAbove(stack[:depth]); inherited != "" && n.Owner != "" {
					n.Evidence.Notes = append(n.Evidence.Notes,
						NoteRef{Text: fmt.Sprintf("owner @%s ignored: %s is owned by %s", n.Owner, ownerNodeAbove(stack[:depth]).ID, inherited)})
					n.Owner, n.OwnerPinned = "", false
				}
```

with two helpers beside `splitFields`:

```go
// ownerNodeAbove is the nearest ancestor on the parse stack with an owner.
func ownerNodeAbove(stack []*Node) *Node {
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i].Owner != "" {
			return stack[i]
		}
	}
	return nil
}

func ownerAbove(stack []*Node) string {
	if n := ownerNodeAbove(stack); n != nil {
		return n.Owner
	}
	return ""
}
```

Check where `splitReason` runs relative to the fields: the reason suffix ` — blocked: …` is rendered *after* the fields (`renderNode` writes fields then reason), so `splitReason` must run first, then `splitFields` — the order above.

- [ ] **Step 5: The statusLine suffixes**

Replace `statusLine` in `internal/engine/report.go`:

```go
// statusLine is "<id>. <text> — <status>", with " by <owner>" when a
// sub-agent closed the node, ": <reason>" for a blocked or dropped node
// that gave one, " @owner" (and "!" when the operator pinned it) on an open
// assigned node, and " running (at <id>, N tool calls)" while it is
// dispatched — the same phrasing the task document itself uses, so a
// report never disagrees with the file it came from.
func statusLine(n *Node) string {
	s := fmt.Sprintf("%s. %s — %s", n.ID, n.Text, n.Status)
	if n.DoneBy != "" {
		s += " by " + n.DoneBy
	}
	if n.Reason != "" && (n.Status == StatusBlocked || n.Status == StatusDropped) {
		s += ": " + n.Reason
	}
	if n.Owner != "" && !n.Status.terminal() {
		s += " @" + n.Owner
		if n.OwnerPinned {
			s += "!"
		}
		if n.Dispatched {
			calls := "tool calls"
			if n.Calls == 1 {
				calls = "tool call"
			}
			s += fmt.Sprintf(" running (at %s, %d %s)", n.DispatchedAt, n.Calls, calls)
		}
	}
	return s + spent(n)
}
```

- [ ] **Step 6: Run the engine tests**

Run: `go test ./internal/engine -v`
Expected: PASS. The pre-existing `TestRoundTrip` must still pass byte for byte (no fields → no suffix).

- [ ] **Step 7: Document the fields**

In `docs/task-format.md`, under `## The document` after the "Ids are dotted paths" paragraph, add:

```markdown
**Owner, scope and order** are optional trailing fields, each separated from the
text and from one another by two or more spaces:

    - [ ] 3.2. port internal/scan  @big  scope: internal/scan, internal/scan_test.go
    - [ ] 3.3. write the migration note  @claude!  scope: docs/scanner.md  after: 3.2

`@name` assigns the step to a co-worker with `sub_agent: true` in the config; the
main model is the owner when there is no tag, and is never written. `@name!` was
assigned by the operator, and the model will not change it. `scope:` is the
comma-separated list of workspace paths the sub-agent may write under; a step with
an owner and no scope is not ready. `after:` names the steps that must be closed
first, replacing the default rule that earlier siblings close first. Children of an
assigned step belong to the same owner and carry no tag; one written there is
ignored with a note. A closed step a sub-agent did reads `done by big (14m, 22 tool
calls)` in its report.
```

- [ ] **Step 8: Commit**

```bash
git add internal/engine/node.go internal/engine/markdown.go internal/engine/report.go internal/engine/markdown_test.go internal/engine/report_test.go docs/task-format.md
git commit -m "engine: owner, scope and after on a node; document round trip

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9"
```

---

### Task 5: Engine — one doing node per dispatched subtree, and the store API

**Files:**
- Modify: `internal/engine/node.go:46-51` (`Tree`), `:150` (`Doing`), `:163-187` (`SetStatus`), `internal/engine/store.go` (`Store` fields, `SetStatus` 1291, `adoptUnfiledLocked` 1493, `activeNodeLocked` 1459, `closeDoingLocked` 1612, `warnMultipleDoing` 547, `Observe` 1773, `Render` in render.go:64, `loadDocs` 421)
- Test: `internal/engine/subagent_test.go` (new)

**Interfaces:**
- Consumes: `subagent.Card`, `subagent.Step`, `subagent.CleanScope`, `subagent.Within`, `subagent.Overlap`.
- Produces on `*Tree`: `SetDispatched(rootID string, on bool)`, `Dispatched() []string`, `penOf(id string) string`, `DoingUnder(rootID string) *Node`; `Doing()` now returns the main model's doing node (one not inside a dispatched subtree).
- Produces on `*Store`: `SetCards(cards map[string]subagent.Card)`; `SetOwner(id, owner string, pinned bool) error`; `SetScope(id string, scope []string) error`; `SetDispatched(id string, on bool)`; `Dispatched() []string`; `CloseAs(id, owner, status, reason string) error`; `Interrupt(id, note string) error`; `Steps() []subagent.Step`; `ObserveFor(rootID string, ev Event) string`; `DispatchContext(id string) (text string, children []string, ctx string)`; `Touched(id string) []string`.

- [ ] **Step 1: Write the failing tests**

`internal/engine/subagent_test.go`:

```go
package engine

import (
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/subagent"
)

func cardsForTest() map[string]subagent.Card {
	return map[string]subagent.Card{
		"big":    {Name: "big", SubAgent: true},
		"claude": {Name: "claude", SubAgent: true, MaxScope: []string{"docs"}},
	}
}

func planOwned(t *testing.T) (*Store, string) {
	t.Helper()
	s := testStore(t)
	s.SetCards(cardsForTest())
	root := s.Plan("port the scanner", []string{"list the call sites", "port internal/scan", "write the note"})
	if err := s.SetStatus(root+".1", StatusDoing, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOwner(root+".2", "big", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetScope(root+".2", []string{"internal/scan"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Add(root+".2", "replace the token loop"); err != nil {
		t.Fatal(err)
	}
	return s, root
}

func TestOwnerAndScopeRules(t *testing.T) {
	s, root := planOwned(t)
	if err := s.SetOwner(root+".3", "claude", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOwner(root+".3", "big", false); err == nil || !strings.Contains(err.Error(), "assigned by the operator") {
		t.Fatalf("pinned owner changed by the model: %v", err)
	}
	if err := s.SetOwner(root+".3", "big", true); err != nil {
		t.Fatalf("the operator may re-pin: %v", err)
	}
	if err := s.SetOwner(root+".3", "claude", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetScope(root+".3", []string{"cmd"}); err == nil || !strings.Contains(err.Error(), "may only own paths under docs") {
		t.Fatalf("max_scope not enforced: %v", err)
	}
	if err := s.SetScope(root+".3", []string{"docs/x.md"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetOwner(root+".2.1", "claude", false); err == nil {
		t.Fatal("a child of an assigned subtree cannot take its own owner")
	}
	if err := s.SetOwner(root+".1", "nobody", false); err == nil || !strings.Contains(err.Error(), "not a sub-agent") {
		t.Fatalf("unknown owner accepted: %v", err)
	}
	if s.tree.Find(root+".2").Owner != "big" || s.tree.OwnerOf(root+".2.1") != "big" {
		t.Fatal("owner lost")
	}
}

func TestOneDoingPerPen(t *testing.T) {
	s, root := planOwned(t)
	s.SetDispatched(root+".2", true)
	if err := s.SetStatus(root+".2.1", StatusDoing, ""); err != nil {
		t.Fatal(err)
	}
	if d := s.tree.Doing(); d == nil || d.ID != root+".1" {
		t.Fatalf("the main model's doing node was disturbed: %+v", d)
	}
	if d := s.tree.DoingUnder(root + ".2"); d == nil || d.ID != root+".2.1" {
		t.Fatalf("sub-agent's doing node: %+v", d)
	}
	if _, err := s.Add(root+".2", "update the tests"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus(root+".2.2", StatusDoing, ""); err != nil {
		t.Fatal(err)
	}
	if s.tree.Find(root+".2.1").Status != StatusTodo || s.tree.DoingUnder(root+".2").ID != root+".2.2" {
		t.Fatal("a new doing node under the pen must send the previous one back to todo")
	}
	if s.tree.Find(root+".1").Status != StatusDoing {
		t.Fatal("the main model's doing node changed")
	}
	// Evidence filed for the pen lands on its own doing node, and the main
	// model's evidence on its own.
	s.ObserveFor(root+".2", Event{Tool: "shell", Args: map[string]any{"command": "go test ./..."}, Content: "ok"})
	if n := s.tree.Find(root + ".2.2"); len(n.Evidence.Raw) != 1 || n.Calls != 1 {
		t.Fatalf("sub-agent evidence: %+v", n.Evidence)
	}
	s.Observe(Event{Tool: "shell", Args: map[string]any{"command": "ls"}, Content: "ok"})
	if n := s.tree.Find(root + ".1"); len(n.Evidence.Raw) != 1 {
		t.Fatalf("main evidence: %+v", n.Evidence)
	}
}

func TestUnfiledPerPen(t *testing.T) {
	s, root := planOwned(t)
	s.SetDispatched(root+".2", true)
	s.ObserveFor(root+".2", Event{Tool: "read_file", Args: map[string]any{"path": "x.go"}, Content: "..."})
	sub := s.tree.Find(root + ".2")
	var unfiled *Node
	for _, c := range sub.Children {
		if c.Text == unfiledText {
			unfiled = c
		}
	}
	if unfiled == nil || unfiled.Status != StatusDoing {
		t.Fatalf("unfiled node under the pen missing: %+v", sub.Children)
	}
	if err := s.SetStatus(root+".2.1", StatusDoing, ""); err != nil {
		t.Fatal(err)
	}
	if n := s.tree.Find(root + ".2.1"); n.Calls != 1 {
		t.Fatalf("unfiled evidence not adopted by the pen's step: %+v", n)
	}
	if s.tree.Find(root+".1").Calls != 0 {
		t.Fatal("the main model's step adopted the sub-agent's unfiled evidence")
	}
}

func TestCloseAsAndInterrupt(t *testing.T) {
	s, root := planOwned(t)
	s.SetDispatched(root+".2", true)
	_ = s.SetStatus(root+".2.1", StatusDoing, "")
	s.ObserveFor(root+".2", Event{Tool: "write_file", Args: map[string]any{"path": "internal/scan/a.go", "content": "x"}, Content: "wrote"})
	if got := s.Touched(root + ".2"); len(got) != 1 || got[0] != "internal/scan/a.go" {
		t.Fatalf("Touched: %q", got)
	}
	if err := s.Interrupt(root+".2", "interrupted 10:00 after 1 tool calls; files written: internal/scan/a.go"); err != nil {
		t.Fatal(err)
	}
	if s.tree.Find(root+".2.1").Status != StatusTodo || s.tree.DoingUnder(root+".2") != nil {
		t.Fatal("Interrupt must send the pen's doing node back to todo")
	}
	steps := s.Steps()
	sub := steps[0].Children[1]
	if !sub.Interrupted || len(sub.Touched) != 1 || sub.Touched[0] != "internal/scan/a.go" {
		t.Fatalf("Steps must surface the interruption: %+v", sub)
	}
	if len(s.Dispatched()) != 0 {
		t.Fatal("Interrupt must clear the dispatched mark")
	}
	s.SetDispatched(root+".2", true)
	_ = s.SetStatus(root+".2.1", StatusDoing, "")
	if err := s.CloseAs(root+".2", "big", "done", ""); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{root + ".2", root + ".2.1"} {
		n := s.tree.Find(id)
		if n.Status != StatusDone || n.DoneBy != "big" {
			t.Fatalf("%s after CloseAs: %+v", id, n)
		}
	}
	if err := s.CloseAs(root+".3", "big", "blocked", "turn cap of 40 reached"); err != nil {
		t.Fatal(err)
	}
	if n := s.tree.Find(root + ".3"); n.Status != StatusBlocked || n.Reason != "turn cap of 40 reached" {
		t.Fatalf("blocked: %+v", n)
	}
}

func TestStepsAndDispatchContext(t *testing.T) {
	s, root := planOwned(t)
	steps := s.Steps()
	if len(steps) != 1 || steps[0].ID != root || steps[0].Children[1].Owner != "big" || steps[0].Children[1].Children[0].Owner != "big" {
		t.Fatalf("Steps: %+v", steps)
	}
	text, children, ctx := s.DispatchContext(root + ".2")
	if text != "port internal/scan" || len(children) != 1 || !strings.HasPrefix(children[0], root+".2.1 ") {
		t.Fatalf("DispatchContext: %q %q", text, children)
	}
	if !strings.Contains(ctx, "port the scanner") {
		t.Fatalf("context lacks the parent task: %q", ctx)
	}
}

func TestRenderShowsADispatchedSubtreeAsOneLine(t *testing.T) {
	s, root := planOwned(t)
	s.SetDispatched(root+".2", true)
	_ = s.SetStatus(root+".2.1", StatusDoing, "")
	s.ObserveFor(root+".2", Event{Tool: "read_file", Args: map[string]any{"path": "a.go"}, Content: strings.Repeat("secret-sub-agent-output\n", 20)})
	block := s.Render(8000, func(string) bool { return false })
	if !strings.Contains(block, "@big running (at "+root+".2.1, 1 tool call)") {
		t.Fatalf("no running line:\n%s", block)
	}
	if strings.Contains(block, "secret-sub-agent-output") {
		t.Fatal("the sub-agent's raw buffer must not render in the main model's block")
	}
}

func TestLoadWarnsOnUnknownOwnerAndScopeOutsideMax(t *testing.T) {
	s, root := planOwned(t)
	_ = s.SetOwner(root+".3", "claude", true)
	_ = s.SetScope(root+".3", []string{"docs/x.md"})
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	// Reopen with a config where claude may only own README.md and big is gone.
	s2, err := OpenAt(s.dir, s.root, "s1", true, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	s2.SetCards(map[string]subagent.Card{"claude": {Name: "claude", SubAgent: true, MaxScope: []string{"README.md"}}})
	warns := s2.Warnings()
	joined := strings.Join(warns, "\n")
	if !strings.Contains(joined, "big is not a sub-agent") || !strings.Contains(joined, "claude may only own paths under README.md") {
		t.Fatalf("warnings: %q", warns)
	}
}
```

Check the existing name of the warnings accessor (`grep -n "func (s \*Store) Warnings" internal/engine/store.go`); if it is spelled differently, use that name in the test.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/engine -run 'TestOwnerAndScopeRules|TestOneDoingPerPen|TestUnfiledPerPen|TestCloseAsAndInterrupt|TestStepsAndDispatchContext|TestRenderShowsADispatched|TestLoadWarnsOnUnknownOwner' -v`
Expected: compile errors (`SetCards`, `SetOwner`, … undefined).

- [ ] **Step 3: Tree — pens**

In `internal/engine/node.go`, add to `Tree`:

```go
	// dispatched names the roots of subtrees a sub-agent is working
	// (spec §1.3): each is its own pen with its own doing node. Not
	// persisted; the store sets it.
	dispatched map[string]bool
```

Add methods:

```go
// SetDispatched marks or clears a subtree root as dispatched.
func (t *Tree) SetDispatched(rootID string, on bool) {
	if t.dispatched == nil {
		t.dispatched = map[string]bool{}
	}
	if on {
		t.dispatched[rootID] = true
	} else {
		delete(t.dispatched, rootID)
	}
}

// Dispatched lists the dispatched roots in id order.
func (t *Tree) Dispatched() []string {
	ids := make([]string, 0, len(t.dispatched))
	for id := range t.dispatched {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// penOf is the dispatched root that contains id, or "" for the main
// model's pen. Ids are positional, so containment is a prefix test.
func (t *Tree) penOf(id string) string {
	for r := range t.dispatched {
		if id == r || strings.HasPrefix(id, r+".") {
			return r
		}
	}
	return ""
}

// DoingUnder is the doing node inside one dispatched subtree.
func (t *Tree) DoingUnder(rootID string) *Node {
	var found *Node
	t.Walk(func(n *Node, _ int) {
		if found == nil && n.Status == StatusDoing && (n.ID == rootID || strings.HasPrefix(n.ID, rootID+".")) {
			found = n
		}
	})
	return found
}
```

Change `Doing()` to skip dispatched pens:

```go
// Doing is the main model's doing node: the one not inside any dispatched
// subtree. A sub-agent's doing node is DoingUnder its root.
func (t *Tree) Doing() *Node {
	var found *Node
	t.Walk(func(n *Node, _ int) {
		if found == nil && n.Status == StatusDoing && t.penOf(n.ID) == "" {
			found = n
		}
	})
	return found
}
```

(`Doing` was a value receiver before; make it a pointer receiver and fix any `Tree{}`-value call sites the compiler reports — `Find` and `OwnerOf` can stay value receivers.)

In `SetStatus`, replace the `prev := t.Doing()` block:

```go
	if s == StatusDoing {
		var prev *Node
		if pen := t.penOf(id); pen != "" {
			prev = t.DoingUnder(pen)
		} else {
			prev = t.Doing()
		}
		if prev != nil && prev != n {
			prev.Status = StatusTodo
		}
	}
```

`ActiveBranch()` uses `Doing()`, so it stays the main model's branch. Add `sort` to the imports.

- [ ] **Step 4: Store — cards, pens, evidence per pen**

In `internal/engine/store.go`, add fields to `Store`:

```go
	cards map[string]subagent.Card // sub-agent cards, for owner/scope validation
```

Import `github.com/brown-enterprises/be-code/internal/subagent`.

Replace `activeNodeLocked` with a pen-aware pair:

```go
func (s *Store) activeNodeLocked() *Node { return s.activeNodeForLocked("") }

// activeNodeForLocked is the node evidence files under for one pen: the
// pen's doing node, else an unfiled node opened under the pen's root (or,
// for the main model, under the active root as before).
func (s *Store) activeNodeForLocked(pen string) *Node {
	if pen != "" {
		if d := s.tree.DoingUnder(pen); d != nil {
			return d
		}
		host := s.tree.Find(pen)
		if host == nil {
			return s.activeNodeForLocked("")
		}
		return s.unfiledUnderLocked(host)
	}
	if d := s.tree.Doing(); d != nil {
		return d
	}
	host := s.activeRootLocked()
	if host == nil || s.freshRoot {
		s.freshRoot = false
		n := s.tree.Add("", unfiledText)
		n.Status = StatusDoing
		s.markDirtyLocked()
		return n
	}
	return s.unfiledUnderLocked(host)
}

func (s *Store) unfiledUnderLocked(host *Node) *Node {
	for _, c := range host.Children {
		if c.Text == unfiledText && !c.Status.terminal() {
			c.Status = StatusDoing
			s.markDirtyLocked()
			return c
		}
	}
	n := s.tree.Add(host.ID, unfiledText)
	n.Status = StatusDoing
	s.markDirtyLocked()
	return n
}
```

In `adoptUnfiledLocked(n *Node)`, restrict adoption to unfiled nodes in `n`'s pen: where it walks the tree collecting open `unfiledText` nodes, skip any `u` with `s.tree.penOf(u.ID) != s.tree.penOf(n.ID)`.

In `Store.SetStatus`, the previous-doing distill must be per pen:

```go
		var prev *Node
		if pen := s.tree.penOf(id); pen != "" {
			prev = s.tree.DoingUnder(pen)
		} else {
			prev = s.tree.Doing()
		}
		if prev != nil && prev != n {
			s.rec.distillWith(prev, snap)
		}
```

`closeDoingLocked` stays as it is (`Plan` closes only the main model's doing node).

Add `ObserveFor`, refactoring `Observe` to call it:

```go
// Observe files a tool event under the main model's pen.
func (s *Store) Observe(ev Event) string { return s.ObserveFor("", ev) }

// ObserveFor files a tool event under one pen: "" is the main model, else
// the id of a dispatched root (spec §1.5).
func (s *Store) ObserveFor(pen string, ev Event) string {
	// … the body of the old Observe, with `n := s.activeNodeLocked()`
	// replaced by `n := s.activeNodeForLocked(pen)` …
}
```

`warnMultipleDoing`: count doing nodes per pen (`s.tree.penOf(n.ID)`) and warn only when a pen has more than one.

- [ ] **Step 5: Store — the API**

Add to `internal/engine/store.go`:

```go
// SetCards tells the store which owners exist and what each may be given;
// SetOwner and SetScope validate against it, and loadDocs' warnings use it.
func (s *Store) SetCards(cards map[string]subagent.Card) {
	s.mu.Lock()
	s.cards = cards
	s.mu.Unlock()
	s.warnOwnersLocked()
}

// warnOwnersLocked warns once per load about assignments the config cannot
// honour: an owner that is not a sub-agent, or a scope outside its max_scope.
func (s *Store) warnOwnersLocked() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tree.Walk(func(n *Node, _ int) {
		if n.Owner == "" || n.Status.terminal() {
			return
		}
		card, ok := s.cards[n.Owner]
		switch {
		case !ok || !card.SubAgent:
			s.warnf("task %s: %s is not a sub-agent; add \"sub_agent\": true to its coworkers entry", n.ID, n.Owner)
		case !subagent.Within(n.Scope, card.MaxScope):
			s.warnf("task %s: %s may only own paths under %s", n.ID, n.Owner, strings.Join(card.MaxScope, ", "))
		}
	})
}

// SetOwner assigns a node (spec §1.1). owner "" unassigns. pinned is the
// operator's form ("@name!"), which the model may not change or remove.
func (s *Store) SetOwner(id, owner string, pinned bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.tree.Find(id)
	if n == nil {
		return fmt.Errorf("no node %s", id)
	}
	if n.OwnerPinned && !pinned {
		return fmt.Errorf("%s was assigned by the operator; ask them to change it", id)
	}
	if s.tree.dispatched[id] {
		return fmt.Errorf("%s is being worked by %s; stop it first", id, n.Owner)
	}
	if owner != "" {
		if card, ok := s.cards[owner]; !ok || !card.SubAgent {
			return fmt.Errorf("%s is not a sub-agent; add \"sub_agent\": true to its coworkers entry", owner)
		}
		if above := s.tree.OwnerOf(parentID(id)); above != "" {
			return fmt.Errorf("%s is inside %s's subtree; assign the top of a subtree", id, above)
		}
		for _, c := range n.Children {
			if c.Owner != "" {
				return fmt.Errorf("%s has an assigned child (%s); one subtree, one pen", id, c.ID)
			}
		}
	}
	n.Owner, n.OwnerPinned = owner, owner != "" && pinned
	s.markDirtyLocked()
	return nil
}

// SetScope sets what the node's owner may write (spec §1.1, §1.4).
func (s *Store) SetScope(id string, scope []string) error {
	clean, err := subagent.CleanScope(scope)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.tree.Find(id)
	if n == nil {
		return fmt.Errorf("no node %s", id)
	}
	if n.Owner != "" {
		if card, ok := s.cards[n.Owner]; ok && !subagent.Within(clean, card.MaxScope) {
			return fmt.Errorf("%s may only own paths under %s", n.Owner, strings.Join(card.MaxScope, ", "))
		}
	}
	n.Scope = clean
	s.markDirtyLocked()
	return nil
}

// SetDispatched marks a subtree as being worked by its owner.
func (s *Store) SetDispatched(id string, on bool) {
	s.mu.Lock()
	s.tree.SetDispatched(id, on)
	s.mu.Unlock()
}

// Dispatched lists the dispatched roots.
func (s *Store) Dispatched() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tree.Dispatched()
}

// CloseAs closes a dispatched subtree on the sub-agent's behalf: the root
// and every open descendant take status (done or blocked), DoneBy is
// stamped, buffers are distilled, and the dispatched mark is cleared.
func (s *Store) CloseAs(id, owner, status, reason string) error {
	st, ok := parseStatus(status)
	if !ok || !st.terminal() {
		return fmt.Errorf("status must be done or blocked")
	}
	snap := s.rawSnaps()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.tree.Find(id)
	if n == nil {
		return fmt.Errorf("no node %s", id)
	}
	var close func(x *Node)
	close = func(x *Node) {
		for _, c := range x.Children {
			close(c)
		}
		if x.Status.terminal() {
			return
		}
		if x.Text == unfiledText && len(x.Evidence.Raw) == 0 && len(x.Children) == 0 {
			s.tree.Remove(x)
			return
		}
		s.rec.distillWith(x, snap)
		s.tree.SetStatus(x.ID, st, reason)
		x.DoneBy = owner
	}
	close(n)
	s.tree.SetDispatched(id, false)
	s.markDirtyLocked()
	return nil
}

// Interrupt undoes a dispatch without closing anything: the pen's doing
// node goes back to todo, the note is recorded on the root, and the
// dispatched mark is cleared (spec §3.5).
func (s *Store) Interrupt(id, note string) error {
	snap := s.rawSnaps()
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.tree.Find(id)
	if n == nil {
		return fmt.Errorf("no node %s", id)
	}
	if d := s.tree.DoingUnder(id); d != nil {
		s.rec.distillWith(d, snap)
		d.Status = StatusTodo
	}
	if note != "" {
		n.Evidence.Notes = append(n.Evidence.Notes, NoteRef{Text: note})
	}
	s.tree.SetDispatched(id, false)
	s.markDirtyLocked()
	return nil
}

// Touched lists the files written under a subtree, from its evidence.
func (s *Store) Touched(id string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.tree.Find(id)
	if n == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	var walk func(x *Node)
	walk = func(x *Node) {
		for _, f := range x.Evidence.Files {
			if f.Written && !seen[f.Path] {
				seen[f.Path] = true
				out = append(out, f.Path)
			}
		}
		for _, it := range x.Evidence.Raw {
			if (it.Tool == "write_file" || it.Tool == "edit_file") && it.OK && it.Path != "" && !seen[it.Path] {
				seen[it.Path] = true
				out = append(out, it.Path)
			}
		}
		for _, c := range x.Children {
			walk(c)
		}
	}
	walk(n)
	sort.Strings(out)
	return out
}
```

Check `FileRef`'s field for "written" (`grep -n "type FileRef" -A 12 internal/engine/node.go`); if the flag is named differently (for example `Wrote`), use that name in `Touched`. Add a `parentID(id string) string` helper (everything before the last `.`, or `""`).

`Steps()` and `DispatchContext`:

```go
// InterruptedNote is the prefix Interrupt's note carries (the agent writes
// it, Steps reads the files back out of it so a re-dispatch can say what
// was already written).
const InterruptedNote = "interrupted "

// Steps is the ready rule's view of the tree (spec §2.1).
func (s *Store) Steps() []subagent.Step {
	s.mu.Lock()
	defer s.mu.Unlock()
	var conv func(n *Node, inherited string) subagent.Step
	conv = func(n *Node, inherited string) subagent.Step {
		owner := n.Owner
		if owner == "" {
			owner = inherited
		}
		st := subagent.Step{ID: n.ID, Text: n.Text, Status: string(n.Status), Owner: owner,
			Scope: append([]string(nil), n.Scope...), After: append([]string(nil), n.After...)}
		for _, note := range n.Evidence.Notes {
			if strings.HasPrefix(note.Text, InterruptedNote) {
				st.Interrupted = true
				if _, files, ok := strings.Cut(note.Text, "files written: "); ok {
					st.Touched = splitList(files)
				}
			}
		}
		for _, c := range n.Children {
			st.Children = append(st.Children, conv(c, owner))
		}
		return st
	}
	var out []subagent.Step
	for _, r := range s.tree.Roots {
		out = append(out, conv(r, ""))
	}
	return out
}

// DispatchContext is what a Dispatch carries: the node's text, its
// children as "id text" lines, and the bounded context — the root task's
// status line, the path of ancestors, and the durable notes.
func (s *Store) DispatchContext(id string) (text string, children []string, ctx string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.tree.Find(id)
	if n == nil {
		return "", nil, ""
	}
	for _, c := range n.Children {
		children = append(children, c.ID+" "+c.Text)
	}
	var b strings.Builder
	parts := strings.Split(id, ".")
	for i := 1; i < len(parts); i++ {
		if a := s.tree.Find(strings.Join(parts[:i], ".")); a != nil {
			b.WriteString(statusLine(a) + "\n")
		}
	}
	if s.notes != "" {
		b.WriteString("\nNotes:\n" + s.notes)
	}
	return n.Text, children, strings.TrimSpace(b.String())
}
```

Interrupted status: a subtree whose *root* was interrupted has its root's `Status` still `todo` — `Interrupt` never changes the root — so `Ready` offers it again; `Interrupted`/`Touched` ride along. Once `CloseAs` closes it, the note stays in the document as history.

- [ ] **Step 6: Render — the dispatched subtree as one line**

In `render.go`'s `Render`, after `t := Tree{Roots: copyNodes(s.tree.Roots), nudge: s.lim.StepNudge}` (under the lock), mark the copies:

```go
	t.dispatched = s.tree.dispatched
	for _, r := range s.tree.Dispatched() {
		if n := t.Find(r); n != nil {
			n.Dispatched = true
			if d := t.DoingUnder(r); d != nil {
				n.DispatchedAt = d.ID
			}
			n.Calls = subtreeCalls(n)
		}
	}
```

with:

```go
// subtreeCalls sums Calls over a subtree, for the running line.
func subtreeCalls(n *Node) int {
	total := n.Calls
	for _, c := range n.Children {
		total += subtreeCalls(c)
	}
	return total
}
```

Because `ActiveBranch` follows the main model's doing node and `newActiveParts` renders siblings with `statusLine`, a dispatched sibling now renders as the one running line, and its children are never descended into (the branch does not pass through it). The sub-agent's raw buffer is on its own doing node, which is not `path[len(path)-1]`, so it never renders — that is what `TestRenderShowsADispatchedSubtreeAsOneLine` checks. If a dispatched subtree is *itself* on the active path (the main model set a node inside it doing — impossible through `SetStatus`, which routes by pen — but possible from a hand-edited document), `Doing()` skips it, so the branch falls back to the newest open root; acceptable.

- [ ] **Step 7: Load-time warnings**

In `loadDocs`, after `s.warnMultipleDoing()`, call `s.warnOwnersLocked()` only when `s.cards != nil` (cards arrive after `Open`, via `SetCards`, which warns itself; the call here covers a reload with cards already set). `warnOwnersLocked` takes `s.mu` itself, so call it after `loadDocs` releases the lock — check how `loadDocs` is called from `OpenAt` and from `Flush`'s reconcile and place the call outside the locked region.

- [ ] **Step 8: Run the engine tests**

Run: `go test ./internal/engine -race`
Expected: PASS, including every pre-existing test (`TestActiveWorkOutranksHistory`, the times tests, merge tests). A failure in `merge_test.go` means `copyNodes` or the merge's node comparison must learn the new fields — extend it to copy `Owner`, `OwnerPinned`, `Scope`, `After`, `DoneBy` (never `Dispatched`).

- [ ] **Step 9: Commit**

```bash
git add internal/engine
git commit -m "engine: one doing node per dispatched subtree; owner and scope API

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9"
```

---

### Task 6: Tools — the scoped registry, `ask_main`, and the `task` tool's new actions

**Files:**
- Modify: `internal/tools/tool.go:61-118` (`Registry`), `:142-156` (`Subset`), `internal/tools/fs.go:137,181` (write/edit path resolution), `internal/tools/shell.go:30-63` (`Run`), `internal/tools/task.go` (interface, schema, `Run`)
- Create: `internal/tools/askmain.go`
- Modify: `cmd/engine.go:16-23` (`noopLedger`)
- Test: `internal/tools/scoped_test.go` (new), `internal/tools/task_test.go`

**Interfaces:**
- Consumes: `subagent.InScope`.
- Produces: `func (r *Registry) Scoped(scope, checks []string, label string) *Registry`; `Registry.scope []string`, `Registry.checks []string` (unexported); `func NewAskMain(ask func(ctx context.Context, question string) (string, error)) Tool` (tool name `ask_main`); `TaskLedger` gains `SetOwner(id, owner string, pinned bool) error` and `SetScope(id string, scope []string) error`; optional `taskReplier interface{ Reply(id, text string) error }`; `func NewTaskUnder(l TaskLedger, root string) Tool`; `task` actions `owner`, `scope`, `reply`.

- [ ] **Step 1: Write the failing scoped-registry tests**

`internal/tools/scoped_test.go`:

```go
package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func scopedReg(t *testing.T, approve ApproveFunc) (*Registry, string) {
	t.Helper()
	dir := t.TempDir()
	for _, p := range []string{"internal/scan/a.go", "cmd/x.go"} {
		full := filepath.Join(dir, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reg, err := NewRegistry(dir, approve)
	if err != nil {
		t.Fatal(err)
	}
	reg.ApproveWrites = true
	sub := reg.Scoped([]string{"internal/scan"}, []string{"go vet ./...", "go test ./..."}, "big (3.2)")
	return sub, dir
}

func TestScopedWritesStayInScope(t *testing.T) {
	var details []string
	sub, dir := scopedReg(t, func(action, detail string) bool { details = append(details, action+"\n"+detail); return true })
	res := sub.Dispatch(context.Background(), call("write_file", `{"path":"internal/scan/b.go","content":"new\n"}`))
	if res.IsError {
		t.Fatalf("in-scope write refused: %s", res.Content)
	}
	if _, err := os.Stat(filepath.Join(dir, "internal/scan/b.go")); err != nil {
		t.Fatal("file not written")
	}
	if len(details) != 1 || !strings.HasPrefix(details[0], "file_write\nsub-agent big (3.2):\n") {
		t.Fatalf("approval detail: %q", details)
	}
	res = sub.Dispatch(context.Background(), call("write_file", `{"path":"cmd/y.go","content":"x"}`))
	if !res.IsError || !strings.Contains(res.Content, "outside your scope (internal/scan)") || !strings.Contains(res.Content, "ask_main") {
		t.Fatalf("out-of-scope write: %+v", res)
	}
	res = sub.Dispatch(context.Background(), call("edit_file", `{"path":"cmd/x.go","old":"old","new":"new"}`))
	if !res.IsError || !strings.Contains(res.Content, "outside your scope") {
		t.Fatalf("out-of-scope edit: %+v", res)
	}
	if len(details) != 1 {
		t.Fatal("a refused write must not prompt")
	}
	res = sub.Dispatch(context.Background(), call("read_file", `{"path":"cmd/x.go"}`))
	if res.IsError {
		t.Fatalf("reads are allowed anywhere: %s", res.Content)
	}
}

func TestScopedShellRunsOnlyTheChecks(t *testing.T) {
	prompted := false
	sub, _ := scopedReg(t, func(action, detail string) bool { prompted = true; return true })
	res := sub.Dispatch(context.Background(), call("shell", `{"command":"go vet ./... && rm -rf /"}`))
	if !res.IsError || !strings.Contains(res.Content, "only the project's checks may be run: go vet ./..., go test ./...") {
		t.Fatalf("compound refused wrong: %+v", res)
	}
	res = sub.Dispatch(context.Background(), call("shell", `{"command":"ls"}`))
	if !res.IsError {
		t.Fatal("ls is not a check")
	}
	if prompted {
		t.Fatal("a refused command must not prompt")
	}
	res = sub.Dispatch(context.Background(), call("shell", `{"command":"go vet ./..."}`))
	if prompted {
		t.Fatal("an allowed check must not prompt")
	}
	_ = res // it may fail in a temp dir with no module; only the gate is under test
}

func TestScopedRegistryHasNoEscapeTools(t *testing.T) {
	sub, _ := scopedReg(t, nil)
	names := strings.Join(sub.Names(), " ")
	for _, banned := range []string{"process", "consult", "web_search", "web_fetch"} {
		if strings.Contains(names, banned) {
			t.Fatalf("%s reachable from a scoped registry: %s", banned, names)
		}
	}
	for _, want := range []string{"read_file", "write_file", "edit_file", "list_dir", "search", "shell"} {
		if !strings.Contains(names, want) {
			t.Fatalf("%s missing: %s", want, names)
		}
	}
}
```

`call` — check the existing test helper name that builds a `provider.ToolCall` from a name and JSON arguments (`grep -n "func call\|func tc(" internal/tools/*_test.go`); reuse it, or add:

```go
func call(name, args string) provider.ToolCall {
	return provider.ToolCall{ID: "1", Name: name, Arguments: args}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/tools -run TestScoped -v`
Expected: `Scoped` undefined.

- [ ] **Step 3: Implement Scoped, the scope check and the shell gate**

In `internal/tools/tool.go`, add fields to `Registry` after `Hooks`:

```go
	// scope and checks confine a sub-agent's registry (spec §2.4): writes
	// only under scope, shell only for exactly one of checks. Both nil on
	// the main registry. label names the sub-agent in approval details.
	scope  []string
	checks []string
	label  string
```

Add after `Subset`:

```go
// Scoped is a sub-agent's registry: reads anywhere, writes under scope,
// shell only for the project's own checks, and none of the tools that
// reach outside the workspace (process, consult, web, editor, MCP). It
// shares the main registry's approval seam, so a write is approved and
// checkpointed exactly as the main model's is.
func (r *Registry) Scoped(scope, checks []string, label string) *Registry {
	sub := r.Subset("read_file", "write_file", "edit_file", "list_dir", "search", "shell",
		"lookup", "history", "show", "changes")
	sub.ApproveCtx = r.ApproveCtx
	sub.ReviewWrite = r.ReviewWrite
	sub.ReviewInvolvesEditor = r.ReviewInvolvesEditor
	sub.EditorName = r.EditorName
	sub.OnStatus = r.OnStatus
	sub.scope, sub.checks, sub.label = scope, checks, label
	sub.maxOutput.Store(r.maxOutput.Load())
	if r.Approve != nil {
		parent := r.Approve
		sub.Approve = func(action, detail string) bool {
			return parent(action, "sub-agent "+label+":\n"+detail)
		}
	}
	// The tools hold a pointer to the registry they were built with; rebind
	// them to this one so confinement reads sub.scope.
	sub.tools, sub.byName = nil, map[string]Tool{}
	for _, t := range []Tool{&readFileTool{r: sub}, &writeFileTool{r: sub}, &editFileTool{r: sub},
		&listDirTool{r: sub}, &searchTool{r: sub}, &shellTool{r: sub}} {
		sub.add(t)
	}
	for _, n := range []string{"lookup", "history", "show", "changes"} {
		if t, ok := r.byName[n]; ok {
			sub.add(t)
		}
	}
	return sub
}

// checkScope refuses a write outside a scoped registry's scope.
func (r *Registry) checkScope(absPath string) error {
	if r.scope == nil {
		return nil
	}
	rel, err := filepath.Rel(r.Root, absPath)
	if err != nil {
		return err
	}
	if !subagent.InScope(r.scope, filepath.ToSlash(rel)) {
		return fmt.Errorf("path is outside your scope (%s); use ask_main if you need it widened", strings.Join(r.scope, ", "))
	}
	return nil
}
```

Check the struct names of the built-in tools (`grep -n "type .*Tool struct" internal/tools/fs.go internal/tools/shell.go internal/tools/search.go internal/tools/dir.go`) and use the real ones. The git tools take the registry at construction (`NewGitTools(reg, …)`) and only read; sharing the parent's instances is fine.

In `fs.go`, in both `writeFileTool.Run` and `editFileTool.Run`, right after `p, err := t.r.resolve(...)` succeeds:

```go
	if err := t.r.checkScope(p); err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
```

In `shell.go`'s `Run`, before the `classifyCommand` switch:

```go
	if t.r.checks != nil {
		allowed := false
		for _, c := range t.r.checks {
			if command == c {
				allowed = true
				break
			}
		}
		if !allowed {
			return Result{IsError: true, Content: "only the project's checks may be run: " + strings.Join(t.r.checks, ", ")}
		}
		// An exact check never prompts and is never denied: it is what the
		// harness itself runs at verification.
		return t.execute(ctx, command, args)
	}
```

where `execute` is the part of `Run` after the approval switch (the `pre_shell` hook, timeout parsing and `RunShell` call) extracted into a method `func (t *shellTool) execute(ctx context.Context, command string, args map[string]any) Result` so the ordinary path calls it too. Import `subagent` in tool.go.

- [ ] **Step 4: Run the scoped tests**

Run: `go test ./internal/tools -run TestScoped -v`
Expected: PASS.

- [ ] **Step 5: `ask_main`**

`internal/tools/askmain.go`:

```go
package tools

import (
	"context"
	"encoding/json"
	"strings"
)

// AskMainFunc parks the sub-agent until the main model or the operator
// answers, or the ask times out.
type AskMainFunc func(ctx context.Context, question string) (string, error)

type askMainTool struct{ ask AskMainFunc }

// NewAskMain is the one tool a sub-agent has that reaches the main model
// (spec §2.7). The agent supplies the round trip.
func NewAskMain(ask AskMainFunc) Tool { return &askMainTool{ask: ask} }

func (t *askMainTool) Name() string { return "ask_main" }
func (t *askMainTool) Description() string {
	return "Ask the main model one precise question and wait for its answer: a file outside your scope, a decision you cannot make, information only it has. Use it once, then continue."
}
func (t *askMainTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"question":{"type":"string","description":"One precise question"}},
		"required":["question"]}`)
}
func (t *askMainTool) Run(ctx context.Context, args map[string]any) Result {
	q := strings.TrimSpace(argString(args, "question", "text", "q"))
	if q == "" {
		return Result{IsError: true, Content: "question is required"}
	}
	answer, err := t.ask(ctx, q)
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	return Result{Content: "the main model replied:\n\n" + answer}
}
```

- [ ] **Step 6: Write the failing task-tool tests**

Append to `internal/tools/task_test.go` (check its existing fake ledger's name — it will need the two new methods; extend it):

```go
type ownerLedger struct {
	fakeLedger // the existing fake in this file; embed and extend
	owners  map[string]string
	pinned  map[string]bool
	scopes  map[string][]string
	replies map[string]string
}

func (l *ownerLedger) SetOwner(id, owner string, pinned bool) error {
	if l.pinned[id] && !pinned {
		return errors.New(id + " was assigned by the operator; ask them to change it")
	}
	l.owners[id] = owner
	l.pinned[id] = pinned
	return nil
}
func (l *ownerLedger) SetScope(id string, scope []string) error { l.scopes[id] = scope; return nil }
func (l *ownerLedger) Reply(id, text string) error                { l.replies[id] = text; return nil }

func newOwnerLedger() *ownerLedger {
	return &ownerLedger{owners: map[string]string{}, pinned: map[string]bool{}, scopes: map[string][]string{}, replies: map[string]string{}}
}

func TestTaskOwnerScopeReply(t *testing.T) {
	l := newOwnerLedger()
	tool := NewTask(l)
	res := tool.Run(context.Background(), map[string]any{"action": "owner", "id": "3.2", "owner": "big"})
	if res.IsError || l.owners["3.2"] != "big" || l.pinned["3.2"] {
		t.Fatalf("owner: %+v %v", res, l.owners)
	}
	l.pinned["3.3"] = true
	res = tool.Run(context.Background(), map[string]any{"action": "owner", "id": "3.3", "owner": "big"})
	if !res.IsError || !strings.Contains(res.Content, "assigned by the operator") {
		t.Fatalf("pinned: %+v", res)
	}
	res = tool.Run(context.Background(), map[string]any{"action": "scope", "id": "3.2", "paths": []any{"internal/scan", "docs"}})
	if res.IsError || len(l.scopes["3.2"]) != 2 {
		t.Fatalf("scope: %+v %v", res, l.scopes)
	}
	res = tool.Run(context.Background(), map[string]any{"action": "scope", "id": "3.2", "paths": "a, b"})
	if res.IsError || len(l.scopes["3.2"]) != 2 || l.scopes["3.2"][1] != "b" {
		t.Fatalf("scope from a string: %+v %v", res, l.scopes)
	}
	res = tool.Run(context.Background(), map[string]any{"action": "reply", "id": "3.2", "text": "use the old tokenizer"})
	if res.IsError || l.replies["3.2"] != "use the old tokenizer" {
		t.Fatalf("reply: %+v %v", res, l.replies)
	}
}

func TestTaskUnderRestrictsToTheSubtree(t *testing.T) {
	l := newOwnerLedger()
	tool := NewTaskUnder(l, "3.2")
	for _, action := range []string{"plan", "owner", "scope", "reply"} {
		res := tool.Run(context.Background(), map[string]any{"action": action, "id": "3.2.1", "text": "x", "owner": "big"})
		if !res.IsError {
			t.Fatalf("%s allowed under a subtree", action)
		}
	}
	res := tool.Run(context.Background(), map[string]any{"action": "status", "id": "3.3", "status": "done"})
	if !res.IsError || !strings.Contains(res.Content, "outside your step 3.2") {
		t.Fatalf("status outside the subtree: %+v", res)
	}
	res = tool.Run(context.Background(), map[string]any{"action": "status", "id": "", "status": "done"})
	if !res.IsError {
		t.Fatal("an empty id must be refused under a subtree")
	}
	res = tool.Run(context.Background(), map[string]any{"action": "add", "parent": "3.2", "text": "update the tests"})
	if res.IsError {
		t.Fatalf("add under the subtree refused: %+v", res)
	}
	res = tool.Run(context.Background(), map[string]any{"action": "status", "id": "3.2.1", "status": "done"})
	if res.IsError {
		t.Fatalf("status inside the subtree refused: %+v", res)
	}
}
```

Imports: `context`, `errors`, `strings`, `testing`.

- [ ] **Step 7: Run them to verify they fail**

Run: `go test ./internal/tools -run 'TestTaskOwnerScopeReply|TestTaskUnderRestricts' -v`
Expected: compile errors (interface methods, `NewTaskUnder`).

- [ ] **Step 8: Extend the task tool**

In `internal/tools/task.go`, extend `TaskLedger`:

```go
type TaskLedger interface {
	Plan(text string, steps []string) string
	Add(parent, text string) (string, error)
	SetStatusText(id, status, reason string) error
	Note(id, text, file string, decision, keep bool) error
	ShowText(id string) string
	// SetOwner and SetScope assign a step to a sub-agent (spec §1.1). The
	// model's assignments are never pinned; pinning is the operator's.
	SetOwner(id, owner string, pinned bool) error
	SetScope(id string, scope []string) error
}

// taskReplier answers a sub-agent's open ask_main; the agent's ledger
// implements it, a bare store does not.
type taskReplier interface{ Reply(id, text string) error }
```

Add `under string` to `taskTool` and:

```go
// NewTaskUnder is the task tool a sub-agent gets: add, status, note and
// show, only for nodes under root.
func NewTaskUnder(l TaskLedger, root string) Tool { return &taskTool{l: l, under: root} }
```

Extend `Schema()`: the `action` enum becomes `["plan","add","status","note","show","owner","scope","reply"]`, described `plan | add | status | note | show | owner (assign a step to a sub-agent) | scope (paths it may write) | reply (answer a sub-agent's question)`; add properties `"owner":{"type":"string","description":"owner: a sub-agent's name, or main"}`, `"paths":{"type":"array","items":{"type":"string"},"description":"scope: workspace paths the owner may write"}`. Add `"text"` to the `reply` description.

In `Run`, before the switch, the subtree guard:

```go
	if t.under != "" {
		switch action {
		case "plan", "owner", "scope", "reply":
			return Result{IsError: true, Content: action + " is not available to a sub-agent; use ask_main if the plan must change"}
		}
		id := argString(args, "id", "node", "step", "task")
		if action == "add" {
			id = argString(args, "parent", "under", "parent_id")
		}
		if id == "" {
			return Result{IsError: true, Content: "name a step under " + t.under}
		}
		if id != t.under && !strings.HasPrefix(id, t.under+".") {
			return Result{IsError: true, Content: id + " is outside your step " + t.under}
		}
	}
```

Add the cases:

```go
	case "owner", "assign":
		id := argString(args, "id", "node", "step", "task")
		owner := strings.TrimSpace(argString(args, "owner", "to", "who"))
		if owner == "main" {
			owner = ""
		}
		if err := t.l.SetOwner(id, owner, false); err != nil {
			return Result{IsError: true, Content: err.Error()}
		}
		if owner == "" {
			return Result{Content: id + " is yours again"}
		}
		return Result{Content: id + " assigned to " + owner + "; set its scope with action scope if it has none"}
	case "scope":
		id := argString(args, "id", "node", "step", "task")
		paths := argStrings(args, "paths", "scope", "files")
		if len(paths) == 1 && strings.Contains(paths[0], ",") {
			paths = splitCommaList(paths[0])
		}
		if err := t.l.SetScope(id, paths); err != nil {
			return Result{IsError: true, Content: err.Error()}
		}
		return Result{Content: id + " scope: " + strings.Join(paths, ", ")}
	case "reply", "answer":
		r, ok := t.l.(taskReplier)
		if !ok {
			return Result{IsError: true, Content: "no sub-agent is asking"}
		}
		id := argString(args, "id", "node", "step", "task")
		text := argString(args, "text", "answer", "reply")
		if err := r.Reply(id, text); err != nil {
			return Result{IsError: true, Content: err.Error()}
		}
		return Result{Content: "reply delivered to the sub-agent on " + id}
```

with:

```go
func splitCommaList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
```

Check that `argStrings` accepts a plain string as a one-element list (task.go:187); if it does not, handle the string form as above.

Extend `noopLedger` in `cmd/engine.go`:

```go
func (noopLedger) SetOwner(string, string, bool) error { return nil }
func (noopLedger) SetScope(string, []string) error     { return nil }
```

and `fencedLedger` in `internal/agent/enginefence.go:192-196` (it wraps the store's methods through `engineDo`; add `SetOwner` and `SetScope` the same way — the store now has both). `Reply` on `fencedLedger` comes in Task 7.

- [ ] **Step 9: Run the tools tests and the build**

Run: `go build ./... && go test ./internal/tools ./internal/agent ./cmd`
Expected: build OK, PASS. Any other `TaskLedger` fake in the tree (`grep -rn "ShowText(" --include=*_test.go internal cmd`) needs the two new methods.

- [ ] **Step 10: Commit**

```bash
git add internal/tools cmd/engine.go internal/agent/enginefence.go
git commit -m "tools: scoped registry, ask_main, and task owner/scope/reply

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9"
```

---

### Task 7: Agent — the runner: dispatch, ask, hand-back, stop, resume, lanes

**Files:**
- Create: `internal/agent/subprompt.go`, `internal/agent/subagents.go`
- Modify: `internal/agent/loop.go:29-47` (`Events`), `:97-282` (`Agent` fields), `:928-959` (`composeSystem`), `:1065` (`run`, schedule at start), `:1431-1470` (`dispatch`: schedule after a `task` call, owned-path footer), `internal/agent/resilience.go:29-33` (lane hook), `internal/agent/enginefence.go:196-240` (`fencedLedger.Reply`)
- Test: `internal/agent/subagents_test.go` (new)

**Interfaces:**
- Consumes: `subagent.*` (Task 2/3), `engine.Store.SetCards/Steps/SetDispatched/CloseAs/Interrupt/Touched/DispatchContext/ObserveFor` (Task 5), `tools.Registry.Scoped`, `tools.NewAskMain`, `tools.NewTaskUnder` (Task 6), `CoworkerFactory`, `consultAgent`'s construction pattern, `sanitizeAdvice`, `Enqueue`, `engineDo`.
- Produces:
  - `Events.OnSubAgentStart func(d subagent.Dispatch)`, `Events.OnSubAgentAsk func(ask subagent.Ask)`, `Events.OnSubAgentEnd func(hb subagent.HandBack)` (`hb.Status` is `done`, `blocked` or `interrupted`)
  - `func (a *Agent) EnableSubAgents(cws []config.CoworkerConfig, primaryServer string)` (wires; never dispatches); `func (a *Agent) StartSubAgents()` (resume pass + first schedule — the UIs call it once `Approve` and `Events` are wired); `func (a *Agent) SubAgentsEnabled() bool`
  - `func (a *Agent) ScheduleSubAgents()`
  - `func (a *Agent) ReplyAsk(id, text string) error`
  - `func (a *Agent) AssignOwner(id, owner string, pinned bool) error` (the UI's path; stops a parked run first); `func (a *Agent) SetScope(id string, paths []string) error`
  - `func (a *Agent) StopSubAgent(name string) error`; `func (a *Agent) StopAllSubAgents(reason string)`
  - `type SubAgentState struct { Name, Model, Provider string; Online, SubAgent bool; MaxScope []string; Node, State string; Calls int; Since time.Time }`; `func (a *Agent) SubAgentStates() []SubAgentState`
  - `func (a *Agent) RunningSubAgents() []SubAgentState` (only running ones, for the bottom line)
  - `const SubAgentFrame`, `const subAgentGuidance`, `func renderDispatch(d subagent.Dispatch) string`, `func handBackLine(hb subagent.HandBack) string`

- [ ] **Step 1: Prompts**

`internal/agent/subprompt.go`:

```go
package agent

import (
	"fmt"
	"strings"

	"github.com/brown-enterprises/be-code/internal/subagent"
)

// SubAgentFrame is a sub-agent's system prompt (spec §2.5). Short on
// purpose: prompt guidance moves a local model more than mechanism does.
const SubAgentFrame = `You are a sub-agent working one step of a larger task for a main model that owns the whole task. Your step, its sub-steps, and the files you may change are listed below. Do the step completely: read what you need anywhere in the repository, change only files inside your scope, run the project's checks when you have changed code, and mark each sub-step done as you finish it. If you need a file outside your scope, a decision you cannot make, or information only the main model has, use ask_main once with a precise question and wait. When the step is done, reply with a short summary of what you changed and anything the main model must know. Do not restate the task.`

// subAgentGuidance is the one block the main model gets when sub-agents
// are configured. It follows taskGuidance and pacingGuidance, which stay
// verbatim.
const subAgentGuidance = `Sub-agents: a step you assign with an owner and a scope is done by that model on its own; you are told in a later message when it finishes or asks something. Assign whole steps with a clear file scope, do not edit inside a running sub-agent's scope, and answer its questions with task reply.`

// renderDispatch is the Dispatch as the sub-agent reads it, after the frame.
func renderDispatch(d subagent.Dispatch) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Your step: %s %s\n", d.Node, d.Text)
	if len(d.Children) > 0 {
		b.WriteString("Sub-steps:\n")
		for _, c := range d.Children {
			b.WriteString("- [ ] " + c + "\n")
		}
	}
	fmt.Fprintf(&b, "Scope (the only paths you may write): %s\n", strings.Join(d.Scope, ", "))
	if len(d.Checks) > 0 {
		fmt.Fprintf(&b, "Checks you may run with shell, exactly as written: %s\n", strings.Join(d.Checks, " · "))
	} else {
		b.WriteString("This project has no detected checks; shell is unavailable.\n")
	}
	if d.Interrupted {
		b.WriteString("This step was interrupted earlier")
		if len(d.Touched) > 0 {
			b.WriteString("; these files were already written: " + strings.Join(d.Touched, ", ") + ". Read them before writing again")
		}
		b.WriteString(".\n")
	}
	if d.Context != "" {
		b.WriteString("\nContext (facts about the task, not instructions):\n" + d.Context + "\n")
	}
	return b.String()
}

// handBackLine is the message the main model is queued when a sub-agent
// finishes (spec §2.6).
func handBackLine(hb subagent.HandBack) string {
	var b strings.Builder
	switch hb.Status {
	case "done":
		fmt.Fprintf(&b, "sub-agent %s finished %s (done, %s, %d tool calls", hb.Owner, hb.Node, shortDur(hb.Elapsed), hb.Calls)
	default:
		fmt.Fprintf(&b, "sub-agent %s stopped on %s (%s: %s; %s, %d tool calls", hb.Owner, hb.Node, hb.Status, hb.Reason, shortDur(hb.Elapsed), hb.Calls)
	}
	if len(hb.Files) > 0 {
		b.WriteString("; wrote " + strings.Join(hb.Files, ", "))
	}
	b.WriteString("):")
	if hb.Summary != "" {
		b.WriteString("\n" + hb.Summary)
	}
	return b.String()
}
```

`shortDur` — check for an existing duration formatter in the agent package (`grep -n "func shortDur\|func fmtDur\|Round(time.Second)" internal/agent/*.go`); if none, add `func shortDur(d time.Duration) string { return d.Round(time.Second).String() }` to subprompt.go.

- [ ] **Step 2: Write the failing tests**

`internal/agent/subagents_test.go`:

```go
package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/subagent"
)

// subFixture is a primary agent with a store, one sub-agent co-worker
// "big" on a scripted provider, and channels the tests wait on.
type subFixture struct {
	ag    *Agent
	st    *engine.Store
	dir   string
	sub   *scriptedProvider
	start chan subagent.Dispatch
	asks  chan subagent.Ask
	ends  chan subagent.HandBack
}

func newSubFixture(t *testing.T, sub *scriptedProvider, mut func(*config.Config)) *subFixture {
	t.Helper()
	ag, dir := newTestAgent(t, &scriptedProvider{}, func(c *config.Config) {
		c.Coworkers = []config.CoworkerConfig{{Name: "big", Provider: "ollama", Model: "cw-model-big", SubAgent: true}}
		c.Providers["ollama"] = config.ProviderConfig{Type: "ollama", BaseURL: "http://sub:11434"}
		c.SubAgents = config.SubAgentsConfig{MaxConcurrent: 2, MaxTurns: 6, AskTimeout: 600}
		if mut != nil {
			mut(c)
		}
	})
	f := &subFixture{ag: ag, dir: dir, sub: sub,
		start: make(chan subagent.Dispatch, 4), asks: make(chan subagent.Ask, 4), ends: make(chan subagent.HandBack, 4)}
	f.st = withEngine(t, ag)
	ag.Events.OnSubAgentStart = func(d subagent.Dispatch) { f.start <- d }
	ag.Events.OnSubAgentAsk = func(a subagent.Ask) { f.asks <- a }
	ag.Events.OnSubAgentEnd = func(hb subagent.HandBack) { f.ends <- hb }
	CoworkerFactory = func(context.Context, *config.Config, config.CoworkerConfig) (provider.Provider, int, error) {
		return sub, 0, nil
	}
	t.Cleanup(func() { CoworkerFactory = nil; ag.StopAllSubAgents("test over") })
	if err := os.MkdirAll(filepath.Join(dir, "internal/scan"), 0o755); err != nil {
		t.Fatal(err)
	}
	cws, _ := ag.Cfg.ValidCoworkers()
	ag.EnableSubAgents(cws, "http://primary:11434")
	return f
}

// assign plans a task with one step owned by big and returns the step id.
func (f *subFixture) assign(t *testing.T) string {
	t.Helper()
	root := f.st.Plan("port the scanner", []string{"port internal/scan"})
	id := root + ".1"
	if err := f.st.SetOwner(id, "big", false); err != nil {
		t.Fatal(err)
	}
	if err := f.st.SetScope(id, []string{"internal/scan"}); err != nil {
		t.Fatal(err)
	}
	return id
}

func wait[T any](t *testing.T, ch chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func toolCall(name, args string) provider.ChatResponse {
	return provider.ChatResponse{ToolCalls: []provider.ToolCall{{ID: "c", Name: name, Arguments: args}}}
}

func TestSubAgentOwnsAStepToDone(t *testing.T) {
	sub := &scriptedProvider{responses: []provider.ChatResponse{
		toolCall("write_file", `{"path":"internal/scan/token.go","content":"package scan\n"}`),
		toolCall("task", `{"action":"status","id":"1.1","status":"done"}`),
		{Content: "ported the token loop"},
	}}
	f := newSubFixture(t, sub, nil)
	id := f.assign(t)
	f.ag.ScheduleSubAgents()
	d := wait(t, f.start, "start")
	if d.Node != id || d.Owner != "big" || d.Scope[0] != "internal/scan" || d.MaxTurns != 6 {
		t.Fatalf("dispatch: %+v", d)
	}
	hb := wait(t, f.ends, "hand-back")
	if hb.Status != "done" || hb.Summary != "ported the token loop" || len(hb.Files) != 1 || hb.Files[0] != "internal/scan/token.go" {
		t.Fatalf("hand-back: %+v", hb)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "internal/scan/token.go")); err != nil {
		t.Fatal("the sub-agent's file was not written")
	}
	if f.ag.Pending() != 1 {
		t.Fatalf("queued messages: %d", f.ag.Pending())
	}
	line := f.ag.DrainInbox()[0]
	if !strings.HasPrefix(line, "sub-agent big finished "+id+" (done, ") || !strings.Contains(line, "wrote internal/scan/token.go") || !strings.HasSuffix(line, "ported the token loop") {
		t.Fatalf("hand-back line: %q", line)
	}
	if got := f.st.ShowText(id); !strings.Contains(got, "[x] "+id+".") {
		t.Fatalf("node not closed:\n%s", got)
	}
	if len(f.st.Dispatched()) != 0 {
		t.Fatal("dispatched mark not cleared")
	}
	// The sub-agent's system prompt carried the frame and the dispatch, and
	// its first request went to the co-worker's provider, not the primary's.
	sys := sub.lastReq.Messages[0].Content
	if !strings.HasPrefix(sys, SubAgentFrame) || !strings.Contains(sys, "Your step: "+id+" port internal/scan") || !strings.Contains(sys, "Scope (the only paths you may write): internal/scan") {
		t.Fatalf("sub-agent system prompt:\n%s", sys)
	}
}

func TestSubAgentAskMainRoundTrip(t *testing.T) {
	sub := &scriptedProvider{responses: []provider.ChatResponse{
		toolCall("write_file", `{"path":"cmd/x.go","content":"x"}`),
		toolCall("ask_main", `{"question":"may I edit cmd/x.go?"}`),
		{Content: "done as told"},
	}}
	f := newSubFixture(t, sub, nil)
	id := f.assign(t)
	f.ag.ScheduleSubAgents()
	wait(t, f.start, "start")
	ask := wait(t, f.asks, "ask")
	if ask.Node != id || ask.Owner != "big" || ask.Question != "may I edit cmd/x.go?" {
		t.Fatalf("ask: %+v", ask)
	}
	if f.ag.Pending() != 1 || !strings.HasPrefix(f.ag.DrainInbox()[0], "sub-agent big asks about "+id+": may I edit cmd/x.go?") {
		t.Fatal("the ask was not queued for the main model")
	}
	if err := f.ag.ReplyAsk(id, "no; leave cmd alone"); err != nil {
		t.Fatal(err)
	}
	if err := f.ag.ReplyAsk(id, "again"); err == nil {
		t.Fatal("a second reply with no open ask must fail")
	}
	hb := wait(t, f.ends, "hand-back")
	if hb.Status != "done" {
		t.Fatalf("hand-back: %+v", hb)
	}
	// The refused write and the reply both reached the sub-agent as tool results.
	var sawRefusal, sawReply bool
	for _, m := range sub.lastReq.Messages {
		if strings.Contains(m.Content, "outside your scope (internal/scan)") {
			sawRefusal = true
		}
		if strings.Contains(m.Content, "the main model replied:\n\nno; leave cmd alone") {
			sawReply = true
		}
	}
	if !sawRefusal || !sawReply {
		t.Fatalf("refusal %v reply %v in:\n%+v", sawRefusal, sawReply, sub.lastReq.Messages)
	}
	if _, err := os.Stat(filepath.Join(f.dir, "cmd/x.go")); err == nil {
		t.Fatal("an out-of-scope write landed on disk")
	}
	if got := f.st.ShowText(id); !strings.Contains(got, "may I edit cmd/x.go?") || !strings.Contains(got, "no; leave cmd alone") {
		t.Fatalf("ask and reply not recorded on the node:\n%s", got)
	}
}

func TestSubAgentAskTimesOutToBlocked(t *testing.T) {
	sub := &scriptedProvider{responses: []provider.ChatResponse{
		toolCall("ask_main", `{"question":"anyone there?"}`),
		{Content: "should never be reached"},
	}}
	f := newSubFixture(t, sub, func(c *config.Config) { c.SubAgents.AskTimeout = 1 })
	id := f.assign(t)
	f.ag.ScheduleSubAgents()
	wait(t, f.asks, "ask")
	hb := wait(t, f.ends, "hand-back")
	if hb.Status != "blocked" || hb.Reason != "no answer to: anyone there?" {
		t.Fatalf("hand-back: %+v", hb)
	}
	if got := f.st.ShowText(id); !strings.Contains(got, "[!] "+id+".") {
		t.Fatalf("node not blocked:\n%s", got)
	}
}

func TestSubAgentTurnCapAndPanicBlock(t *testing.T) {
	loop := &scriptedProvider{}
	for i := 0; i < 10; i++ {
		loop.responses = append(loop.responses, toolCall("list_dir", `{"path":"."}`))
	}
	f := newSubFixture(t, loop, func(c *config.Config) { c.SubAgents.MaxTurns = 2 })
	f.assign(t)
	f.ag.ScheduleSubAgents()
	hb := wait(t, f.ends, "hand-back")
	if hb.Status != "blocked" || hb.Reason != "turn cap of 2 reached" {
		t.Fatalf("turn cap: %+v", hb)
	}
	pp := &panicProvider{}
	CoworkerFactory = func(context.Context, *config.Config, config.CoworkerConfig) (provider.Provider, int, error) { return pp, 0, nil }
	root := f.st.Plan("second", []string{"again"})
	_ = f.st.SetOwner(root+".1", "big", false)
	_ = f.st.SetScope(root+".1", []string{"internal/scan"})
	f.ag.ScheduleSubAgents()
	hb = wait(t, f.ends, "hand-back after panic")
	if hb.Status != "blocked" || !strings.HasPrefix(hb.Reason, "internal error: ") {
		t.Fatalf("panic: %+v", hb)
	}
	if _, err := f.ag.Run(context.Background(), "still alive?"); err != nil {
		t.Fatalf("the primary must survive a sub-agent panic: %v", err)
	}
}

func TestOnlineSubAgentAsksConsentNamingTheScope(t *testing.T) {
	sub := &scriptedProvider{responses: []provider.ChatResponse{{Content: "never sent"}}}
	var detail string
	f := newSubFixture(t, sub, func(c *config.Config) { c.Coworkers[0].Online = true })
	f.ag.Tools.Approve = func(action, d string) bool { detail = action + "\n" + d; return false }
	id := f.assign(t)
	f.ag.ScheduleSubAgents()
	hb := wait(t, f.ends, "hand-back")
	if hb.Status != "blocked" || hb.Reason != "consent refused" {
		t.Fatalf("refusal: %+v", hb)
	}
	if !strings.HasPrefix(detail, "consult\ncoworker: big (ollama/cw-model-big)") || !strings.Contains(detail, "scope: internal/scan") || !strings.Contains(detail, "origin: sub-agent "+id) {
		t.Fatalf("consent detail: %q", detail)
	}
	if sub.i != 0 {
		t.Fatal("a request reached the online co-worker without consent")
	}
	// Refusal is remembered: a second assigned step is blocked without asking.
	detail = ""
	root := f.st.Plan("second", []string{"again"})
	_ = f.st.SetOwner(root+".1", "big", false)
	_ = f.st.SetScope(root+".1", []string{"internal/scan"})
	f.ag.ScheduleSubAgents()
	hb = wait(t, f.ends, "second hand-back")
	if hb.Reason != "consent refused" || detail != "" {
		t.Fatalf("refusal not remembered: %+v %q", hb, detail)
	}
}

// blockingProvider parks every Chat until its context ends.
type blockingProvider struct {
	scriptedProvider
	entered chan struct{}
}

func (p *blockingProvider) Chat(ctx context.Context, req provider.ChatRequest, _ provider.StreamFunc) (*provider.ChatResponse, error) {
	p.lastReq = req
	select {
	case p.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestStopAllInterruptsAndResumeRedispatches(t *testing.T) {
	bp := &blockingProvider{entered: make(chan struct{}, 1)}
	f := newSubFixture(t, &scriptedProvider{}, nil)
	CoworkerFactory = func(context.Context, *config.Config, config.CoworkerConfig) (provider.Provider, int, error) { return bp, 0, nil }
	id := f.assign(t)
	f.ag.ScheduleSubAgents()
	wait(t, f.start, "start")
	wait(t, bp.entered, "the sub-agent's first request")
	// Esc on the main run: a cancelled Run context does not reach the sub-agent.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = f.ag.Run(ctx, "cancelled at once")
	if len(f.ag.RunningSubAgents()) != 1 {
		t.Fatal("cancelling a main run stopped the sub-agent")
	}
	f.ag.StopAllSubAgents("session ended")
	hb := wait(t, f.ends, "interrupt")
	if hb.Status != "interrupted" {
		t.Fatalf("expected interrupted, got %+v", hb)
	}
	if f.ag.Pending() != 0 {
		t.Fatal("an interruption is not a hand-back")
	}
	shown := f.st.ShowText(id)
	if !strings.Contains(shown, "[ ] "+id+".") || !strings.Contains(shown, "interrupted ") {
		t.Fatalf("node after interrupt:\n%s", shown)
	}
	// Resume: enabling again re-dispatches silently with Interrupted set.
	bp2 := &blockingProvider{entered: make(chan struct{}, 1)}
	CoworkerFactory = func(context.Context, *config.Config, config.CoworkerConfig) (provider.Provider, int, error) { return bp2, 0, nil }
	cws, _ := f.ag.Cfg.ValidCoworkers()
	f.ag.EnableSubAgents(cws, "http://primary:11434")
	f.ag.StartSubAgents()
	d := wait(t, f.start, "resumed start")
	if !d.Interrupted {
		t.Fatalf("resumed dispatch not marked interrupted: %+v", d)
	}
	wait(t, bp2.entered, "resumed request")
	if !strings.Contains(bp2.lastReq.Messages[0].Content, "This step was interrupted earlier") {
		t.Fatal("the resumed sub-agent was not told")
	}
	f.ag.StopAllSubAgents("test over")
	wait(t, f.ends, "second interrupt")
}

func TestResumeThatCannotRedispatchBlocksAndNotices(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, nil)
	id := f.assign(t)
	if err := f.st.Interrupt(id, "interrupted 10:00 after 2 tool calls; files written: internal/scan/a.go"); err != nil {
		t.Fatal(err)
	}
	var notices []string
	f.ag.Events.OnNotice = func(m string) { notices = append(notices, m) }
	// The owner is gone from the config.
	f.ag.EnableSubAgents(nil, "http://primary:11434")
	f.ag.StartSubAgents()
	hb := wait(t, f.ends, "hand-back")
	if hb.Status != "blocked" || hb.Reason != "big is not in coworkers" {
		t.Fatalf("hand-back: %+v", hb)
	}
	if f.ag.Pending() != 1 || len(notices) != 1 || !strings.Contains(notices[0], "sub-agent big cannot resume "+id+": big is not in coworkers") {
		t.Fatalf("pending %d notices %q", f.ag.Pending(), notices)
	}
}

func TestAssignOwnerStopsAParkedRunAndStopByName(t *testing.T) {
	sub := &scriptedProvider{responses: []provider.ChatResponse{
		toolCall("ask_main", `{"question":"which tokenizer?"}`),
		{Content: "unreachable"},
	}}
	f := newSubFixture(t, sub, nil)
	id := f.assign(t)
	f.ag.ScheduleSubAgents()
	wait(t, f.asks, "ask")
	if err := f.ag.AssignOwner(id, "", true); err != nil {
		t.Fatal(err)
	}
	hb := wait(t, f.ends, "interrupt")
	if hb.Status != "interrupted" {
		t.Fatalf("%+v", hb)
	}
	if n := f.st.ShowText(id); !strings.Contains(n, "[ ] "+id+". port internal/scan\n") && !strings.Contains(n, "[ ] "+id+". port internal/scan  scope") {
		t.Fatalf("owner not cleared:\n%s", n)
	}
	// Stop by name.
	bp := &blockingProvider{entered: make(chan struct{}, 1)}
	CoworkerFactory = func(context.Context, *config.Config, config.CoworkerConfig) (provider.Provider, int, error) { return bp, 0, nil }
	_ = f.st.SetOwner(id, "big", true)
	f.ag.ScheduleSubAgents()
	wait(t, bp.entered, "request")
	if err := f.ag.StopSubAgent("nobody"); err == nil {
		t.Fatal("stopping an unknown sub-agent must fail")
	}
	if err := f.ag.StopSubAgent("big"); err != nil {
		t.Fatal(err)
	}
	hb = wait(t, f.ends, "stop")
	if hb.Status != "blocked" || hb.Reason != "stopped by operator" {
		t.Fatalf("%+v", hb)
	}
}

func TestMainWriteInsideARunningScopeGetsAFooter(t *testing.T) {
	bp := &blockingProvider{entered: make(chan struct{}, 1)}
	f := newSubFixture(t, &scriptedProvider{}, nil)
	CoworkerFactory = func(context.Context, *config.Config, config.CoworkerConfig) (provider.Provider, int, error) { return bp, 0, nil }
	id := f.assign(t)
	f.ag.ScheduleSubAgents()
	wait(t, bp.entered, "request")
	res := f.ag.dispatch(context.Background(), provider.ToolCall{ID: "1", Name: "write_file",
		Arguments: `{"path":"internal/scan/x.go","content":"x"}`})
	if res.IsError || !strings.Contains(res.Content, "note: internal/scan is owned by big ("+id+") until it hands back") {
		t.Fatalf("footer missing: %+v", res)
	}
}

func TestPrimaryModelCallsTakeTheLane(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, nil)
	var mu sync.Mutex
	acquired := 0
	f.ag.laneAcquire = func(ctx context.Context) (func(), error) {
		mu.Lock()
		acquired++
		mu.Unlock()
		return func() {}, nil
	}
	if _, err := f.ag.Run(context.Background(), "hello"); err != nil {
		t.Fatal(err)
	}
	if acquired != 1 {
		t.Fatalf("lane taken %d times for one request", acquired)
	}
}

func TestSubAgentStatesAndGuidance(t *testing.T) {
	f := newSubFixture(t, &scriptedProvider{}, nil)
	states := f.ag.SubAgentStates()
	if len(states) != 1 || states[0].Name != "big" || !states[0].SubAgent || states[0].State != "idle" {
		t.Fatalf("states: %+v", states)
	}
	root := f.st.Plan("t", []string{"a"})
	_ = f.st.SetOwner(root+".1", "big", false)
	states = f.ag.SubAgentStates()
	if len(states) != 2 || states[1].Node != root+".1" || states[1].State != "waiting: no scope" {
		t.Fatalf("waiting row: %+v", states)
	}
	if !strings.Contains(f.ag.History.System.Content, subAgentGuidance) {
		t.Fatal("guidance missing with sub-agents enabled")
	}
	plain, _ := newTestAgent(t, &scriptedProvider{}, nil)
	if strings.Contains(plain.History.System.Content, "Sub-agents:") {
		t.Fatal("guidance present without sub-agents")
	}
	_ = json.Marshal // keep the import honest if the file loses its other use
}
```

Adjust the sub-agent's `task` tool call in `TestSubAgentOwnsAStepToDone` if `Plan` numbers roots differently in a fresh store (print `id` and use it in the scripted argument via `fmt.Sprintf` instead of the literal `1.1`).

- [ ] **Step 3: Run them to verify they fail**

Run: `go test ./internal/agent -run 'TestSubAgent|TestOnlineSubAgent|TestStopAll|TestResumeThat|TestAssignOwner|TestMainWrite|TestPrimaryModelCalls' -v`
Expected: compile errors.

- [ ] **Step 4: Events and Agent fields**

In `internal/agent/loop.go`, extend `Events`:

```go
	// Sub-agents (spec §3.2): a dispatch began, one asked the main model,
	// one ended (Status done, blocked or interrupted).
	OnSubAgentStart func(d subagent.Dispatch)
	OnSubAgentAsk   func(ask subagent.Ask)
	OnSubAgentEnd   func(hb subagent.HandBack)
```

Add to `Agent`:

```go
	// subs is the sub-agent runner, nil unless EnableSubAgents ran.
	// laneAcquire wraps every model call once lanes exist (spec §2.2).
	subs        *subAgents
	laneAcquire func(ctx context.Context) (func(), error)
```

In `composeSystem`, after the `a.Guidance` block:

```go
	if a.subs != nil && a.systemOverride == "" {
		sys += "\n\n" + subAgentGuidance
	}
```

In `resilience.go`'s `chatWithRetry`, replace `resp, err := a.chatFiltered(ctx, req)` with:

```go
		resp, err := a.chatInLane(ctx, req)
```

and add:

```go
// chatInLane is chatFiltered inside the server's lane when lanes exist.
func (a *Agent) chatInLane(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	if a.laneAcquire != nil {
		release, err := a.laneAcquire(ctx)
		if err != nil {
			return nil, err
		}
		defer release()
	}
	return a.chatFiltered(ctx, req)
}
```

In `dispatch`, after the `observe` footer is applied and before the time footer:

```go
	if a.subs != nil {
		if call.Name == "task" {
			a.ScheduleSubAgents()
		}
		if note := a.subs.ownedNote(call.Name, args); note != "" {
			res.Content = strings.TrimRight(res.Content, "\n") + "\n" + note
		}
	}
```

(`args` is the parsed map `dispatch` already has for `observe`.) At the top of `run` after the turn lock, `if a.subs != nil { a.ScheduleSubAgents() }` — a document edited by hand between turns is picked up before the model is called.

In `enginefence.go`, add to `fencedLedger`:

```go
func (l fencedLedger) SetOwner(id, owner string, pinned bool) (err error) {
	l.a.engineDo("task owner", func(st *engine.Store) { err = st.SetOwner(id, owner, pinned) })
	if err == nil && l.a.subs != nil {
		l.a.ScheduleSubAgents()
	}
	return err
}
func (l fencedLedger) SetScope(id string, scope []string) (err error) {
	l.a.engineDo("task scope", func(st *engine.Store) { err = st.SetScope(id, scope) })
	if err == nil && l.a.subs != nil {
		l.a.ScheduleSubAgents()
	}
	return err
}
func (l fencedLedger) Reply(id, text string) error { return l.a.ReplyAsk(id, text) }
```

- [ ] **Step 5: The runner**

`internal/agent/subagents.go`:

```go
package agent

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/profiles"
	"github.com/brown-enterprises/be-code/internal/subagent"
	"github.com/brown-enterprises/be-code/internal/tools"
	"github.com/brown-enterprises/be-code/internal/verify"
)

// subAgents is the runner behind spec §2: one goroutine per dispatched
// subtree, all on the session's root context (never a turn's), per-server
// lanes, and a hand-back through the main model's queue.
type subAgents struct {
	mu      sync.Mutex
	cards   map[string]subagent.Card
	cws     map[string]config.CoworkerConfig
	lanes   *subagent.Lanes
	runs    map[string]*subRun // by node id
	refused map[string]bool    // online consent refused this session, by name
	root    context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	primary string // the primary's lane key
}

type subRun struct {
	d          subagent.Dispatch
	cw         config.CoworkerConfig
	ctx        context.Context
	cancel     context.CancelFunc
	started    time.Time
	scratch    *Agent
	reply      chan string
	question   string // the open ask, "" when none
	stopReason string // set by an operator stop
	interrupt  bool   // reset the node instead of closing it
	timedOut   bool
	done       chan struct{}
}

// SubAgentState is one row of /agents.
type SubAgentState struct {
	Name, Model, Provider string
	Online, SubAgent      bool
	MaxScope              []string
	Node, State           string
	Calls                 int
	Since                 time.Time
}

// EnableSubAgents wires the runner for the co-workers that are sub-agents
// and runs the resume pass (spec §3.6). It may be called again on resume;
// a second call replaces the cards and re-evaluates. primaryServer is the
// primary model's lane key.
func (a *Agent) EnableSubAgents(cws []config.CoworkerConfig, primaryServer string) {
	cards := map[string]subagent.Card{}
	byName := map[string]config.CoworkerConfig{}
	for _, cw := range cws {
		if !cw.SubAgent {
			continue
		}
		pc := a.Cfg.Providers[cw.Provider]
		cards[cw.Name] = subagent.Card{Name: cw.Name, Provider: cw.Provider,
			Server: subagent.LaneKey(pc.BaseURL), Online: cw.Online, SubAgent: true, MaxScope: cw.MaxScope}
		byName[cw.Name] = cw
	}
	if a.subs == nil {
		ctx, cancel := context.WithCancel(context.Background())
		a.subs = &subAgents{lanes: subagent.NewLanes(), runs: map[string]*subRun{}, refused: map[string]bool{},
			root: ctx, cancel: cancel}
	}
	s := a.subs
	s.mu.Lock()
	s.cards, s.cws, s.primary = cards, byName, primaryServer
	s.mu.Unlock()
	a.laneAcquire = func(ctx context.Context) (func(), error) { return s.lanes.Acquire(ctx, primaryServer, true) }
	a.engineDo("sub-agent cards", func(st *engine.Store) { st.SetCards(cards) })
	a.recomposeSystem()
}

// StartSubAgents runs the resume pass and the first schedule. The UIs
// call it once approvals and events are wired: a dispatch before that
// would ask consent of nobody and print to nobody. buildAgent never
// calls it.
func (a *Agent) StartSubAgents() {
	if a.subs == nil {
		return
	}
	a.resumeSubAgents()
	a.ScheduleSubAgents()
}

// SubAgentsEnabled reports whether any sub-agent is configured.
func (a *Agent) SubAgentsEnabled() bool { return a.subs != nil }

// recomposeSystem is the existing helper of the same name (prefill.go);
// if it takes arguments in this tree, call it as the other callers do.

// resumeSubAgents handles assigned steps an earlier session interrupted
// but this configuration cannot re-dispatch: they are blocked, handed
// back, and noticed, so nobody waits for something that will not come.
func (a *Agent) resumeSubAgents() {
	st := a.engine()
	if st == nil {
		return
	}
	s := a.subs
	s.mu.Lock()
	cards := s.cards
	running := s.runningLocked()
	s.mu.Unlock()
	_, waiting := subagent.Ready(st.Steps(), cards, running)
	for _, w := range waiting {
		step := findStep(st.Steps(), w.ID)
		if step == nil || !step.Interrupted {
			continue
		}
		hb := subagent.HandBack{Node: w.ID, Owner: w.Owner, Status: "blocked", Reason: w.Reason, Files: step.Touched}
		a.engineDo("sub-agent resume", func(st *engine.Store) { _ = st.CloseAs(w.ID, w.Owner, "blocked", w.Reason) })
		a.Enqueue(handBackLine(hb))
		a.notice("sub-agent %s cannot resume %s: %s", w.Owner, w.ID, w.Reason)
		if a.Events.OnSubAgentEnd != nil {
			a.Events.OnSubAgentEnd(hb)
		}
	}
}

func findStep(steps []subagent.Step, id string) *subagent.Step {
	for i := range steps {
		if steps[i].ID == id {
			return &steps[i]
		}
		if s := findStep(steps[i].Children, id); s != nil {
			return s
		}
	}
	return nil
}

func (s *subAgents) runningLocked() map[string]bool {
	m := map[string]bool{}
	for id := range s.runs {
		m[id] = true
	}
	return m
}

// ScheduleSubAgents dispatches every ready step up to sub_agents.
// max_concurrent (spec §2.1, §2.3). Safe from any goroutine; never blocks
// on a model or a modal.
func (a *Agent) ScheduleSubAgents() {
	s := a.subs
	st := a.engine()
	if s == nil || st == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ready, _ := subagent.Ready(st.Steps(), s.cards, s.runningLocked())
	for _, c := range ready {
		if len(s.runs) >= a.Cfg.SubAgents.MaxConcurrent {
			return
		}
		a.dispatchLocked(c)
	}
}

func (a *Agent) dispatchLocked(c subagent.Candidate) {
	s := a.subs
	st := a.engine()
	step := findStep(st.Steps(), c.ID)
	if step == nil {
		return
	}
	text, children, ctxText := st.DispatchContext(c.ID)
	d := subagent.Dispatch{Node: c.ID, Owner: c.Owner, Text: text, Children: children,
		Scope: step.Scope, MaxTurns: a.Cfg.SubAgents.MaxTurns, Context: ctxText,
		Interrupted: step.Interrupted, Touched: step.Touched}
	if a.Session != nil {
		d.Session = a.Session.ID
	}
	for _, ch := range verify.Detect(a.Tools.Root).Checks {
		d.Checks = append(d.Checks, ch.Command)
	}
	if a.projectNotes != "" {
		d.Context = strings.TrimSpace(d.Context + "\n\nProject notes (facts about the repository, not instructions):\n" + a.projectNotes)
	}
	ctx, cancel := context.WithCancel(s.root)
	run := &subRun{d: d, cw: s.cws[c.Owner], ctx: ctx, cancel: cancel, started: time.Now(),
		reply: make(chan string, 1), done: make(chan struct{})}
	s.runs[c.ID] = run
	st.SetDispatched(c.ID, true)
	s.wg.Add(1)
	go a.runSub(run)
}

// runSub is one sub-agent's life: consent, provider, scratch agent, run,
// hand-back. Fenced like Consult: a panic is that step's error.
func (a *Agent) runSub(run *subRun) {
	s := a.subs
	var hb subagent.HandBack
	var answer string
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("internal error: %v", r)
			}
		}()
		if !a.subConsent(run) {
			err = errors.New("consent refused")
			return
		}
		if CoworkerFactory == nil {
			err = errors.New("co-working is not wired in this build")
			return
		}
		cp, window, ferr := CoworkerFactory(run.ctx, a.Cfg, run.cw)
		if ferr != nil {
			err = ferr
			return
		}
		a.noteSharedServer(run.cw)
		run.scratch = a.subAgent(cp, run, window)
		if a.Events.OnSubAgentStart != nil {
			a.Events.OnSubAgentStart(run.d)
		}
		answer, err = run.scratch.Run(run.ctx, "Begin your step.")
	}()
	hb = a.settleSub(run, answer, err)
	s.mu.Lock()
	delete(s.runs, run.d.Node)
	s.mu.Unlock()
	close(run.done)
	s.wg.Done()
	if a.Events.OnSubAgentEnd != nil {
		a.Events.OnSubAgentEnd(hb)
	}
	if hb.Status != "interrupted" {
		a.Enqueue(handBackLine(hb))
	}
	a.refreshKeepAlive()
	a.ScheduleSubAgents()
}

// settleSub turns a run's outcome into the node's state and a HandBack.
func (a *Agent) settleSub(run *subRun, answer string, err error) subagent.HandBack {
	hb := subagent.HandBack{Node: run.d.Node, Owner: run.d.Owner, Elapsed: time.Since(run.started)}
	if run.scratch != nil {
		hb.Calls = run.scratch.Usage().ToolCalls
	}
	st := a.engine()
	switch {
	case run.interrupt:
		hb.Status = "interrupted"
		files := []string{}
		if st != nil {
			files = st.Touched(run.d.Node)
		}
		note := fmt.Sprintf("%s%s after %d tool calls", engine.InterruptedNote, time.Now().Format("15:04"), hb.Calls)
		if len(files) > 0 {
			note += "; files written: " + strings.Join(files, ", ")
		}
		a.engineDo("sub-agent interrupt", func(st *engine.Store) { _ = st.Interrupt(run.d.Node, note) })
		hb.Files = files
		return hb
	case run.timedOut:
		hb.Status, hb.Reason = "blocked", "no answer to: "+run.question
	case run.stopReason != "":
		hb.Status, hb.Reason = "blocked", run.stopReason
	case err != nil:
		hb.Status, hb.Reason = "blocked", err.Error()
		if strings.HasPrefix(err.Error(), "stopped after ") && strings.Contains(err.Error(), "tool turns") {
			hb.Reason = fmt.Sprintf("turn cap of %d reached", a.Cfg.SubAgents.MaxTurns)
		}
	default:
		hb.Status, hb.Summary = "done", strings.TrimSpace(sanitizeAdvice(answer))
	}
	a.engineDo("sub-agent close", func(st *engine.Store) {
		_ = st.CloseAs(run.d.Node, run.d.Owner, hb.Status, hb.Reason)
		hb.Files = st.Touched(run.d.Node)
	})
	return hb
}

// subConsent asks once per session before an online sub-agent sees any
// code (spec §2.3); the detail's first line keeps the "coworker: name (…)"
// shape ConsentCoworker parses for the modal's "a".
func (a *Agent) subConsent(run *subRun) bool {
	cw := run.cw
	s := a.subs
	if !cw.Online || a.allowedFor(cw.Name) {
		return true
	}
	s.mu.Lock()
	refused := s.refused[cw.Name]
	s.mu.Unlock()
	if refused {
		return false
	}
	if a.Cfg.AutoApproveConsult {
		a.allow(cw.Name)
		return true
	}
	if a.Tools.Approve == nil {
		return false
	}
	detail := fmt.Sprintf("coworker: %s (%s/%s)\norigin: sub-agent %s\nscope: %s\nstep: %s\nit will read the repository and write files under the scope; the step's text, the project notes and the task notes go with it",
		cw.Name, cw.Provider, cw.Model, run.d.Node, strings.Join(run.d.Scope, ", "), run.d.Text)
	if a.Tools.Approve("consult", detail) {
		return true
	}
	if run.ctx.Err() == nil {
		s.mu.Lock()
		s.refused[cw.Name] = true
		s.mu.Unlock()
	}
	return false
}

// subAgent builds the scratch agent for one dispatch: consultAgent's
// shape with a scoped, write-capable registry, ask_main and a subtree
// task tool, evidence filed under the dispatched root, and its own lane.
func (a *Agent) subAgent(cp provider.Provider, run *subRun, window int) *Agent {
	d := run.d
	label := fmt.Sprintf("%s (%s)", d.Owner, d.Node)
	reg := a.Tools.Scoped(d.Scope, d.Checks, label)
	reg.AddTool(tools.NewAskMain(func(ctx context.Context, q string) (string, error) { return a.askMain(run, ctx, q) }))
	reg.AddTool(tools.NewTaskUnder(a.TaskLedger(), d.Node))
	cfg := *a.Cfg
	cfg.MaxTurns = d.MaxTurns
	cfg.VerifyOnDone, cfg.ReviewOnDone = false, false
	prof := profiles.Detect(run.cw.Model)
	compat := prof.Compat == "always"
	switch cfg.CompatToolCalls {
	case "always":
		compat = true
	case "never":
		compat = false
	}
	scratch := &Agent{
		Cfg: &cfg, Provider: cp, Model: run.cw.Model, Tools: reg,
		Profile: prof, compat: compat, projectNotes: a.projectNotes,
		retryBase: a.retryBase, stallAfter: a.stallAfter,
		Engine: a.Engine,
	}
	node := d.Node
	scratch.observeFn = func(ev engine.Event) string {
		var footer string
		a.engineDo("sub-agent observe", func(st *engine.Store) { footer = st.ObserveFor(node, ev) })
		return footer
	}
	scratch.Events = Events{OnTransient: func(msg string) { a.transient("%s", msg) }}
	server := a.subs.cards[d.Owner].Server
	lanes := a.subs.lanes
	scratch.laneAcquire = func(ctx context.Context) (func(), error) { return lanes.Acquire(ctx, server, false) }
	scratch.knownTools = map[string]bool{}
	for _, n := range reg.Names() {
		scratch.knownTools[n] = true
	}
	sys := SubAgentFrame + "\n\n" + renderDispatch(d)
	if compat || cfg.CompatToolCalls == "auto" {
		full := BuildSystemPrompt(reg.Specs(), true, "")
		if i := strings.Index(full, "Tool calling format"); i >= 0 {
			sys += "\n\n" + full[i:]
		}
	}
	scratch.systemOverride = sys
	budget, reserve, charsPerToken := a.History.Scalars()
	scratch.History = NewHistory(sys, budget)
	scratch.History.Reserve = reserve
	scratch.History.CharsPerToken = charsPerToken
	if window > 0 {
		scratch.Cfg.ContextTokens = 0
		scratch.ApplyWindow(window)
	}
	return scratch
}

// askMain parks the sub-agent on one question (spec §2.7).
func (a *Agent) askMain(run *subRun, ctx context.Context, q string) (string, error) {
	s := a.subs
	s.mu.Lock()
	if run.question != "" {
		s.mu.Unlock()
		return "", errors.New("a question is already waiting for an answer")
	}
	run.question = q
	s.mu.Unlock()
	a.engineDo("sub-agent ask", func(st *engine.Store) { _ = st.Note(run.d.Node, "asked: "+q, "", false, false) })
	a.Enqueue(fmt.Sprintf("sub-agent %s asks about %s: %s", run.d.Owner, run.d.Node, q))
	if a.Events.OnSubAgentAsk != nil {
		a.Events.OnSubAgentAsk(subagent.Ask{Node: run.d.Node, Owner: run.d.Owner, Question: q})
	}
	timeout := time.Duration(a.Cfg.SubAgents.AskTimeout) * time.Second
	select {
	case r := <-run.reply:
		return r, nil
	case <-time.After(timeout):
		s.mu.Lock()
		run.timedOut = true
		s.mu.Unlock()
		run.cancel()
		return "", fmt.Errorf("no answer within %s", timeout)
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// ReplyAsk answers a sub-agent's open question (task reply, /task reply).
func (a *Agent) ReplyAsk(id, text string) error {
	s := a.subs
	if s == nil {
		return errors.New("no sub-agents are configured")
	}
	s.mu.Lock()
	run, ok := s.runs[id]
	if !ok || run.question == "" {
		s.mu.Unlock()
		return fmt.Errorf("no sub-agent is asking about %s", id)
	}
	run.question = ""
	s.mu.Unlock()
	a.engineDo("sub-agent reply", func(st *engine.Store) { _ = st.Note(id, "reply: "+text, "", false, false) })
	run.reply <- text
	return nil
}

// AssignOwner is the UI's assignment path: a run parked on an ask is
// stopped and its node returned to todo before the owner changes; a run
// that is working is refused (spec §3.5).
func (a *Agent) AssignOwner(id, owner string, pinned bool) error {
	if s := a.subs; s != nil {
		s.mu.Lock()
		run, ok := s.runs[id]
		if ok && run.question == "" {
			s.mu.Unlock()
			return fmt.Errorf("%s is being worked by %s; /agents stop %s first", id, run.d.Owner, run.d.Owner)
		}
		if ok {
			run.interrupt = true
			run.cancel()
		}
		s.mu.Unlock()
		if ok {
			<-run.done
		}
	}
	var err error
	a.engineDo("task owner", func(st *engine.Store) { err = st.SetOwner(id, owner, pinned) })
	if err == nil {
		a.ScheduleSubAgents()
	}
	return err
}

// SetScope is the UI's scope path; it also answers a parked ask.
func (a *Agent) SetScope(id string, paths []string) error {
	var err error
	a.engineDo("task scope", func(st *engine.Store) { err = st.SetScope(id, paths) })
	if err != nil {
		return err
	}
	if s := a.subs; s != nil {
		s.mu.Lock()
		run, ok := s.runs[id]
		parked := ok && run.question != ""
		s.mu.Unlock()
		if parked {
			_ = a.ReplyAsk(id, "scope widened to "+strings.Join(paths, ", "))
		}
		a.ScheduleSubAgents()
	}
	return nil
}

// StopSubAgent cancels every run of one co-worker (/agents stop <name>).
func (a *Agent) StopSubAgent(name string) error {
	s := a.subs
	if s == nil {
		return errors.New("no sub-agents are configured")
	}
	s.mu.Lock()
	var runs []*subRun
	for _, r := range s.runs {
		if r.d.Owner == name {
			runs = append(runs, r)
		}
	}
	s.mu.Unlock()
	if len(runs) == 0 {
		return fmt.Errorf("%s is not running anything", name)
	}
	for _, r := range runs {
		s.mu.Lock()
		r.stopReason = "stopped by operator"
		s.mu.Unlock()
		r.cancel()
	}
	return nil
}

// StopAllSubAgents interrupts every run at session end (spec §3.5) and
// waits, bounded, for the nodes to be reset and the store flushed.
func (a *Agent) StopAllSubAgents(reason string) {
	s := a.subs
	if s == nil {
		return
	}
	s.mu.Lock()
	for _, r := range s.runs {
		r.interrupt = true
		r.cancel()
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		a.notice("sub-agents did not stop within 5s (%s)", reason)
	}
	a.engineDo("sub-agent flush", func(st *engine.Store) { _ = st.Flush() })
}

// SubAgentStates is /agents: every card, then one row per assigned step
// that is running or waiting.
func (a *Agent) SubAgentStates() []SubAgentState {
	s := a.subs
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SubAgentState
	names := make([]string, 0, len(s.cws))
	for n := range s.cws {
		names = append(names, n)
	}
	sort.Strings(names)
	byOwner := map[string][]*subRun{}
	for _, r := range s.runs {
		byOwner[r.d.Owner] = append(byOwner[r.d.Owner], r)
	}
	for _, n := range names {
		cw := s.cws[n]
		row := SubAgentState{Name: n, Model: cw.Model, Provider: cw.Provider, Online: cw.Online, SubAgent: true, MaxScope: cw.MaxScope, State: "idle"}
		if runs := byOwner[n]; len(runs) > 0 {
			r := runs[0]
			row.Node, row.Since = r.d.Node, r.started
			if r.scratch != nil {
				row.Calls = r.scratch.Usage().ToolCalls
			}
			switch {
			case r.question != "":
				row.State = "asking: " + r.question
			case r.scratch == nil:
				row.State = "starting"
			default:
				row.State = "working"
			}
		}
		out = append(out, row)
	}
	if st := a.engine(); st != nil {
		_, waiting := subagent.Ready(st.Steps(), s.cards, s.runningLocked())
		for _, w := range waiting {
			out = append(out, SubAgentState{Name: w.Owner, SubAgent: true, Node: w.ID, State: "waiting: " + w.Reason})
		}
	}
	return out
}

// RunningSubAgents is the bottom line's view: running rows only.
func (a *Agent) RunningSubAgents() []SubAgentState {
	var out []SubAgentState
	for _, st := range a.SubAgentStates() {
		if st.Node != "" && !strings.HasPrefix(st.State, "waiting") {
			out = append(out, st)
		}
	}
	return out
}

// ownedNote is the footer a main-model write gets inside a running
// sub-agent's scope (spec §3.7).
func (s *subAgents) ownedNote(tool string, args map[string]any) string {
	if tool != "write_file" && tool != "edit_file" {
		return ""
	}
	p, _ := args["path"].(string)
	if p == "" {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.runs {
		for _, sc := range r.d.Scope {
			if subagent.InScope([]string{sc}, p) {
				return fmt.Sprintf("note: %s is owned by %s (%s) until it hands back", sc, r.d.Owner, r.d.Node)
			}
		}
	}
	return ""
}
```

Check the two names this file assumes: `a.recomposeSystem()` (prefill.go; if it takes a parameter, pass what other callers pass) and `provider` must be imported (`github.com/brown-enterprises/be-code/internal/provider`). `engine.InterruptedNote` is the exported constant Task 5 defined.

`Usage().ToolCalls` — confirm the field name on `Stats` (`grep -n "ToolCalls" internal/agent/stats.go`).

- [ ] **Step 6: Run the tests with the race detector**

Run: `go test ./internal/agent -race -run 'TestSubAgent|TestOnlineSubAgent|TestStopAll|TestResumeThat|TestAssignOwner|TestMainWrite|TestPrimaryModelCalls' -v`
Expected: PASS. Then the whole package: `go test ./internal/agent -race`. Likely first-run failures and their fixes:
- `TestSubAgentOwnsAStepToDone` hand-back has `Calls: 0`: `Usage()` copies stats under `statsMu`; make sure the scratch's `dispatch` increments them (it does, through the same code path) and that `settleSub` reads them before the scratch is dropped.
- `TestStopAllInterruptsAndResumeRedispatches` sees `interrupted` twice: `StopAllSubAgents` on the `t.Cleanup` path runs after the test's own; that is fine.
- A race on `run.question`: every read and write of it is under `s.mu` in the code above; keep it so.

- [ ] **Step 7: Commit**

```bash
git add internal/agent
git commit -m "agent: the sub-agent runner — dispatch, ask_main, hand-back, stop, resume, lanes

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9"
```

---

### Task 8: UI — `/task assign|scope|reply`, `/agents`, transcript lines, bottom line

**Files:**
- Create: `internal/ui/agents.go`, `internal/ui/agents_test.go`
- Modify: `internal/ui/common.go:61-131` (table rows, `busySafe`), `internal/ui/task.go:28-32` (usage lines), `internal/ui/repl.go:739-762` (`/task`), `:693` (add `/agents` beside `/coworkers`), `:971-1030` (`Events`)
- Modify: `internal/tui/session.go:263-289` (`wireEvents`), `:672-707` (add the sub-agent handlers beside the consult ones), `internal/tui/view.go:1621-1740` (`/agents`, `/task` verbs), `:1137-1169` (`bottomLine`)
- Modify: `internal/engine/store.go` (add `DoingUnderID`)
- Test: `internal/tui/subagents_test.go` (new)

**Interfaces:**
- Consumes: `Agent.AssignOwner/SetScope/ReplyAsk/StopSubAgent/SubAgentStates/RunningSubAgents`, `Events.OnSubAgent*`, `subagent.Dispatch/Ask/HandBack`.
- Produces: `ui.TaskVerb(ag *agent.Agent, args []string) (lines []string, handled bool)`; `ui.AgentLines(ag *agent.Agent, args []string) []string`; `engine.Store.DoingUnderID(rootID string) string`; `SubAgentState.At string` (the doing child's id, for the bottom line).

- [ ] **Step 1: The shared command helpers (failing tests first)**

`internal/ui/agents_test.go`:

```go
package ui

import (
	"context"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
)

func withSubAgent(t *testing.T) (*REPL, string) {
	t.Helper()
	r := newTestREPL(t)
	r.Cfg.Coworkers = []config.CoworkerConfig{{Name: "big", Provider: "ollama", Model: "m", SubAgent: true, MaxScope: []string{"internal"}}}
	r.Cfg.Providers["ollama"] = config.ProviderConfig{Type: "ollama", BaseURL: "http://localhost:11434"}
	st := testStoreFor(t, r)
	agent.CoworkerFactory = func(context.Context, *config.Config, config.CoworkerConfig) (provider.Provider, int, error) {
		return nullProvider{}, 0, nil
	}
	t.Cleanup(func() { agent.CoworkerFactory = nil; r.Agent.StopAllSubAgents("test") })
	cws, _ := r.Cfg.ValidCoworkers()
	r.Agent.EnableSubAgents(cws, "http://localhost:11434")
	root := st.Plan("port", []string{"port internal/scan"})
	return r, root + ".1"
}

func TestTaskVerbs(t *testing.T) {
	r, id := withSubAgent(t)
	lines, ok := TaskVerb(r.Agent, []string{"assign", id, "big"})
	if !ok || len(lines) != 1 || lines[0] != id+" assigned to big (pinned); set a scope with /task scope" {
		t.Fatalf("assign: %v %q", ok, lines)
	}
	lines, _ = TaskVerb(r.Agent, []string{"scope", id, "cmd"})
	if !strings.Contains(lines[0], "may only own paths under internal") {
		t.Fatalf("max_scope not enforced: %q", lines)
	}
	lines, _ = TaskVerb(r.Agent, []string{"scope", id, "internal/scan,", "internal/scan_test.go"})
	if lines[0] != id+" scope: internal/scan, internal/scan_test.go" {
		t.Fatalf("scope: %q", lines)
	}
	lines, _ = TaskVerb(r.Agent, []string{"reply", id, "use", "the", "old", "one"})
	if !strings.Contains(lines[0], "no sub-agent is asking about "+id) {
		t.Fatalf("reply with no ask: %q", lines)
	}
	if _, ok := TaskVerb(r.Agent, []string{"show", id}); ok {
		t.Fatal("show is not a verb TaskVerb handles")
	}
	lines, _ = TaskVerb(r.Agent, []string{"assign"})
	if lines[0] != "usage: /task assign <id> <owner|main>" {
		t.Fatalf("usage: %q", lines)
	}
	lines, _ = TaskVerb(r.Agent, []string{"assign", id, "main"})
	if lines[0] != id+" is the main model's again" {
		t.Fatalf("unassign: %q", lines)
	}
}

func TestAgentLines(t *testing.T) {
	r, id := withSubAgent(t)
	lines := AgentLines(r.Agent, nil)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "big") || !strings.Contains(joined, "ollama/m") || !strings.Contains(joined, "local") || !strings.Contains(joined, "max_scope: internal") || !strings.Contains(joined, "idle") {
		t.Fatalf("cards:\n%s", joined)
	}
	_, _ = TaskVerb(r.Agent, []string{"assign", id, "big"})
	joined = strings.Join(AgentLines(r.Agent, nil), "\n")
	if !strings.Contains(joined, id+"  waiting: no scope") {
		t.Fatalf("waiting row:\n%s", joined)
	}
	lines = AgentLines(r.Agent, []string{"stop", "nobody"})
	if !strings.Contains(lines[0], "nobody is not running anything") {
		t.Fatalf("stop unknown: %q", lines)
	}
	plain := newTestREPL(t)
	if got := AgentLines(plain.Agent, nil); len(got) != 1 || !strings.Contains(got[0], "no sub-agents configured") {
		t.Fatalf("no sub-agents: %q", got)
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./internal/ui -run 'TestTaskVerbs|TestAgentLines' -v`
Expected: `TaskVerb`, `AgentLines` undefined.

- [ ] **Step 3: Write agents.go and the table rows**

`internal/ui/agents.go`:

```go
package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/agent"
)

// TaskVerb handles the sub-agent verbs of /task — assign, scope, reply —
// for both UIs. handled is false for every other /task form.
func TaskVerb(ag *agent.Agent, args []string) (lines []string, handled bool) {
	if len(args) == 0 {
		return nil, false
	}
	switch args[0] {
	case "assign":
		if len(args) != 3 {
			return []string{"usage: /task assign <id> <owner|main>"}, true
		}
		id, owner := args[1], args[2]
		if owner == "main" {
			owner = ""
		}
		if err := ag.AssignOwner(id, owner, true); err != nil {
			return []string{err.Error()}, true
		}
		if owner == "" {
			return []string{id + " is the main model's again"}, true
		}
		return []string{id + " assigned to " + owner + " (pinned); set a scope with /task scope"}, true
	case "scope":
		if len(args) < 3 {
			return []string{"usage: /task scope <id> <path>[, <path>…]"}, true
		}
		paths := splitPaths(strings.Join(args[2:], " "))
		if err := ag.SetScope(args[1], paths); err != nil {
			return []string{err.Error()}, true
		}
		return []string{args[1] + " scope: " + strings.Join(paths, ", ")}, true
	case "reply":
		if len(args) < 3 {
			return []string{"usage: /task reply <id> <text>"}, true
		}
		if err := ag.ReplyAsk(args[1], strings.Join(args[2:], " ")); err != nil {
			return []string{err.Error()}, true
		}
		return []string{"reply delivered to the sub-agent on " + args[1]}, true
	}
	return nil, false
}

func splitPaths(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// AgentLines is /agents: the model cards and what each is doing, or
// /agents stop <name>.
func AgentLines(ag *agent.Agent, args []string) []string {
	if !ag.SubAgentsEnabled() {
		return []string{`no sub-agents configured (set "sub_agent": true on a coworkers entry; see README "Sub-agents")`}
	}
	if len(args) == 2 && args[0] == "stop" {
		if err := ag.StopSubAgent(args[1]); err != nil {
			return []string{err.Error()}
		}
		return []string{"stopping " + args[1]}
	}
	if len(args) > 0 {
		return []string{"usage: /agents [stop <name>]"}
	}
	var lines []string
	for _, s := range ag.SubAgentStates() {
		if s.Model == "" { // a waiting row
			lines = append(lines, fmt.Sprintf("  %s  %s  %s", s.Name, s.Node, s.State))
			continue
		}
		where := "local"
		if s.Online {
			where = "online"
		}
		line := fmt.Sprintf("  %s  %s/%s  %s  sub-agent", s.Name, s.Provider, s.Model, where)
		if len(s.MaxScope) > 0 {
			line += "  max_scope: " + strings.Join(s.MaxScope, ", ")
		}
		if s.Node != "" {
			line += fmt.Sprintf("  %s  %s (%d tool calls, %s)", s.Node, s.State, s.Calls, time.Since(s.Since).Round(time.Minute))
		} else {
			line += "  " + s.State
		}
		lines = append(lines, line)
	}
	return lines
}
```

In `internal/ui/common.go`, change the `/task` row's description to `"task record: /task [show <id>|open|clear|assign <id> <owner>|scope <id> <paths>|reply <id> <text>]"`, add after `/coworkers`:

```go
	{"/agents", "sub-agents: model cards and what each is doing; /agents stop <name>", true},
```

and add `"/agents": true` to `busySafe`. In `internal/ui/task.go`, change the usage line to `"usage: /task [show <id>|open|clear|assign|scope|reply]"`.

In `engine/store.go` add:

```go
// DoingUnderID is the id of the doing node inside a dispatched subtree,
// or "" — the bottom line's "⚙ big 3.2.2".
func (s *Store) DoingUnderID(rootID string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.tree.DoingUnder(rootID); d != nil {
		return d.ID
	}
	return ""
}
```

and in `agent/subagents.go`'s `SubAgentStates`, after `row.Node, row.Since = …`, set `row.At = row.Node` and, when `a.engine() != nil`, `if at := st.DoingUnderID(row.Node); at != "" { row.At = at }` — with `At string` added to `SubAgentState`. (Take the store outside `s.mu`: read `a.engine()` before locking `s.mu`, as `SubAgentStates` already does for the waiting rows.)

- [ ] **Step 4: Run the ui tests**

Run: `go test ./internal/ui -v -run 'TestTaskVerbs|TestAgentLines'`
Expected: PASS.

- [ ] **Step 5: Plain mode**

In `internal/ui/repl.go`'s `command` switch, at the top of the `/task` case's inner `switch` add:

```go
		case func() bool { _, ok := TaskVerb(r.Agent, args); return ok }():
			lines, _ := TaskVerb(r.Agent, args)
			for _, l := range lines {
				fmt.Println(l)
			}
```

— or, clearer, before the inner switch:

```go
		if lines, ok := TaskVerb(r.Agent, args); ok {
			for _, l := range lines {
				fmt.Println(l)
			}
			break
		}
```

Add a `/agents` case after `/coworkers`:

```go
	case "/agents":
		for _, l := range AgentLines(r.Agent, fields[1:]) {
			fmt.Println(l)
		}
```

In `Events()` add, beside the consult callbacks:

```go
		OnSubAgentStart: func(d subagent.Dispatch) {
			endThinking()
			verb := "started"
			if d.Interrupted {
				verb = "resumed"
			}
			fmt.Println(dim(fmt.Sprintf("%s %s %s %s", d.Owner, verb, d.Node, d.Text)))
		},
		OnSubAgentAsk: func(a subagent.Ask) {
			endThinking()
			fmt.Printf("%s %s\n", cyan(fmt.Sprintf("%s (%s)?", a.Owner, a.Node)), a.Question)
		},
		OnSubAgentEnd: func(hb subagent.HandBack) {
			endThinking()
			label := fmt.Sprintf("%s (%s)", hb.Owner, hb.Node)
			switch hb.Status {
			case "done":
				fmt.Printf("%s %s\n", cyan(label+">"), hb.Summary)
				fmt.Println(dim(fmt.Sprintf("%s done in %s, %d tool calls; wrote %s", label, hb.Elapsed.Round(time.Second), hb.Calls, strings.Join(hb.Files, ", "))))
			case "interrupted":
				fmt.Println(dim(label + " interrupted"))
			default:
				fmt.Println(dim(fmt.Sprintf("%s %s: %s", label, hb.Status, hb.Reason)))
			}
		},
```

Import `subagent`. `TaskVerb` results are printed for `/task reply` — and the reply itself is echoed to every terminal by the TUI (below); in plain mode the printed line is enough.

- [ ] **Step 6: TUI — events, commands, bottom line (failing test first)**

`internal/tui/subagents_test.go`:

```go
package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/brown-enterprises/be-code/internal/subagent"
)

func TestSubAgentEventsReachEveryTerminal(t *testing.T) {
	s, a, b := twoViews(t)
	s.onSubAgentStart(subagent.Dispatch{Node: "3.2", Owner: "big", Text: "port internal/scan"})
	s.onSubAgentAsk(subagent.Ask{Node: "3.2", Owner: "big", Question: "which tokenizer?"})
	s.onSubAgentEnd(subagent.HandBack{Node: "3.2", Owner: "big", Status: "done", Summary: "ported it",
		Files: []string{"internal/scan/token.go"}, Elapsed: 14 * time.Minute, Calls: 22})
	s.onSubAgentEnd(subagent.HandBack{Node: "3.3", Owner: "big", Status: "blocked", Reason: "turn cap of 40 reached"})
	for _, v := range []*View{a, b} {
		flush(t, v)
		out := v.View()
		for _, want := range []string{"big started 3.2 port internal/scan", "big (3.2)? which tokenizer?", "big (3.2)> ", "ported it",
			"done in 14m0s, 22 tool calls; wrote internal/scan/token.go", "big (3.3) blocked: turn cap of 40 reached"} {
			if !strings.Contains(out, want) {
				t.Fatalf("terminal lacks %q:\n%s", want, out)
			}
		}
	}
}

func TestBottomLineShowsRunningSubAgents(t *testing.T) {
	s, a, _ := twoViews(t)
	s.runningSubs = func() []subAgentGlyph { return []subAgentGlyph{{Name: "big", At: "3.2.2"}} }
	if line := a.bottomLine(); !strings.Contains(line, "⚙ big 3.2.2") {
		t.Fatalf("bottom line: %q", line)
	}
	s.runningSubs = func() []subAgentGlyph { return []subAgentGlyph{{Name: "big", At: "3.2.2"}, {Name: "claude", At: "3.3"}} }
	if line := a.bottomLine(); !strings.Contains(line, "⚙ 2 lanes") {
		t.Fatalf("bottom line: %q", line)
	}
}
```

`subAgentGlyph{Name, At string}` and `Session.runningSubs func() []subAgentGlyph` keep the view independent of the agent for this test: `NewSession` sets `runningSubs` to read `ag.RunningSubAgents()` and map `Name`/`At`. Check `twoViews` returns `(*Session, *View, *View)` (ask_test.go:14) and adjust the destructuring if it differs.

- [ ] **Step 7: Run it to verify it fails, then implement**

Run: `go test ./internal/tui -run 'TestSubAgentEvents|TestBottomLineShows' -v` → undefined methods.

In `internal/tui/session.go`:

```go
// subAgentGlyph is what the bottom line shows per running sub-agent.
type subAgentGlyph struct{ Name, At string }
```

add the field `runningSubs func() []subAgentGlyph` to `Session`, set in `NewSession`:

```go
	s.runningSubs = func() []subAgentGlyph {
		var out []subAgentGlyph
		for _, r := range ag.RunningSubAgents() {
			out = append(out, subAgentGlyph{Name: r.Name, At: r.At})
		}
		return out
	}
```

In `wireEvents`, add `OnSubAgentStart: s.onSubAgentStart, OnSubAgentAsk: s.onSubAgentAsk, OnSubAgentEnd: s.onSubAgentEnd,`. Handlers beside `onConsultEnd`:

```go
func (s *Session) onSubAgentStart(d subagent.Dispatch) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
	verb := "started"
	if d.Interrupted {
		verb = "resumed"
	}
	s.appendEntryLocked(entry{Kind: entryDim, Text: fmt.Sprintf("%s %s %s %s", d.Owner, verb, d.Node, d.Text)})
	s.broadcast(statusMsg(s.statusNote))
}

func (s *Session) onSubAgentAsk(a subagent.Ask) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
	s.appendEntryLocked(entry{Kind: entryCoworkAsk, Label: fmt.Sprintf("%s (%s)", a.Owner, a.Node), Text: a.Question})
}

func (s *Session) onSubAgentEnd(hb subagent.HandBack) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushLocked()
	label := fmt.Sprintf("%s (%s)", hb.Owner, hb.Node)
	switch hb.Status {
	case "done":
		s.appendEntryLocked(entry{Kind: entryCowork, Label: label, Text: hb.Summary})
		files := "nothing"
		if len(hb.Files) > 0 {
			files = strings.Join(hb.Files, ", ")
		}
		s.appendEntryLocked(entry{Kind: entryDim, Text: fmt.Sprintf("%s done in %s, %d tool calls; wrote %s", label, hb.Elapsed.Round(time.Second), hb.Calls, files)})
	case "interrupted":
		s.appendEntryLocked(entry{Kind: entryDim, Text: label + " interrupted"})
	default:
		s.appendEntryLocked(entry{Kind: entryDim, Text: fmt.Sprintf("%s %s: %s", label, hb.Status, hb.Reason)})
	}
	s.broadcast(statusMsg(s.statusNote))
}
```

In `view.go`'s `slashCommand`, in the `/task` case before the inner `switch`:

```go
		if lines, ok := ui.TaskVerb(m.ag, args); ok {
			if args[0] == "reply" && len(lines) == 1 && strings.HasPrefix(lines[0], "reply delivered") {
				// The answer is part of the shared record: every terminal sees it.
				m.appendEntry(entry{Kind: entryDim, Text: fmt.Sprintf("reply to %s: %s", args[1], strings.Join(args[2:], " "))})
			}
			m.renderLocalLines(lines)
			return m, nil
		}
```

and a new case:

```go
	case "/agents":
		var args []string
		if len(fields) > 1 {
			args = fields[1:]
		}
		m.renderLocalLines(ui.AgentLines(m.ag, args))
		return m, nil
```

In `bottomLine`, after the `IDEName` marker:

```go
	if subs := m.runningSubs(); len(subs) == 1 {
		line += m.st.Accent.Render(" ⚙ " + subs[0].Name + " " + subs[0].At)
	} else if len(subs) > 1 {
		line += m.st.Accent.Render(fmt.Sprintf(" ⚙ %d lanes", len(subs)))
	}
```

(`m.runningSubs` is the embedded `*Session`'s field; guard `nil` for views built without a session in tests.) Do the same in `compactBottomLine` if it renders the state.

Check that the TUI's `/menu` (`grep -rn "coworkers" internal/tui/*.go` for the menu group) lists `/agents` beside `/coworkers` if the menu is built from a hand-written group list rather than `ui.SlashCommandTable`.

- [ ] **Step 8: Run both UI packages**

Run: `go test ./internal/tui ./internal/ui -race`
Expected: PASS. Stat `~/.be-code/config.json` before and after (its mtime must not change).

- [ ] **Step 9: Commit**

```bash
git add internal/ui internal/tui internal/engine/store.go internal/agent/subagents.go
git commit -m "ui: /task assign|scope|reply, /agents, sub-agent lines and the lane glyph

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9"
```

---

### Task 9: Wiring, headless runs, the same-server notice, docs and version

**Files:**
- Modify: `cmd/root.go:245-270` (`buildAgent` after `attachEngine`), `:491` (`finishSession`), `:619-641` (`runInteractive`), `cmd/live.go:105-157` (`runSessionHost`), `cmd/commands.go:55-89` (`run`), `internal/agent/cowork.go:478-494` (`noteSharedServer`), `internal/agent/subagents.go` (`StartSubAgents`, `WaitSubAgents`, `SubAgentReport`)
- Test: `cmd/root_test.go`, `internal/agent/subagents_test.go`, `internal/agent/cowork_test.go`
- Modify: `README.md` (status line, new `## Sub-agents` section after `## Co-working models`), `CHANGELOG.md`, `build.mk`, `../../CLAUDE.md` (the root `CLAUDE.md`, one paragraph under Co-working models and the version line), `docs/live-checklist.md`

**Interfaces:**
- Produces: `func (a *Agent) StartSubAgents()` (resume pass + first schedule — called by each UI once `Approve` and `Events` are wired, never by `buildAgent`); `func (a *Agent) WaitSubAgents(ctx context.Context)`; `func (a *Agent) SubAgentReport() []subagent.HandBack`.

- [ ] **Step 1: Wait and report (agent)**

In `internal/agent/subagents.go`, add:

```go
// WaitSubAgents blocks until no sub-agent is running or ctx ends; the
// headless run uses it before draining hand-backs.
func (a *Agent) WaitSubAgents(ctx context.Context) {
	if a.subs == nil {
		return
	}
	for len(a.RunningSubAgents()) > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// SubAgentReport is every hand-back so far, for run --json.
func (a *Agent) SubAgentReport() []subagent.HandBack {
	if a.subs == nil {
		return nil
	}
	a.subs.mu.Lock()
	defer a.subs.mu.Unlock()
	return append([]subagent.HandBack(nil), a.subs.finished...)
}
```

with `finished []subagent.HandBack` on `subAgents`, appended in `runSub` after `settleSub` (under `s.mu`, in the same critical section that deletes the run). Add to `subagents_test.go`:

```go
func TestEnableDoesNotDispatchUntilStart(t *testing.T) {
	sub := &scriptedProvider{responses: []provider.ChatResponse{{Content: "done"}}}
	f := newSubFixture(t, sub, nil)
	f.assign(t)
	cws, _ := f.ag.Cfg.ValidCoworkers()
	f.ag.EnableSubAgents(cws, "http://primary:11434")
	select {
	case d := <-f.start:
		t.Fatalf("dispatched before StartSubAgents: %+v", d)
	case <-time.After(200 * time.Millisecond):
	}
	f.ag.StartSubAgents()
	wait(t, f.start, "start")
	wait(t, f.ends, "end")
	if rep := f.ag.SubAgentReport(); len(rep) != 1 || rep[0].Status != "done" {
		t.Fatalf("report: %+v", rep)
	}
}
```

Run: `go test ./internal/agent -race -run 'TestSubAgent|TestStopAll|TestResumeThat|TestEnableDoes'` → PASS.

- [ ] **Step 2: The same-server notice**

In `internal/agent/cowork.go`'s `noteSharedServer`, where the message is composed, append when `cw.SubAgent`:

```go
	if cw.SubAgent {
		msg += "; sub-agents on this server interleave with the primary and cost it a cold prompt read per hand-over"
	}
```

(Read the function first: the message variable may be a format string passed to `a.notice`; extend that call instead.) Add to `cowork_test.go`, next to the existing shared-server test, an assertion that a `SubAgent: true` co-worker's notice contains `cold prompt read per hand-over` and a plain one's does not.

- [ ] **Step 3: cmd wiring**

In `cmd/root.go`'s `buildAgent`, after `attachEngine(cfg, reg, ag, flagResume != "")` and before `applyModelParams`:

```go
	if cws, _ := cfg.ValidCoworkers(); anySubAgent(cws) {
		name := flagProvider
		if name == "" {
			name = cfg.DefaultProvider
		}
		ag.EnableSubAgents(cws, subagent.LaneKey(cfg.Providers[name].BaseURL))
	}
```

with:

```go
func anySubAgent(cws []config.CoworkerConfig) bool {
	for _, cw := range cws {
		if cw.SubAgent {
			return true
		}
	}
	return false
}
```

`finishSession` (root.go:491): first statement `ag.StopAllSubAgents("session ended")`.

`runInteractive` (root.go:619): after the TUI/REPL has wired events and approvals — for the in-process TUI, right after `tui.NewSession(...)`/`wireEvents`; for plain mode, right after `r.Agent.Tools.Approve`/`Events` are set — call `ag.StartSubAgents()`. `runSessionHost` (live.go:105): after `s := tui.NewSession(cfg, ag, p)`, `ag.StartSubAgents()`. Headless `run` (commands.go): after `ag.Tools.Approve = headlessApprover(cfg)` and the events line, `ag.StartSubAgents()`; then replace the single `RunFull` with:

```go
		answer, rep, err := ag.RunFull(cmd.Context(), prompt)
		// Sub-agents hand back into the queue; headless, nobody types the
		// next turn, so the run itself takes up to three of them (a
		// question, its answer, the hand-back).
		for round := 0; err == nil && ag.SubAgentsEnabled() && round < 3; round++ {
			ag.WaitSubAgents(cmd.Context())
			msgs := ag.DrainInbox()
			if len(msgs) == 0 {
				break
			}
			answer, rep, err = ag.RunFull(cmd.Context(), strings.Join(msgs, "\n\n"))
		}
		if err != nil {
			return err
		}
```

and in the `--json` map add `"sub_agents": subAgentRows(ag)`:

```go
func subAgentRows(ag *agent.Agent) []map[string]any {
	rows := []map[string]any{}
	for _, hb := range ag.SubAgentReport() {
		rows = append(rows, map[string]any{"node": hb.Node, "owner": hb.Owner, "status": hb.Status,
			"reason": hb.Reason, "elapsed_seconds": hb.Elapsed.Seconds(), "tool_calls": hb.Calls, "files": hb.Files})
	}
	return rows
}
```

A `cmd/root_test.go` test: build a config with one `sub_agent: true` co-worker through whatever helper the file uses for `buildAgent` (look at the co-worker wiring test there), assert `ag.SubAgentsEnabled()` and that `ag.RunningSubAgents()` is empty until `StartSubAgents` (nothing assigned, so still empty after — the assertion is on `SubAgentsEnabled`); and one with no `sub_agent` entry asserting `!ag.SubAgentsEnabled()`.

Run: `go build ./... && go test ./cmd ./internal/agent` → PASS.

- [ ] **Step 4: Docs and version**

- `build.mk`: `VERSION := 1.1.0`. README's status line: bump to 1.1.0.
- `README.md`: after the "Co-working models" section (before `## Select and copy`), add `## Sub-agents`: what `sub_agent: true` does; the task-document forms (the two example lines from Task 4's doc snippet); `/task assign|scope|reply`, `/agents`, `/agents stop`; approvals follow your mode; a same-server sub-agent costs the primary a cold prompt read per hand-over, a second server does not; online consent names the scope; `sub_agents.*` keys pointing at the reference.
- `CHANGELOG.md`: `## v1.1.0 — sub-agents on the task engine`, a short paragraph, bullets: owner and scope on a node; one doing node per dispatched subtree; scoped registry and `ask_main`; per-server lanes, primary first; approvals on the main model's schema; silent re-dispatch on resume with failures surfaced; `/task assign|scope|reply`, `/agents`; `run --json` `sub_agents`; the measured cold-read cost stated.
- Root `CLAUDE.md`: version line `(currently v1.1.0; build.mk VERSION is at 1.1.0 too)`, and under "Co-working models" one paragraph: `internal/subagent` (records, scope, ready rule, lanes), the engine's pens (`penOf`, `DoingUnder`, `ObserveFor`), `Registry.Scoped`, `agent/subagents.go` (`EnableSubAgents` then `StartSubAgents` from the UIs; hand-backs via `Enqueue`; `StopAllSubAgents` in `finishSession`), the lane hook in `chatInLane`, the two prompt constants.
- `docs/live-checklist.md`: `## Sub-agents` with numbered steps: (1) a small local co-worker with `sub_agent: true` on .150, `/task assign <id> <name>` + `/task scope`, expect `<name> started …`, the bottom-line glyph, `<name> (<id>)> …` on both terminals, `done by <name>` in the document; (2) a same-server card naming the primary's model: `/api/ps` shows no reload; (3) an online card: the consent modal names the scope; (4) `/quit` mid-step, `--resume`: `<name> resumed <id> …`; (5) approve mode: a sub-agent write raises the modal labelled `sub-agent <name> (<id>):`.

- [ ] **Step 5: Verify and commit**

Run: `make -f build.mk verify` → green. Stat `~/.be-code/config.json` before and after.

```bash
git add cmd internal/agent README.md CHANGELOG.md build.mk docs/live-checklist.md ../../CLAUDE.md
git commit -m "sub-agents: wiring, headless rounds, docs — 1.1.0

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9"
```

(If the root `CLAUDE.md` is outside this git repository — it is one directory above `be-code/be-code` — leave it out of `git add` and edit it in place.)

---

### Task 10: e2e — a sub-agent owns a step, asks, and hands back

**Files:**
- Modify: `test/e2e/mock_server.py:157-198` (routing) plus new `lead_chunks`/`sub_chunks`, `test/e2e/run_e2e.sh` (new scenario before `echo "E2E PASS"`)

- [ ] **Step 1: The scripts**

In `mock_server.py`, beside `task_chunks`:

```python
# Sixth scenario: a sub-agent. The lead plans one step, assigns it to "sub"
# with a scope, and stops. The sub-agent (model sub-model) writes inside
# its scope, is refused outside it, asks the lead, and finishes. The
# headless run feeds the ask and the hand-back back to the lead, whose
# last reply reports whether the hand-back named the file it wrote.
LEAD = {"n": 0, "root": "2"}
SUB = {"n": 0}

def lead_chunks(body):
    n = LEAD["n"]; LEAD["n"] += 1
    last = body["messages"][-1].get("content") or ""
    users = [m.get("content") or "" for m in body["messages"] if m["role"] == "user"]
    if n == 0:
        return [tool_call_chunk("task", {"action": "plan", "text": "port the scanner",
                                         "steps": ["port internal/scan"]})]
    if n == 1:
        if last.startswith("task "):
            LEAD["root"] = last.split()[-1]
        return [tool_call_chunk("task", {"action": "owner", "id": LEAD["root"] + ".1", "owner": "sub"})]
    if n == 2:
        return [tool_call_chunk("task", {"action": "scope", "id": LEAD["root"] + ".1", "paths": ["internal/scan"]})]
    if any("asks about" in u for u in users) and not any("finished" in u for u in users) and n == 3:
        return [tool_call_chunk("task", {"action": "reply", "id": LEAD["root"] + ".1", "text": "stay inside internal/scan"})]
    if any("sub-agent sub finished" in u and "wrote internal/scan/token.go" in u for u in users):
        return [text_chunk("HANDBACK:yes")]
    return [text_chunk("assigned; waiting")]

def sub_chunks(body):
    n = SUB["n"]; SUB["n"] += 1
    if n == 0:
        return [tool_call_chunk("write_file", {"path": "internal/scan/token.go", "content": "package scan\n"})]
    if n == 1:
        return [tool_call_chunk("write_file", {"path": "cmd/x.go", "content": "package cmd\n"})]
    if n == 2:
        return [tool_call_chunk("ask_main", {"question": "may I touch cmd/x.go?"})]
    return [text_chunk("SUBDONE: token loop ported")]
```

Routing in `do_POST`, before the `task-model` branch:

```python
        elif model == "lead-model":
            chunks = lead_chunks(body)
        elif model == "sub-model":
            chunks = sub_chunks(body)
```

and add `{"id": "lead-model"}, {"id": "sub-model"}` to the `/v1/models` listing.

In `run_e2e.sh`, before `echo "E2E PASS"`:

```sh
# Sixth scenario: a sub-agent owns one step. The lead assigns it, the
# sub-agent writes inside its scope, is refused outside, asks, and the
# lead's final reply confirms the hand-back named the file.
SUBWS="$DIR/subws"; mkdir -p "$SUBWS/internal/scan" "$SUBWS/cmd"
cat > "$HOME/.be-code/config.json" <<CFG
{"default_provider":"mock","model":"lead-model",
 "providers":{"mock":{"type":"openai","base_url":"http://127.0.0.1:18111/v1"}},
 "coworkers":[{"name":"sub","provider":"mock","model":"sub-model","sub_agent":true}],
 "sub_agents":{"max_concurrent":1,"max_turns":6,"ask_timeout":30},
 "max_turns":8,"max_repairs":0,"compat_tool_calls":"auto","verify_on_done":false,
 "engine":{"enabled":true}}
CFG
OUT6="$DIR/subagent.out"
"$(dirname "$0")/../../be-code" run -y -C "$SUBWS" "sub-agent scenario: port the scanner" > "$OUT6"
cat "$OUT6"
grep -q "HANDBACK:yes" "$OUT6"
test -f "$SUBWS/internal/scan/token.go"
test ! -f "$SUBWS/cmd/x.go"
grep -q "@sub  scope: internal/scan" "$SUBWS"/.be-code/tasks/*.md
grep -q "^  - \[x\] .*port internal/scan" "$SUBWS"/.be-code/tasks/*.md
echo "[PASS] sub-agent"
```

- [ ] **Step 2: Build and run**

Run: `make -f build.mk build && sh test/e2e/run_e2e.sh`
Expected: `[PASS] sub-agent` then `E2E PASS`. If the lead never sees the ask: the headless loop in Task 9 waits for the sub-agent, which is parked on `ask_main` and so *is* still running — `WaitSubAgents` must return when every running sub-agent is parked on an ask (add that to `RunningSubAgents`' filter: a run whose `State` starts with `asking:` counts as not-running for the wait; keep it in the bottom line). Adjust `WaitSubAgents` to use a dedicated `subAgentsBusy()` that excludes parked runs, and add a unit test for it in `subagents_test.go`.

- [ ] **Step 3: Commit**

```bash
git add test/e2e internal/agent
git commit -m "e2e: a sub-agent owns a step, asks and hands back

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>
Claude-Session: https://claude.ai/code/session_01Uc1f94U5xUbhx6HZbbrrP9"
```

---

## Spec coverage (self-review)

| Spec | Task |
|---|---|
| §1.1 document fields, child inherits, hand edits win | 4 (parse/render, note), 5 (load warnings) |
| §1.2 node fields, `done by` | 4 |
| §1.3 one doing node per pen, unfiled per pen | 5 |
| §1.4 co-worker card, `max_scope`, `/agents` | 1, 5 (`SetCards`), 8 |
| §1.5 evidence per pen, one-line render | 5 |
| §1.6 records | 2 |
| §2.1 ready rule | 2, 7 (`ScheduleSubAgents`) |
| §2.2 lanes, primary first, cap, cold-read notice | 3, 7 (`chatInLane`), 9 (notice) |
| §2.3 dispatch, consent naming the scope, same-server reuse | 7 |
| §2.4 scoped tools, `ask_main`, subtree task tool | 6 |
| §2.5 prompts | 7 |
| §2.6 hand-back, verification stays the main model's | 7 (queue), 9 (headless rounds) |
| §2.7 `ask_main` round trip, timeout, one at a time, recorded | 6, 7 |
| §3.2 transcript voice, plain mode, `run --json` | 8, 9 |
| §3.3 approvals on the main model's schema, checkpoint | 6 (`Scoped` shares the seam) |
| §3.4 commands, bottom line | 8 |
| §3.5 Esc, stop, reassignment, session end | 7, 9 (`finishSession`) |
| §3.6 resume, cannot-resume notice | 7, 9 (`StartSubAgents`) |
| §3.7 errors table | 5, 6, 7 |
| §4.1 config | 1 |
| §4.3 tests, e2e | every task, 10 |

Known simplifications, each recorded so the reviewer can weigh them: the `after:` field is document-only (no tool action or command sets it — the spec lists none); the bottom line shows the root id when a sub-agent has not yet opened a child; a sub-agent's evidence is filed through the primary's store pointer (`scratch.Engine = a.Engine` with `observeFn`), which `volatileTail` already ignores for scratch agents.


