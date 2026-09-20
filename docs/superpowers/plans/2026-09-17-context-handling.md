# Context Handling Overhaul Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rewrite `internal/engine` around a task tree that records continuously and survives compaction, make native `/api/chat` the normal Ollama path with a configurable `num_ctx`, and put every model load behind one loader that applies per-model parameters.

**Architecture:** A task node owns the evidence produced while it was `doing`; the `doing` node keeps raw tool output verbatim and is distilled when it closes; finished branches roll up into Task Reports. Truth lives as Markdown in `<project>/.be-code/tasks/`, with a small disposable state file in the dotdir. The provider gains a native chat path carrying an options block, and a new `internal/loader` package is the only caller that loads a model or sends `num_ctx`, gated on consent because the server is shared.

**Tech Stack:** Go 1.22+, no new dependencies. Existing packages: `internal/engine`, `internal/provider`, `internal/agent`, `internal/tools`, `internal/ui`, `internal/tui`, `cmd`.

**Spec:** `docs/superpowers/specs/2026-09-17-context-handling-design.md` — read it first; this plan argues from it.

## Global Constraints

- **Advisory, always.** No engine or loader path may fail a turn, block the loop, or panic a session. A store that will not open leaves the `task` tool registered over a no-op ledger, printing exactly `warn: engine: %v; continuing without working memory\n`. A recorder panic is fenced with `recover()` and reported as a notice, as `Agent.observe` does today.
- **Deterministic recording.** The recorder never calls a model. Every fact in a Task Report is derived from what actually happened.
- **The user's server is shared.** Any action that changes server state — loading a model, or sending a `num_ctx` that differs from the loaded instance — happens only after explicit consent, and never in a non-interactive run.
- **Atomic writes** for every persisted file: write `<path>.tmp.<pid>.<seq>`, then `os.Rename`, mode `0600` for dotdir state and `0644` for workspace Markdown.
- **Path confinement**: every workspace path goes through the registry's root-relative checks; nothing is written outside `reg.Root` except the dotdir.
- **No new dependencies.** Standard library only.
- **Tests never touch the real `~/.be-code`.** Every new test package gets the `TestMain` HOME guard used by `internal/engine/testmain_test.go`: `os.MkdirTemp` into `HOME` and `USERPROFILE`, `m.Run()`, `os.RemoveAll`.
- `make -f build.mk verify` green and `go vet` clean at the end of every task.
- Go 1.22+; run `go test -race ./...` for any task touching goroutines.

---

## File Structure

**`internal/engine/` — rewritten around the task tree.**

| file | responsibility |
|---|---|
| `node.go` | `Node`, `Status`, id arithmetic, tree walks, status transitions |
| `store.go` | open, load, save, reconcile, the mutex discipline, migration from 0.10.0 |
| `markdown.go` | the document format: render a tree to Markdown, parse one back tolerantly |
| `record.go` | `Observe`, the verbatim buffer, distillation, `Cached`, digest and lookup handling |
| `report.go` | rolling a finished branch into a Task Report |
| `render.go` | the `Working memory:` block and the report condensation ladder |

**`internal/provider/` — native Ollama.**

| file | responsibility |
|---|---|
| `ollama.go` | native `/api/chat`: streaming, tools, the options block; existing management calls |
| `ollama_model.go` | tags, running list, window and Modelfile reads used by the loader |

**`internal/loader/` — new package, the only path that loads a model.**

| file | responsibility |
|---|---|
| `loader.go` | resolve per-model parameters, apply them, own the consent gate |

**Elsewhere:** `internal/tools/task.go` (five verbs), `internal/ui/task.go` (the `/task` renderer both UIs call), `cmd/engine.go` (wiring), `internal/agent/loop.go` (recorder hook, compaction), `internal/config/config.go` (new keys).

---

## Task 1: The task tree

**Files:**
- Create: `internal/engine/node.go`
- Test: `internal/engine/node_test.go`
- Delete at the end of Task 3, not here: nothing yet

**Interfaces:**
- Consumes: nothing.
- Produces: `Status` (`StatusTodo`, `StatusDoing`, `StatusDone`, `StatusBlocked`, `StatusDropped`), `Node`, `Tree`, and the methods listed below. Every later task builds on these names.

```go
type Status string

const (
	StatusTodo    Status = "todo"
	StatusDoing   Status = "doing"
	StatusDone    Status = "done"
	StatusBlocked Status = "blocked"
	StatusDropped Status = "dropped"
)

type Node struct {
	ID       string    `json:"id"`     // dotted path: "2", "2.1", "2.1.3"
	Text     string    `json:"text"`
	Status   Status    `json:"status"`
	Reason   string    `json:"reason,omitempty"` // why, for blocked and dropped
	Opened   time.Time `json:"opened"`
	Closed   time.Time `json:"closed,omitempty"`
	Children []*Node   `json:"children,omitempty"`
	Evidence Evidence  `json:"evidence,omitempty"`
}

type Tree struct {
	Roots []*Node `json:"roots"`
}
```

`Evidence` is defined in Task 2; declare it here as an empty struct with a `//` note and fill it in Task 2, so `node.go` compiles alone.

- [ ] **Step 1: Write the failing tests**

`internal/engine/node_test.go`:

```go
package engine

import "testing"

// TestIDsAreDottedPathsAndSurviveSiblingChanges: a child's id is its
// parent's id plus its 1-based position, and adding a child to one node
// never renumbers another branch.
func TestIDsAreDottedPathsAndSurviveSiblingChanges(t *testing.T) {
	tr := &Tree{}
	a := tr.Add("", "fix the parser")
	b := tr.Add("", "add the cache")
	if a.ID != "1" || b.ID != "2" {
		t.Fatalf("roots: %q %q", a.ID, b.ID)
	}
	c1 := tr.Add(a.ID, "find the bug")
	c2 := tr.Add(a.ID, "fix and verify")
	if c1.ID != "1.1" || c2.ID != "1.2" {
		t.Fatalf("children: %q %q", c1.ID, c2.ID)
	}
	deep := tr.Add(c1.ID, "read lexer.go")
	if deep.ID != "1.1.1" {
		t.Fatalf("grandchild: %q", deep.ID)
	}
	// Adding under 1 must not touch 2.
	if b.ID != "2" {
		t.Fatalf("sibling renumbered: %q", b.ID)
	}
}

// TestFindAndWalk: every node is reachable by id, and Walk visits parents
// before children in document order.
func TestFindAndWalk(t *testing.T) {
	tr := &Tree{}
	a := tr.Add("", "a")
	tr.Add(a.ID, "a1")
	tr.Add(a.ID, "a2")
	tr.Add("", "b")
	if n := tr.Find("1.2"); n == nil || n.Text != "a2" {
		t.Fatalf("find 1.2: %+v", n)
	}
	if tr.Find("9.9") != nil {
		t.Fatal("found a node that does not exist")
	}
	var order []string
	tr.Walk(func(n *Node, depth int) { order = append(order, n.ID) })
	want := []string{"1", "1.1", "1.2", "2"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("walk order %v, want %v", order, want)
		}
	}
}

// TestOneDoingAtATime: marking a node doing clears any other doing node,
// and closing a node stamps Closed exactly once.
func TestOneDoingAtATime(t *testing.T) {
	tr := &Tree{}
	a := tr.Add("", "a")
	b := tr.Add("", "b")
	tr.SetStatus(a.ID, StatusDoing, "")
	tr.SetStatus(b.ID, StatusDoing, "")
	if a.Status != StatusTodo {
		t.Fatalf("a still %s; doing must be exclusive", a.Status)
	}
	if got := tr.Doing(); got == nil || got.ID != b.ID {
		t.Fatalf("doing: %+v", got)
	}
	tr.SetStatus(b.ID, StatusDone, "")
	if b.Closed.IsZero() {
		t.Fatal("Closed not stamped")
	}
	first := b.Closed
	tr.SetStatus(b.ID, StatusDone, "")
	if !b.Closed.Equal(first) {
		t.Fatal("Closed re-stamped on a second close")
	}
	if tr.Doing() != nil {
		t.Fatal("a closed node is still doing")
	}
}

// TestBlockedAndDroppedKeepTheirReason: the two terminal statuses that are
// not success carry why, because a report six turns later reads very
// differently for "could not" and "decided not to".
func TestBlockedAndDroppedKeepTheirReason(t *testing.T) {
	tr := &Tree{}
	a := tr.Add("", "a")
	tr.SetStatus(a.ID, StatusBlocked, "needs the API key")
	if a.Reason != "needs the API key" {
		t.Fatalf("reason: %q", a.Reason)
	}
	tr.SetStatus(a.ID, StatusDropped, "not needed after all")
	if a.Status != StatusDropped || a.Reason != "not needed after all" {
		t.Fatalf("%s / %q", a.Status, a.Reason)
	}
}

// TestTerminalAndActiveBranch: a root is terminal when it and every
// descendant are done, blocked or dropped; ActiveBranch is the path from a
// root down to the doing node.
func TestTerminalAndActiveBranch(t *testing.T) {
	tr := &Tree{}
	a := tr.Add("", "a")
	a1 := tr.Add(a.ID, "a1")
	a2 := tr.Add(a.ID, "a2")
	if tr.Terminal(a) {
		t.Fatal("todo children are not terminal")
	}
	tr.SetStatus(a1.ID, StatusDone, "")
	tr.SetStatus(a2.ID, StatusDropped, "not needed")
	tr.SetStatus(a.ID, StatusDone, "")
	if !tr.Terminal(a) {
		t.Fatal("all-terminal children should be terminal")
	}
	b := tr.Add("", "b")
	b1 := tr.Add(b.ID, "b1")
	tr.SetStatus(b1.ID, StatusDoing, "")
	branch := tr.ActiveBranch()
	if len(branch) != 2 || branch[0].ID != b.ID || branch[1].ID != b1.ID {
		t.Fatalf("branch %v", branch)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/engine/ -run 'TestIDs|TestFindAndWalk|TestOneDoing|TestBlockedAndDropped|TestTerminalAndActive'`
Expected: FAIL, undefined: `Tree`, `Node`, `Status`.

- [ ] **Step 3: Implement `internal/engine/node.go`**

```go
package engine

import (
	"strconv"
	"strings"
	"time"
)

type Status string

const (
	StatusTodo    Status = "todo"
	StatusDoing   Status = "doing"
	StatusDone    Status = "done"
	StatusBlocked Status = "blocked"
	StatusDropped Status = "dropped"
)

// terminal reports whether a status means the node will not be worked on
// again. blocked and dropped stay distinct all the way into the report:
// "could not" and "decided not to" are different facts.
func (s Status) terminal() bool {
	return s == StatusDone || s == StatusBlocked || s == StatusDropped
}

type Node struct {
	ID       string    `json:"id"`
	Text     string    `json:"text"`
	Status   Status    `json:"status"`
	Reason   string    `json:"reason,omitempty"`
	Opened   time.Time `json:"opened"`
	Closed   time.Time `json:"closed,omitempty"`
	Children []*Node   `json:"children,omitempty"`
	Evidence Evidence  `json:"evidence,omitempty"`
}

type Tree struct {
	Roots []*Node `json:"roots"`
}

// Add appends a child under parent ("" for a new root) and returns it. Ids
// are positional, so a node's id never changes while it exists: adding to
// one branch cannot renumber another.
func (t *Tree) Add(parent, text string) *Node {
	n := &Node{Text: strings.TrimSpace(text), Status: StatusTodo, Opened: time.Now()}
	if parent == "" {
		n.ID = strconv.Itoa(len(t.Roots) + 1)
		t.Roots = append(t.Roots, n)
		return n
	}
	p := t.Find(parent)
	if p == nil {
		// An unknown parent must not lose the node: file it as a root.
		n.ID = strconv.Itoa(len(t.Roots) + 1)
		t.Roots = append(t.Roots, n)
		return n
	}
	n.ID = p.ID + "." + strconv.Itoa(len(p.Children)+1)
	p.Children = append(p.Children, n)
	return n
}

func (t *Tree) Find(id string) *Node {
	var found *Node
	t.Walk(func(n *Node, _ int) {
		if n.ID == id {
			found = n
		}
	})
	return found
}

// Walk visits every node, parents before children, in document order.
func (t *Tree) Walk(fn func(n *Node, depth int)) {
	var rec func(ns []*Node, depth int)
	rec = func(ns []*Node, depth int) {
		for _, n := range ns {
			fn(n, depth)
			rec(n.Children, depth+1)
		}
	}
	rec(t.Roots, 0)
}

func (t *Tree) Doing() *Node {
	var d *Node
	t.Walk(func(n *Node, _ int) {
		if n.Status == StatusDoing {
			d = n
		}
	})
	return d
}

// SetStatus moves one node. Exactly one node is doing at a time, so a new
// doing node sends the previous one back to todo; the caller (Task 2) is
// what distils it first.
func (t *Tree) SetStatus(id string, s Status, reason string) *Node {
	n := t.Find(id)
	if n == nil {
		return nil
	}
	if s == StatusDoing {
		if prev := t.Doing(); prev != nil && prev != n {
			prev.Status = StatusTodo
		}
	}
	if s.terminal() && n.Closed.IsZero() {
		n.Closed = time.Now()
	}
	if !s.terminal() {
		n.Closed = time.Time{}
	}
	n.Status = s
	if reason != "" {
		n.Reason = strings.TrimSpace(reason)
	}
	return n
}

// Terminal reports whether this node and everything under it is finished.
func (t *Tree) Terminal(n *Node) bool {
	if n == nil || !n.Status.terminal() {
		return false
	}
	for _, c := range n.Children {
		if !t.Terminal(c) {
			return false
		}
	}
	return true
}

// ActiveBranch is the path from a root down to the doing node, which is
// what the prompt renders in full.
func (t *Tree) ActiveBranch() []*Node {
	d := t.Doing()
	if d == nil {
		return nil
	}
	var path []*Node
	var rec func(ns []*Node, trail []*Node) bool
	rec = func(ns []*Node, trail []*Node) bool {
		for _, n := range ns {
			next := append(append([]*Node{}, trail...), n)
			if n == d {
				path = next
				return true
			}
			if rec(n.Children, next) {
				return true
			}
		}
		return false
	}
	rec(t.Roots, nil)
	return path
}
```

Add a placeholder in `node.go` so the package compiles before Task 2:

```go
// Evidence is what a node accumulated while it was doing. Task 2 fills it.
type Evidence struct{}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/engine/ -run 'TestIDs|TestFindAndWalk|TestOneDoing|TestBlockedAndDropped|TestTerminalAndActive' -v`
Expected: PASS, five tests.

Note: the rest of the package still references the old `Ledger`, so `go build ./...` fails until Task 3 lands. That is expected and is why Task 1's test run is scoped with `-run`.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/node.go internal/engine/node_test.go
git commit -m "engine: the task tree — nodes, dotted ids, statuses, active branch"
```

---

## Task 2: Evidence and the recorder

**Files:**
- Create: `internal/engine/record.go`
- Modify: `internal/engine/node.go` (replace the `Evidence` placeholder)
- Test: `internal/engine/record_test.go`

**Interfaces:**
- Consumes: `Tree`, `Node`, `Status*` (Task 1).
- Produces:

```go
type Range struct{ From, To int }
type Hit struct { File string; Line int; Text string }

type FileRef struct {
	Path   string  `json:"path"`
	Hash   string  `json:"hash,omitempty"`
	Ranges []Range `json:"ranges,omitempty"`
	Edited bool    `json:"edited,omitempty"`
	Note   string  `json:"note,omitempty"`
	Outline []string `json:"outline,omitempty"`
}
type CmdRef struct {
	Cmd     string `json:"cmd"`
	OK      bool   `json:"ok"`
	Excerpt string `json:"excerpt,omitempty"` // capped, first + last lines
}
type LookupRef struct {
	Tool  string `json:"tool"`
	Query string `json:"query"`
	Hits  []Hit  `json:"hits,omitempty"`
}
type NoteRef struct {
	Text     string `json:"text"`
	File     string `json:"file,omitempty"`
	Decision bool   `json:"decision,omitempty"`
}
// RawItem is one verbatim tool call and its output, kept only while its
// node is doing. It lives in the dotdir state file, never in the workspace
// document, because it is large and transient.
type RawItem struct {
	Tool string `json:"tool"`
	Args string `json:"args,omitempty"`
	Out  string `json:"out,omitempty"`
	OK   bool   `json:"ok"`
	Turn int    `json:"turn"`
}
type Evidence struct {
	Files   []FileRef   `json:"files,omitempty"`
	Cmds    []CmdRef    `json:"cmds,omitempty"`
	Lookups []LookupRef `json:"lookups,omitempty"`
	Notes   []NoteRef   `json:"notes,omitempty"`
	Errors  []string    `json:"errors,omitempty"`
	Raw     []RawItem   `json:"raw,omitempty"`     // only while doing
	Dropped int         `json:"dropped,omitempty"` // raw items dropped to a cap
}

type Event struct {
	Tool    string
	Args    map[string]any
	Content string
	IsError bool
}

type Limits struct{ NotesCap, ItemCap, NodeCap int } // bytes
```

Methods on `*Store` (the store itself lands in Task 3; write these as methods on a minimal `recorder` struct that Task 3 embeds, so this task compiles and tests alone):

```go
func (r *recorder) record(n *Node, ev Event, turn int) string
func (r *recorder) distill(n *Node)
func mergeRange(rs []Range, add Range) []Range
func covered(rs []Range, want Range) bool
func seenRange(content string) Range
func excerpt(s string, cap int) string
```

Carry over verbatim from the old `observe.go`, which is being deleted in Task 3: `relTo`, `fileState`, `parseHits` (regex `^([^\s:][^:]*):(\d+):(.*)$`, 50 hits, text truncated at 120 chars), `LookupKey`, `mergeRange`, `covered`, `seenRange`, and the two footer constants:

```go
const footerAlreadyRead = "already read at turn %d (unchanged); outline and notes are in your context"
const footerCached = "(cached; files unchanged)"
```

- [ ] **Step 1: Write the failing tests**

`internal/engine/record_test.go`:

```go
package engine

import (
	"strings"
	"testing"
)

func newRec(item, node int) *recorder { return &recorder{lim: Limits{ItemCap: item, NodeCap: node}} }

// TestRawIsKeptVerbatimWhileDoing: the doing node holds exactly what the
// tool returned, because that is the lossless half of the design.
func TestRawIsKeptVerbatimWhileDoing(t *testing.T) {
	r := newRec(4096, 32768)
	n := &Node{ID: "1", Status: StatusDoing}
	r.record(n, Event{Tool: "shell", Args: map[string]any{"command": "go test ./..."},
		Content: "--- FAIL: TestLex (0.00s)\n    lex_test.go:42: want 3 got 4\nFAIL\n"}, 1)
	if len(n.Evidence.Raw) != 1 {
		t.Fatalf("raw: %+v", n.Evidence.Raw)
	}
	if !strings.Contains(n.Evidence.Raw[0].Out, "lex_test.go:42: want 3 got 4") {
		t.Fatalf("not verbatim: %q", n.Evidence.Raw[0].Out)
	}
}

// TestPerItemAndPerNodeCaps: one huge read cannot swallow the buffer, and
// when a cap bites the node says so rather than pretending to be complete.
func TestPerItemAndPerNodeCaps(t *testing.T) {
	r := newRec(64, 200)
	n := &Node{ID: "1", Status: StatusDoing}
	r.record(n, Event{Tool: "read_file", Args: map[string]any{"path": "big.txt"},
		Content: strings.Repeat("x", 5000)}, 1)
	if len(n.Evidence.Raw[0].Out) > 64+len("\n… (truncated)") {
		t.Fatalf("item cap not applied: %d bytes", len(n.Evidence.Raw[0].Out))
	}
	for i := 0; i < 20; i++ {
		r.record(n, Event{Tool: "shell", Args: map[string]any{"command": "echo hi"},
			Content: strings.Repeat("y", 60)}, i+2)
	}
	var total int
	for _, it := range n.Evidence.Raw {
		total += len(it.Out)
	}
	if total > 200 {
		t.Fatalf("node cap not applied: %d bytes in %d items", total, len(n.Evidence.Raw))
	}
	if n.Evidence.Dropped == 0 {
		t.Fatal("dropped count not recorded; the record must not claim to be complete")
	}
}

// TestDistillKeepsTheFactsAndDropsTheBulk: closing a node turns raw output
// into the distilled record, derived from the buffer and not the transcript.
func TestDistillKeepsTheFactsAndDropsTheBulk(t *testing.T) {
	r := newRec(4096, 32768)
	n := &Node{ID: "1", Status: StatusDoing}
	r.record(n, Event{Tool: "read_file", Args: map[string]any{"path": "lexer.go"},
		Content: "     1\tpackage lex\n     2\tfunc Scan() {}\n"}, 1)
	r.record(n, Event{Tool: "shell", Args: map[string]any{"command": "go build ./..."},
		Content: "ok\n"}, 2)
	r.record(n, Event{Tool: "shell", Args: map[string]any{"command": "go test ./..."},
		Content: "FAIL: TestLex\nmore\n", IsError: true}, 3)
	r.distill(n)
	if len(n.Evidence.Raw) != 0 {
		t.Fatalf("raw survived distillation: %+v", n.Evidence.Raw)
	}
	if len(n.Evidence.Files) != 1 || n.Evidence.Files[0].Path != "lexer.go" {
		t.Fatalf("files: %+v", n.Evidence.Files)
	}
	if len(n.Evidence.Cmds) != 2 || n.Evidence.Cmds[0].Cmd != "go build ./..." || !n.Evidence.Cmds[0].OK {
		t.Fatalf("cmds: %+v", n.Evidence.Cmds)
	}
	if n.Evidence.Cmds[1].OK {
		t.Fatal("a failing command must be recorded as failing")
	}
	if len(n.Evidence.Errors) != 1 || n.Evidence.Errors[0] != "FAIL: TestLex" {
		t.Fatalf("errors: %+v", n.Evidence.Errors) // first line only
	}
}

// TestRedundantReadFooterSurvivesTheRewrite: the 0.10.0 behaviour the model
// depends on — a second read of an unchanged range is answered with a
// footer, and the content is still returned in full.
func TestRedundantReadFooterSurvivesTheRewrite(t *testing.T) {
	r := newRec(4096, 32768)
	n := &Node{ID: "1", Status: StatusDoing}
	ev := Event{Tool: "read_file", Args: map[string]any{"path": "a.go"},
		Content: "     1\tpackage a\n     2\tvar X = 1\n"}
	if f := r.record(n, ev, 1); f != "" {
		t.Fatalf("first read returned a footer: %q", f)
	}
	f := r.record(n, ev, 2)
	if !strings.Contains(f, "already read at turn 1") {
		t.Fatalf("footer: %q", f)
	}
}

// TestEvidenceFollowsTheDoingNode: two nodes, two sets of evidence, no
// leakage — this is what makes a report exact.
func TestEvidenceFollowsTheDoingNode(t *testing.T) {
	r := newRec(4096, 32768)
	a := &Node{ID: "1", Status: StatusDoing}
	b := &Node{ID: "2", Status: StatusDoing}
	r.record(a, Event{Tool: "shell", Args: map[string]any{"command": "one"}, Content: "ok"}, 1)
	r.record(b, Event{Tool: "shell", Args: map[string]any{"command": "two"}, Content: "ok"}, 2)
	r.distill(a)
	r.distill(b)
	if len(a.Evidence.Cmds) != 1 || a.Evidence.Cmds[0].Cmd != "one" {
		t.Fatalf("a: %+v", a.Evidence.Cmds)
	}
	if len(b.Evidence.Cmds) != 1 || b.Evidence.Cmds[0].Cmd != "two" {
		t.Fatalf("b: %+v", b.Evidence.Cmds)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/engine/ -run 'TestRaw|TestPerItem|TestDistill|TestRedundantRead|TestEvidenceFollows'`
Expected: FAIL, undefined: `recorder`.

- [ ] **Step 3: Implement `internal/engine/record.go`**

Structure, with the carried-over helpers dropped in unchanged from `observe.go`:

```go
package engine

import (
	"fmt"
	"strings"
)

type recorder struct {
	lim  Limits
	root string // workspace root, for relTo
}

// record files one tool result against the node that is doing. It returns
// the footer to append to the tool result, or "".
//
// The raw item is what makes the recent work lossless: it is exactly what
// the tool returned, capped so one large read cannot swallow the buffer.
func (r *recorder) record(n *Node, ev Event, turn int) string {
	if n == nil {
		return ""
	}
	item := RawItem{Tool: ev.Tool, Args: argsLine(ev.Args), Out: excerpt(ev.Content, r.lim.ItemCap), OK: !ev.IsError, Turn: turn}
	n.Evidence.Raw = append(n.Evidence.Raw, item)
	r.capNode(n)

	if ev.IsError {
		return ""
	}
	switch ev.Tool {
	case "read_file":
		return r.readFooter(n, ev, turn)
	case "write_file", "edit_file":
		r.markEdited(n, ev)
	}
	return ""
}

// capNode drops the oldest raw items until the node is under its byte cap,
// counting what it dropped. A record that quietly loses half its evidence
// while still reading as complete is worse than one that admits the gap.
func (r *recorder) capNode(n *Node) {
	total := 0
	for _, it := range n.Evidence.Raw {
		total += len(it.Out) + len(it.Args)
	}
	for total > r.lim.NodeCap && len(n.Evidence.Raw) > 1 {
		drop := n.Evidence.Raw[0]
		total -= len(drop.Out) + len(drop.Args)
		n.Evidence.Raw = n.Evidence.Raw[1:]
		n.Evidence.Dropped++
	}
}

// distill turns the raw buffer into the durable record and empties it. It
// reads only the buffer, never the transcript, so it does not depend on the
// transcript still existing — which after a compaction it does not.
func (r *recorder) distill(n *Node) {
	if n == nil {
		return
	}
	for _, it := range n.Evidence.Raw {
		switch it.Tool {
		case "read_file", "write_file", "edit_file":
			r.mergeFile(n, it)
		case "shell", "process":
			n.Evidence.Cmds = append(n.Evidence.Cmds, CmdRef{
				Cmd: it.Args, OK: it.OK, Excerpt: excerpt(it.Out, 240),
			})
		case "search", "lookup", "history":
			n.Evidence.Lookups = append(n.Evidence.Lookups, LookupRef{
				Tool: it.Tool, Query: it.Args, Hits: parseHits(it.Out),
			})
		}
		if !it.OK {
			if line := firstLine(it.Out); line != "" {
				n.Evidence.Errors = append(n.Evidence.Errors, line)
			}
		}
	}
	n.Evidence.Raw = nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// excerpt caps a string at n bytes on a line boundary, never mid-rune, and
// says so when it cut.
func excerpt(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	cut := s[:n]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		cut = cut[:i]
	} else {
		for len(cut) > 0 && !utf8.ValidString(cut) {
			cut = cut[:len(cut)-1]
		}
	}
	return cut + "\n… (truncated)"
}
```

`argsLine` renders the argument that identifies the call: `command` for `shell` and `process`, `path` or `file` for the file tools, `query` or `pattern` for the lookups, else the compact JSON of the map. `mergeFile` carries the digest logic from the old `observeRead`: resolve with `relTo`, hash with `fileState`, rebuild `Outline` via `repomap.Outline` when the hash changed, merge ranges with `mergeRange` otherwise. `readFooter` returns `fmt.Sprintf(footerAlreadyRead, prevTurn)` when an existing `FileRef` has the same hash and `covered()` the new range, and `""` otherwise.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/engine/ -run 'TestRaw|TestPerItem|TestDistill|TestRedundantRead|TestEvidenceFollows' -v`
Expected: PASS, five tests.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/record.go internal/engine/record_test.go internal/engine/node.go
git commit -m "engine: evidence and the continuous recorder — verbatim while doing, distilled on close"
```

---

## Task 3: The store, the Markdown document, and migration

**Files:**
- Create: `internal/engine/markdown.go`, `internal/engine/markdown_test.go`
- Rewrite: `internal/engine/store.go`, `internal/engine/store_test.go`
- Delete: `internal/engine/observe.go`, `internal/engine/observe_test.go` (their logic moved to `record.go` in Task 2; keep any helper still referenced)
- Test: `internal/engine/migrate_test.go`

**Interfaces:**
- Consumes: `Tree`, `Node`, `recorder`, `Limits`, `Event` (Tasks 1–2).
- Produces the whole `Store` API every later task calls:

```go
func Key(root string) string                                          // unchanged from 0.10.0
func Open(root, sessionID string, resumed bool, lim Limits) (*Store, error)
func OpenAt(dir, root, sessionID string, resumed bool, lim Limits) (*Store, error)

func (s *Store) Dir() string
func (s *Store) Flush() error
func (s *Store) NextTurn() int
func (s *Store) Turn() int
func (s *Store) ClearSession()

// tree
func (s *Store) Plan(text string, steps []string) string              // returns the new root id
func (s *Store) Add(parent, text string) (string, error)
func (s *Store) SetStatus(id string, status Status, reason string) error
// SetStatusText is the same, taking the wire word, so internal/tools can
// satisfy TaskLedger without importing engine's Status type.
func (s *Store) SetStatusText(id, status, reason string) error
func (s *Store) Note(id, text, file string, decision, keep bool) error
// EnsureRoot opens a root from the user's message when nothing is doing and
// no root is open, so evidence always has a home. Called by the agent at the
// top of a request.
func (s *Store) EnsureRoot(text string) string
func (s *Store) Tree() Tree                                           // deep copy
func (s *Store) ShowText(id string) string                            // one node, a branch, or all

// recorder seam, called by the agent
func (s *Store) Observe(ev Event) string
func (s *Store) Cached(tool string, args map[string]any) (string, bool)
func (s *Store) DigestKey(path string) string
func (s *Store) HasDigest(path string) (Range, bool)

// durable notes, unchanged semantics from 0.10.0
func (s *Store) Notes() string
func (s *Store) AddNoteLine(text string)
func (s *Store) DropNote(n int) error
func (s *Store) ClearNotes()

// baseline for the changes tool, unchanged
func (s *Store) SetBaseline(b Baseline)
func (s *Store) Baseline() Baseline
```

**Storage layout.** Workspace: `<root>/.be-code/tasks/NNN-<slug>.md`, one per root node, mode `0644`, plus `README.md` written once. Dotdir: `~/.be-code/engine/<Key(root)>/state.json`, mode `0600`, holding `{"active":"2.1","turn":37,"raw":{"2.1":[RawItem…]},"docs":{"001-fix-the-parser.md":"<sha256>"},"session":"…","baseline":{…},"notes_hash":"…"}`. Durable notes stay at `~/.be-code/engine/<key>/notes.md` exactly as today.

**Why raw lives in the dotdir:** it is large, transient, and nobody wants it in a file they might commit. Losing `state.json` costs the verbatim buffer for the node in flight, never the tree.

- [ ] **Step 1: Write the failing tests**

`internal/engine/markdown_test.go`:

```go
package engine

import (
	"strings"
	"testing"
)

const doc = `# 001 — fix the parser

- [x] 1. fix the parser
  - [x] 1.1. find the bug
    - files: lexer.go (lines 1–120)
    - cmds: go test ./... — failed
    - error: FAIL: TestLex
  - [>] 1.2. fix and verify
  - [-] 1.3. rewrite the scanner — dropped: not needed after all
`

// TestRoundTrip: parse a document and render it back byte-identical, so
// the engine never churns a file it did not mean to change.
func TestRoundTrip(t *testing.T) {
	tr, _, err := ParseDoc(doc)
	if err != nil {
		t.Fatal(err)
	}
	out := RenderDoc("001", "fix the parser", tr.Roots[0])
	if out != doc {
		t.Fatalf("round trip differs:\n--- got ---\n%s\n--- want ---\n%s", out, doc)
	}
}

// TestStatusMarks: every status has exactly one mark, and a hand-written
// mark is read back as that status.
func TestStatusMarks(t *testing.T) {
	tr, _, err := ParseDoc(doc)
	if err != nil {
		t.Fatal(err)
	}
	get := func(id string) *Node { return tr.Find(id) }
	if get("1").Status != StatusDone || get("1.2").Status != StatusDoing {
		t.Fatalf("statuses: %s %s", get("1").Status, get("1.2").Status)
	}
	if n := get("1.3"); n.Status != StatusDropped || n.Reason != "not needed after all" {
		t.Fatalf("dropped node: %s %q", n.Status, n.Reason)
	}
}

// TestUnknownLinesArePreserved: a human's own prose survives a round trip,
// because the engine is a guest in this file.
func TestUnknownLinesArePreserved(t *testing.T) {
	edited := doc + "\n> note to self: the scanner is the real culprit\n"
	tr, extra, err := ParseDoc(edited)
	if err != nil {
		t.Fatal(err)
	}
	out := RenderDocWithExtra("001", "fix the parser", tr.Roots[0], extra)
	if !strings.Contains(out, "note to self: the scanner is the real culprit") {
		t.Fatal("hand-written line was lost")
	}
}

// TestHandEditedStatusWins: the user marked a step done in their editor;
// the engine must accept that, not overwrite it.
func TestHandEditedStatusWins(t *testing.T) {
	tr, _, err := ParseDoc(strings.Replace(doc, "- [>] 1.2.", "- [x] 1.2.", 1))
	if err != nil {
		t.Fatal(err)
	}
	if tr.Find("1.2").Status != StatusDone {
		t.Fatal("hand edit ignored")
	}
}

// TestMalformedDocumentIsAnError: a document we cannot understand is an
// error the caller quarantines, never something we half-parse and rewrite.
func TestMalformedDocumentIsAnError(t *testing.T) {
	if _, _, err := ParseDoc("# 001 — x\n\n- [?] banana\n  - [x] 1.1. orphan child\n"); err == nil {
		t.Fatal("expected an error")
	}
}
```

`internal/engine/store_test.go` (rewritten; keep `TestKeyIsStableAndShort`, `TestNotesCapTrimsOldestLines`, `TestAddNoteLineDedupsWholeLinesOnly` from the old file unchanged) plus:

```go
// TestTreeSurvivesReopen: the workspace document is the source of truth, so
// a fresh Store on the same root sees the same tree.
func TestTreeSurvivesReopen(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	s, err := OpenAt(dir, root, "s1", false, Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768})
	if err != nil {
		t.Fatal(err)
	}
	id := s.Plan("fix the parser", []string{"find the bug", "fix and verify"})
	if err := s.SetStatus(id+".1", StatusDoing, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	s2, err := OpenAt(dir, root, "s2", false, Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768})
	if err != nil {
		t.Fatal(err)
	}
	tr := s2.Tree()
	if len(tr.Roots) != 1 || len(tr.Roots[0].Children) != 2 {
		t.Fatalf("tree: %+v", tr.Roots)
	}
	if tr.Find(id + ".1").Status != StatusDoing {
		t.Fatal("status did not survive")
	}
}

// TestQuarantineRatherThanOverwrite: a document edited into nonsense is
// moved aside with its content intact, and the session continues.
func TestQuarantineRatherThanOverwrite(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	s, _ := OpenAt(dir, root, "s1", false, Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768})
	s.Plan("a task", []string{"one"})
	s.Flush()
	path := filepath.Join(root, ".be-code", "tasks", "001-a-task.md")
	os.WriteFile(path, []byte("# 001 — a task\n\n- [?] not a status\n"), 0o644)
	if _, err := OpenAt(dir, root, "s2", false, Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768}); err != nil {
		t.Fatalf("a broken document must not fail the open: %v", err)
	}
	glob, _ := filepath.Glob(filepath.Join(root, ".be-code", "tasks", "*.broken-*.md"))
	if len(glob) != 1 {
		t.Fatalf("quarantine files: %v", glob)
	}
	b, _ := os.ReadFile(glob[0])
	if !strings.Contains(string(b), "not a status") {
		t.Fatal("quarantined content was not preserved")
	}
}

// TestGitignoreGetsTheToolFolderOnce: the record stays out of the user's
// commits unless they choose otherwise, and we never write the line twice.
func TestGitignoreGetsTheToolFolderOnce(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("node_modules/\n"), 0o644)
	dir := t.TempDir()
	s, _ := OpenAt(dir, root, "s1", false, Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768})
	s.Plan("a task", nil)
	s.Flush()
	s.Plan("another", nil)
	s.Flush()
	b, _ := os.ReadFile(filepath.Join(root, ".gitignore"))
	if strings.Count(string(b), ".be-code/") != 1 {
		t.Fatalf(".gitignore: %q", b)
	}
	if !strings.Contains(string(b), "node_modules/") {
		t.Fatal("existing .gitignore content was lost")
	}
}
```

`internal/engine/migrate_test.go`:

```go
// TestMigrationLiftsTheFlatLedger: a 0.10.0 store becomes one task with its
// steps as children and its decisions and facts as notes, losing nothing.
func TestMigrationLiftsTheFlatLedger(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()
	old := `{"task":"fix the parser","steps":[{"text":"find the bug","status":"done"},
	  {"text":"fix and verify","status":"doing"}],"decisions":["quotes handled in the scanner"],
	  "facts":["lexer.go is 900 lines"],"session":"s0"}`
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "ledger.json"), []byte(old), 0o600)

	s, err := OpenAt(dir, root, "s0", true, Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768})
	if err != nil {
		t.Fatal(err)
	}
	tr := s.Tree()
	if len(tr.Roots) != 1 || tr.Roots[0].Text != "fix the parser" {
		t.Fatalf("roots: %+v", tr.Roots)
	}
	if len(tr.Roots[0].Children) != 2 || tr.Roots[0].Children[0].Status != StatusDone {
		t.Fatalf("children: %+v", tr.Roots[0].Children)
	}
	notes := tr.Roots[0].Evidence.Notes
	if len(notes) != 2 || !notes[0].Decision {
		t.Fatalf("notes: %+v", notes)
	}
	if _, err := os.Stat(filepath.Join(dir, "ledger.json")); !os.IsNotExist(err) {
		t.Fatal("the migrated ledger should be removed so migration runs once")
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/engine/`
Expected: FAIL, undefined: `ParseDoc`, `RenderDoc`, `Plan`, `SetStatus`, and compile errors in the old `store.go`.

- [ ] **Step 3: Implement the document format**

`internal/engine/markdown.go`. The format is one heading, then a nested list, then per-node evidence as sub-bullets:

```
# 001 — fix the parser

- [x] 1. fix the parser
  - [x] 1.1. find the bug
    - files: lexer.go (lines 1–120)
    - cmds: go test ./... — failed
    - error: FAIL: TestLex
  - [>] 1.2. fix and verify
  - [-] 1.3. rewrite the scanner — dropped: not needed after all
```

Marks: `[ ]` todo, `[>]` doing, `[x]` done, `[!]` blocked, `[-]` dropped. A blocked or dropped node carries ` — blocked: <reason>` or ` — dropped: <reason>`. Evidence keys are `files:`, `cmds:`, `lookups:`, `note:`, `decision:`, `error:`.

```go
// ParseDoc reads a task document. It returns the tree, the lines it did not
// recognise (preserved verbatim so a human's own prose survives a round
// trip), and an error only when the document is structurally broken — the
// caller quarantines rather than half-parsing.
func ParseDoc(text string) (*Tree, []string, error)

// RenderDoc writes a root node as a document. RenderDocWithExtra appends
// the unrecognised lines the parse preserved.
func RenderDoc(num, title string, root *Node) string
func RenderDocWithExtra(num, title string, root *Node, extra []string) string
```

Parse rules, in order of how often they bite: an unknown mark is an error; a child indented under nothing is an error; an id that does not match its position is repaired to its position and the repair noted; a duplicate id is repaired the same way; any line that is neither a heading, a node nor a known evidence key is returned as extra.

- [ ] **Step 4: Implement the store**

`internal/engine/store.go` keeps the 0.10.0 discipline exactly: one `sync.Mutex`, deep copies out of every getter, `markDirtyLocked`, `Flush` that snapshots under the lock and writes with it released, `writeAtomic` with `<path>.tmp.<pid>.<seq>` then `os.Rename`, and `loadJSON`'s rename-on-corrupt. What changes is what it holds: a `Tree`, the `recorder`, the active node id, and the document hashes.

`Flush` writes each dirty root to `<root>/.be-code/tasks/NNN-<slug>.md` (0644) and `state.json` (0600), then ensures `.be-code/` is in the project `.gitignore` — appended once, only if a `.gitignore` exists or the root is a git repository, never creating one otherwise.

Load order on open: read `state.json`; read every `*.md` in `.be-code/tasks/`; for each, if its hash differs from the recorded one, the user edited it, so the document wins; a parse error quarantines the file to `NNN-<slug>.broken-<yyyymmdd-hhmmss>.md` and rebuilds from nothing rather than overwriting. Then, if `ledger.json` exists, migrate it (Step 5) and delete it.

Session scoping keeps the 0.10.0 rule: `if !resumed && state.Session != sessionID` clears the *verbatim buffers and the active node*, not the tree. The tree is the workspace's record, not the session's.

- [ ] **Step 5: Implement migration**

```go
// migrateLedger lifts a 0.10.0 flat ledger into one task. Steps become
// children, decisions and facts become notes on the task, and the file is
// removed so this runs exactly once. Nothing is discarded.
func (s *Store) migrateLedger(path string) error
```

Map `Step.Status`: `todo`→`StatusTodo`, `doing`→`StatusDoing`, `done`→`StatusDone`, `skip`→`StatusDropped` with reason `"skipped in a previous session"`. Digests and lookups from `digests.json`/`lookups.json` become the task's `Evidence.Files` and `Evidence.Lookups`. A migration that fails renames the whole store directory aside and starts fresh, with `warn: engine: could not migrate the working memory (%v); starting fresh, the old store is at %s`.

- [ ] **Step 6: Run the tests**

Run: `go test ./internal/engine/ -v`
Expected: PASS, everything including the kept 0.10.0 tests.

- [ ] **Step 7: Commit**

```bash
git add internal/engine/
git commit -m "engine: task store over Markdown documents, tolerant reconciliation, 0.10.0 migration"
```

---

## Task 4: Reports and the prompt block

**Files:**
- Create: `internal/engine/report.go`, `internal/engine/report_test.go`
- Rewrite: `internal/engine/render.go`, `internal/engine/render_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1–3.
- Produces:

```go
// Report is a finished branch rolled up. Built from the record, never by a
// model, so it cannot disagree with what happened.
func (s *Store) Report(id string) string
// Render is the Working memory: block. Same signature as 0.10.0 so the
// agent's call site does not change.
func (s *Store) Render(budget int, inMap func(path string) bool) string
func (s *Store) TreeText() string // the /task listing
```

The block, in order: Task Reports for terminal roots (oldest first), the active branch with sibling statuses, the doing node verbatim, then durable notes.

**The condensation ladder.** Render composes at full detail, measures, and while it is over budget condenses the oldest report one rung: full → headline plus outcomes and decisions → one line → `Done: <text> — task show <id> for the report`. The active branch and its verbatim step are never condensed; if the block cannot fit with them intact, it emits what it has and appends `(reports condensed)`.

- [ ] **Step 1: Write the failing tests**

`internal/engine/report_test.go`. Both this file and `render_test.go` use one helper, defined once in `report_test.go`:

```go
// testStore is a store on two temp dirs: a workspace root and a dotdir.
func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := OpenAt(t.TempDir(), t.TempDir(), "s1", false,
		Limits{NotesCap: 4096, ItemCap: 4096, NodeCap: 32768})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestReportIsExactAboutWhatHappened: the rollup names the files, the
// commands and their outcomes, the decisions, and what was left undone.
func TestReportIsExactAboutWhatHappened(t *testing.T) {
	s := testStore(t)
	id := s.Plan("fix the parser", []string{"find the bug", "fix and verify", "rewrite the scanner"})
	s.SetStatus(id+".1", StatusDoing, "")
	s.Observe(Event{Tool: "read_file", Args: map[string]any{"path": "lexer.go"}, Content: "     1\tpackage lex\n"})
	s.Observe(Event{Tool: "shell", Args: map[string]any{"command": "go test ./..."}, Content: "FAIL: TestLex\n", IsError: true})
	s.Note(id+".1", "the scanner eats the quote", "lexer.go", true, false)
	s.SetStatus(id+".1", StatusDone, "")
	s.SetStatus(id+".2", StatusDone, "")
	s.SetStatus(id+".3", StatusDropped, "not needed after all")
	s.SetStatus(id, StatusDone, "")

	rep := s.Report(id)
	for _, want := range []string{
		"fix the parser", "lexer.go", "go test ./...", "FAIL: TestLex",
		"the scanner eats the quote", "dropped: not needed after all",
	} {
		if !strings.Contains(rep, want) {
			t.Fatalf("report missing %q:\n%s", want, rep)
		}
	}
}

// TestReportNeedsNoModel: the rollup is deterministic — same store, same
// bytes, twice.
func TestReportNeedsNoModel(t *testing.T) {
	s := testStore(t)
	id := s.Plan("a", []string{"one"})
	s.SetStatus(id+".1", StatusDone, "")
	s.SetStatus(id, StatusDone, "")
	if s.Report(id) != s.Report(id) {
		t.Fatal("report is not deterministic")
	}
}
```

`internal/engine/render_test.go` (rewritten; keep `TestTrimLinesIsUTF8Safe` unchanged):

```go
// TestActiveWorkOutranksHistory: under a tight budget the finished reports
// condense and the work in flight stays whole, because that is the work the
// model is about to continue.
func TestActiveWorkOutranksHistory(t *testing.T) {
	s := testStore(t)
	for i := 0; i < 4; i++ {
		id := s.Plan(fmt.Sprintf("finished task %d", i), []string{"step"})
		s.SetStatus(id+".1", StatusDoing, "")
		s.Observe(Event{Tool: "shell", Args: map[string]any{"command": "go build ./..."},
			Content: strings.Repeat("noise\n", 200)})
		s.SetStatus(id+".1", StatusDone, "")
		s.SetStatus(id, StatusDone, "")
	}
	live := s.Plan("the live task", []string{"the live step"})
	s.SetStatus(live+".1", StatusDoing, "")
	s.Observe(Event{Tool: "shell", Args: map[string]any{"command": "go test ./parser"},
		Content: "--- FAIL: TestQuote\n    parser_test.go:88: unexpected EOF\n"})

	block := s.Render(1200, func(string) bool { return false })
	if len(block) > 1200 {
		t.Fatalf("over budget: %d bytes", len(block))
	}
	for _, want := range []string{"the live task", "the live step", "parser_test.go:88: unexpected EOF"} {
		if !strings.Contains(block, want) {
			t.Fatalf("live work was cut; missing %q:\n%s", want, block)
		}
	}
	if !strings.Contains(block, "finished task 0") {
		t.Fatal("an old task vanished entirely instead of condensing to a line")
	}
}

// TestBlockStartsWithReportsThenActive: order matters — history first, the
// live branch last, so the newest thing is nearest the model's attention.
func TestBlockStartsWithReportsThenActive(t *testing.T) {
	s := testStore(t)
	done := s.Plan("finished", []string{"a"})
	s.SetStatus(done+".1", StatusDone, "")
	s.SetStatus(done, StatusDone, "")
	live := s.Plan("live", []string{"b"})
	s.SetStatus(live+".1", StatusDoing, "")
	block := s.Render(4096, func(string) bool { return false })
	if strings.Index(block, "finished") > strings.Index(block, "live") {
		t.Fatalf("order wrong:\n%s", block)
	}
}

// TestEmptyStoreRendersNothing: a fresh session adds no prompt weight.
func TestEmptyStoreRendersNothing(t *testing.T) {
	if got := testStore(t).Render(4096, func(string) bool { return false }); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/engine/ -run 'TestReport|TestActiveWork|TestBlockStarts|TestEmptyStore'`
Expected: FAIL, undefined: `Report`.

- [ ] **Step 3: Implement `report.go` and `render.go`**

Report sections, each omitted when empty: the task line with its status, one line per child in order with its status and reason, `files:` with ranges and the edited flag, `cmds:` with outcomes, `decisions:`, `errors:`, and `left:` for anything blocked or dropped with why.

Render's ladder, expressed plainly:

```go
func (s *Store) Render(budget int, inMap func(string) bool) string {
	reports := s.terminalReports()      // []string, oldest first, full detail
	active := s.activeBranchText(inMap) // never condensed
	notes := s.notesText()
	for rung := 0; rung < 4; rung++ {
		out := join(reports, active, notes)
		if len(out) <= budget {
			return out
		}
		if !condenseOldest(reports, rung) {
			break
		}
	}
	return trimLines(join(reports, active, notes), budget) + "\n(reports condensed)"
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/engine/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/engine/report.go internal/engine/report_test.go internal/engine/render.go internal/engine/render_test.go
git commit -m "engine: deterministic task reports and the working-memory block with its condensation ladder"
```

---

## Task 5: The tool surface and the agent wiring

**Files:**
- Rewrite: `internal/tools/task.go`, `internal/tools/task_test.go`
- Modify: `cmd/engine.go` (`noopLedger`, `attachEngine`, `registerEngineTools`), `cmd/engine_test.go`
- Modify: `internal/agent/loop.go` (the `observe` hook, `composeSystem`, `run`, `Compact`, `RunFull`)
- Test: `internal/agent/engine_test.go` (new cases)

**Interfaces:**
- Consumes: the `Store` API from Task 3.
- Produces:

```go
// TaskLedger is what the task tool writes to, behind an interface so tools
// never imports engine.
type TaskLedger interface {
	Plan(text string, steps []string) string
	Add(parent, text string) (string, error)
	SetStatusText(id, status, reason string) error
	Note(id, text, file string, decision, keep bool) error
	ShowText(id string) string
}
func NewTask(l TaskLedger) Tool
```

`cmd/engine.go`'s `noopLedger` gains the same five methods, returning `""`, `("", nil)` and `nil`. `*engine.Store` satisfies `TaskLedger` directly, which is why `SetStatusText` exists alongside the typed `SetStatus`.

Tool description, verbatim:

> "Your task record. action=plan records a task and its steps; action=add adds a step or sub-step under one (parent: its id); action=status marks a node doing, done, blocked or dropped (with reason for the last two); action=note records a fact or decision against a node; action=show prints a node, a branch, or the whole tree. Ids are dotted paths like 2.1.3. What you do while a node is doing is recorded against it and survives compaction, so keep exactly one node doing."

Schema, verbatim:

```json
{"type":"object","properties":{
  "action":{"type":"string","enum":["plan","add","status","note","show"],"description":"plan | add | status | note | show"},
  "text":{"type":"string","description":"plan: the task in one line; add: the step; note: the fact or decision"},
  "steps":{"type":"array","items":{"type":"string"},"description":"plan: the steps in order"},
  "id":{"type":"string","description":"the node, a dotted path like 2.1.3"},
  "parent":{"type":"string","description":"add: the node to add under; omit for a new top-level task"},
  "status":{"type":"string","enum":["doing","done","blocked","dropped"],"description":"status: the new status"},
  "reason":{"type":"string","description":"status: why, for blocked and dropped"},
  "file":{"type":"string","description":"note: the file this note is about"},
  "keep":{"type":"boolean","description":"note: also remember this across sessions"}},
  "required":["action"]}
```

Keep the 0.10.0 forgiving parsing: action aliases `decision`/`fact` map to `note`; status aliases `progress`/`in_progress`/`started`→`doing`, `finished`/`complete`/`completed`→`done`, `skip`/`skipped`→`dropped`, `stuck`/`blocked_on`→`blocked`; `steps` accepts a newline string as well as an array, stripped of `1. `, `2) `, `- `, `* ` markers via the existing `stepMarkerRe`.

- [ ] **Step 1: Write the failing tests**

`internal/tools/task_test.go`:

```go
type recTree struct {
	plans   []string
	adds    [][2]string
	status  [][3]string
	notes   []string
	showArg string
}

func (r *recTree) Plan(text string, steps []string) string { r.plans = append(r.plans, text); return "1" }
func (r *recTree) Add(parent, text string) (string, error) {
	r.adds = append(r.adds, [2]string{parent, text})
	return "1.1", nil
}
func (r *recTree) SetStatusText(id, status, reason string) error {
	r.status = append(r.status, [3]string{id, status, reason})
	return nil
}
func (r *recTree) Note(id, text, file string, decision, keep bool) error {
	r.notes = append(r.notes, text)
	return nil
}
func (r *recTree) ShowText(id string) string { r.showArg = id; return "tree text" }

// TestTaskVerbs: every verb reaches the ledger with its arguments intact.
func TestTaskVerbs(t *testing.T) {
	r := &recTree{}
	tool := NewTask(r)
	run := func(args string) Result {
		var m map[string]any
		json.Unmarshal([]byte(args), &m)
		return tool.Run(context.Background(), m)
	}
	if res := run(`{"action":"plan","text":"fix the parser","steps":["find it","fix it"]}`); res.IsError {
		t.Fatalf("plan: %+v", res)
	}
	if res := run(`{"action":"add","parent":"1","text":"read lexer.go"}`); res.IsError || r.adds[0] != [2]string{"1", "read lexer.go"} {
		t.Fatalf("add: %+v %v", res, r.adds)
	}
	if res := run(`{"action":"status","id":"1.1","status":"dropped","reason":"not needed"}`); res.IsError ||
		r.status[0] != [3]string{"1.1", "dropped", "not needed"} {
		t.Fatalf("status: %+v %v", res, r.status)
	}
	if res := run(`{"action":"show","id":"1"}`); res.IsError || r.showArg != "1" {
		t.Fatalf("show: %+v %q", res, r.showArg)
	}
}

// TestStatusAliasesSmallModelsActuallyEmit: a local model writes
// "completed" and "skipped" as often as the words we chose.
func TestStatusAliasesSmallModelsActuallyEmit(t *testing.T) {
	r := &recTree{}
	tool := NewTask(r)
	for _, in := range []struct{ raw, want string }{
		{"completed", "done"}, {"in_progress", "doing"}, {"skipped", "dropped"}, {"stuck", "blocked"},
	} {
		tool.Run(context.Background(), map[string]any{"action": "status", "id": "1", "status": in.raw})
		got := r.status[len(r.status)-1][1]
		if got != in.want {
			t.Fatalf("%q became %q, want %q", in.raw, got, in.want)
		}
	}
}

// TestPlanAcceptsANewlineBlob: the model often sends one string, not an
// array, and the tool must not punish it for that.
func TestPlanAcceptsANewlineBlob(t *testing.T) {
	r := &recTree{}
	NewTask(r).Run(context.Background(), map[string]any{
		"action": "plan", "text": "a task", "steps": "1. one\n2) two\n- three\n",
	})
	if len(r.plans) != 1 {
		t.Fatalf("plans: %v", r.plans)
	}
}
```

`internal/agent/engine_test.go`:

```go
// TestEvidenceIsRecordedAgainstTheDoingNode: the dispatch hook files tool
// results where the report will later find them.
func TestEvidenceIsRecordedAgainstTheDoingNode(t *testing.T) {
	ag, st := agentWithEngine(t) // helper: agent over a temp-dir store
	id := st.Plan("fix the parser", []string{"find it"})
	st.SetStatusText(id+".1", "doing", "")
	ag.dispatch(context.Background(), provider.ToolCall{
		ID: "c1", Name: "shell", Arguments: `{"command":"go test ./..."}`,
	})
	n := st.Tree().Find(id + ".1")
	if len(n.Evidence.Raw) == 0 {
		t.Fatalf("nothing recorded against the doing node: %+v", n.Evidence)
	}
	st.SetStatusText(id+".1", "done", "")
	if got := st.Tree().Find(id + ".1"); len(got.Evidence.Cmds) != 1 {
		t.Fatalf("distilled cmds: %+v", got.Evidence.Cmds)
	}
}

// TestCompactionSurvivesAnEmptySummary: the failure seen on the validation
// VM. An empty summary must leave the session working from the tree, with a
// notice, rather than falling back to blind trimming.
func TestCompactionSurvivesAnEmptySummary(t *testing.T) {
	ag, st := agentWithEngine(t)
	ag.Provider = emptySummaryProvider{} // returns "" for the summary call
	var notices []string
	ag.Events.OnNotice = func(m string) { notices = append(notices, m) }
	id := st.Plan("fix the parser", []string{"find it"})
	st.SetStatusText(id+".1", "doing", "")
	st.Observe(engine.Event{Tool: "shell", Args: map[string]any{"command": "go test ./..."},
		Content: "FAIL: TestLex
"})

	if err := ag.Compact(context.Background()); err != nil {
		t.Fatalf("an empty summary must not fail compaction: %v", err)
	}
	sys := ag.History.System.Content
	if !strings.Contains(sys, "fix the parser") {
		t.Fatalf("the tree did not survive compaction:\n%s", sys)
	}
	if !containsAny(notices, "continuing from the task record") {
		t.Fatalf("no notice explaining the empty summary: %v", notices)
	}
}

// TestTheEngineCannotFailATurn: advisory discipline, which every task in
// this plan inherits. A panicking store, a hanging one and one that returns
// nonsense each leave the turn working.
func TestTheEngineCannotFailATurn(t *testing.T) {
	for _, bad := range []string{"panic", "hang", "garbage"} {
		t.Run(bad, func(t *testing.T) {
			ag := agentWithBadEngine(t, bad)
			res := ag.dispatch(context.Background(), provider.ToolCall{
				ID: "c1", Name: "shell", Arguments: `{"command":"echo hi"}`,
			})
			if res.IsError {
				t.Fatalf("a %s engine failed the tool call: %+v", bad, res)
			}
		})
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/tools/ ./internal/agent/ -run 'TestTask|TestStatusAliases|TestPlanAccepts|TestEvidenceIsRecorded|TestCompactionKeeps'`
Expected: FAIL, the `TaskLedger` interface no longer matches.

- [ ] **Step 3: Implement the tool and the wiring**

`internal/tools/task.go`: five verbs over the new interface, returning the new id from `plan` and `add` so the model can address what it just made (`Result{Content: "task 2.1"}`).

`cmd/engine.go`: `attachEngine` keeps its exact shape and strings — the warn line, registering tools over `noopLedger{}` when the store fails, `ag.RefreshSystem()` afterwards. Only the `Limits` argument is new:

```go
st, err := engine.Open(reg.Root, ag.Session.ID, resumed,
	engine.Limits{NotesCap: cfg.Engine.NotesCap, ItemCap: cfg.Engine.ItemCap, NodeCap: cfg.Engine.NodeCap})
```

`internal/agent/loop.go`, four edits:

1. `run()`: `a.Engine.EnsureTask(userInput)` becomes `a.Engine.EnsureRoot(userInput)` — if no node is `doing` and no root is open, start one from the user's message, so evidence always has a home.
2. `dispatch()`: unchanged in shape; `a.observe(engine.Event{...})` still appends the returned footer.
3. `composeSystem`: unchanged — it already calls `a.Engine.Render(a.Cfg.Engine.Budget, a.inRepoMap)` under `"\n\nWorking memory:\n"`.
4. `Compact()`: keep the `read_file` substitution and `ApplyFileNotes`. Change the empty-summary path: where it currently falls through to trimming, it now notices `compaction: the model returned no summary; continuing from the task record` and keeps the tree-rendered block, trimming the transcript only.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/tools/ ./internal/agent/ ./cmd/ && make -f build.mk verify`
Expected: PASS, and the build is green for the first time since Task 1.

- [ ] **Step 5: Commit**

```bash
git add internal/tools/task.go internal/tools/task_test.go cmd/engine.go cmd/engine_test.go internal/agent/
git commit -m "task tool: five verbs over the tree; agent records against the doing node and survives an empty summary"
```

---

## Task 6: `/task` in both UIs, and the README

**Files:**
- Create: `internal/ui/task.go` (the shared renderer both UIs call), `internal/engine/readme.go`
- Modify: `internal/ui/common.go` (the table row), `internal/ui/repl.go`, `internal/tui/view.go`
- Test: `internal/ui/repl_test.go`, `internal/tui/engine_test.go`

**Interfaces:**
- Consumes: `Store.TreeText`, `Store.ShowText`, `Store.Report`, `Store.Dir`.
- Produces: `ui.TaskLines(eng TaskViewer, args []string) []string` where `TaskViewer` is a two-method interface (`TreeText() string`, `ShowText(id string) string`) so both UIs and the tests share one renderer.

The table row, replacing the 0.10.0 one verbatim:

```go
{"/task", "task record: /task [show <id>|open|clear]", true},
```

`/task` renders the tree, `/task show <id>` one branch or report, `/task open` prints the path to the Markdown document, `/task clear` resets this session's record. `/task` stays busy-safe and view-local, exactly as 0.10.0's is (`renderLocalNote`/`renderLocalLines` in the TUI, plain `fmt.Println` in the REPL) — it is a private question about state, not a transcript entry every terminal needs.

`internal/engine/readme.go` holds the README written into `.be-code/tasks/` on first use, as a Go string constant. It documents: what the folder is, that it is untracked by default and how to commit it, the document format with a worked example, the five status marks, which fields a human may edit safely (text, status, reason, their own prose), what the engine repairs silently (ids out of step with position), what it quarantines (an unparseable document, with where the copy goes), and the one rule that matters for the model: exactly one node is `doing`.

- [ ] **Step 1: Write the failing tests**

```go
// internal/ui/repl_test.go
func TestPlainTaskCommands(t *testing.T) {
	r := newTestREPL(t)
	// No engine: the off message, and nothing that looks like a crash.
	out := capture(t, func() { r.command(context.Background(), "/task") })
	if !strings.Contains(out, "working memory is off (engine.enabled)") {
		t.Fatalf("off message: %q", out)
	}
	st := testStoreFor(t, r) // helper: opens a store on the REPL's root
	id := st.Plan("fix the parser", []string{"find the bug"})
	st.SetStatusText(id+".1", "doing", "")

	out = capture(t, func() { r.command(context.Background(), "/task") })
	if !strings.Contains(out, "fix the parser") || !strings.Contains(out, "find the bug") {
		t.Fatalf("tree listing: %q", out)
	}
	out = capture(t, func() { r.command(context.Background(), "/task show "+id) })
	if !strings.Contains(out, "fix the parser") {
		t.Fatalf("show: %q", out)
	}
	out = capture(t, func() { r.command(context.Background(), "/task open") })
	if !strings.Contains(out, filepath.Join(".be-code", "tasks")) {
		t.Fatalf("open should print the document path: %q", out)
	}
}

// internal/tui/engine_test.go
func TestTaskIsViewLocal(t *testing.T) {
	s := newTestSession(t)
	a, b := newTestView(t, s), newTestView(t, s)
	before := b.transcriptText()
	a.slashCommand("/task")
	if a.transcriptText() == before {
		t.Fatal("the asking view saw nothing")
	}
	if b.transcriptText() != before {
		t.Fatal("/task leaked into another terminal; it is a private question about state")
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/ui/ ./internal/tui/ -run 'TestPlainTask|TestTaskIsViewLocal'`
Expected: FAIL, undefined: `TaskLines`.

- [ ] **Step 3: Implement**

`ui.TaskLines` switches on `args[0]`: `""` → `TreeText()`, `show` → `ShowText(args[1])`, `open` → the document path, `clear` → handled by the caller because it mutates. The off message is the 0.10.0 string, unchanged: `working memory is off (engine.enabled)`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/ui/ ./internal/tui/ && make -f build.mk verify`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/ui/ internal/tui/ internal/engine/readme.go
git commit -m "ui: /task over the tree in both UIs, and the .be-code/tasks README"
```

---

## Task 7: Native Ollama chat

**Files:**
- Modify: `internal/provider/ollama.go`
- Create: `internal/provider/ollama_model.go`
- Test: `internal/provider/ollama_test.go` (extend; keep every existing test passing)

**Interfaces:**
- Consumes: `provider.ChatRequest`, `ChatResponse`, `ToolCall`, `StreamFunc` (unchanged).
- Produces:

```go
// Options carries what the harness controls on the native path. nil fields
// are omitted, so a zero Options sends {} and the server's defaults stand.
type Options struct {
	NumCtx      int     `json:"num_ctx,omitempty"`
	Temperature float64 `json:"temperature,omitempty"`
	NumPredict  int     `json:"num_predict,omitempty"`
	Extra       map[string]any `json:"-"` // merged in last, from config
}
func (p *Ollama) SetOptions(o Options)   // the loader is the only caller
func (p *Ollama) Options() Options
// ModelDetail is what /models needs to show a row worth reading.
type ModelDetail struct {
	ID           string
	SizeBytes    int64
	Family       string
	Quantization string
	Window       int  // 0 when unknown
	Resident     bool
}
func (p *Ollama) Details(ctx context.Context) ([]ModelDetail, error)
```

`Chat`'s routing rule inverts. Today: `if !req.NoThink || len(req.Tools) > 0 { return p.OpenAICompat.Chat(...) }`. After this task: native is the default, and the OpenAI path is the fallback.

```go
func (p *Ollama) Chat(ctx context.Context, req ChatRequest, onDelta StreamFunc) (*ChatResponse, error) {
	if p.nativeBroken.Load() {
		return p.OpenAICompat.Chat(ctx, req, onDelta)
	}
	resp, err := p.nativeChat(ctx, req, onDelta)
	if err != nil && isNativeUnsupported(err) {
		// One fallback per session, announced by the caller's notice path:
		// an older server is a reason to degrade, not to fail.
		p.nativeBroken.Store(true)
		return p.OpenAICompat.Chat(ctx, req, onDelta)
	}
	return resp, err
}
```

The native request body, with the keys Ollama actually reads:

```go
body := map[string]any{
	"model":    req.Model,
	"messages": nativeMessages(req.Messages), // role, content, tool_calls, tool_name
	"stream":   true,
	"options":  p.optionsMap(req),            // num_ctx, temperature, num_predict, then Extra
	"think":    !req.NoThink,
}
if len(req.Tools) > 0 {
	body["tools"] = nativeTools(req.Tools)    // [{"type":"function","function":{name,description,parameters}}]
}
if ka := p.keepAlive; ka != "" {
	body["keep_alive"] = ka
}
```

Streaming is NDJSON, one JSON object per line, not SSE: read with a `bufio.Scanner` at a 4 MiB buffer, decode each line into

```go
type nativeChunk struct {
	Message struct {
		Content   string `json:"content"`
		Thinking  string `json:"thinking"`
		ToolCalls []struct {
			Function struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"` // an object, not a string
			} `json:"function"`
		} `json:"tool_calls"`
	} `json:"message"`
	Done            bool   `json:"done"`
	DoneReason      string `json:"done_reason"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	Error           string `json:"error"`
}
```

Three differences from the OpenAI path that the tests must pin, because getting any of them wrong is silent:

1. **Tool arguments arrive as a JSON object**, not a string. `ToolCall.Arguments` is a raw JSON string, so marshal the object back: `string(tc.Function.Arguments)`, defaulting to `"{}"`.
2. **Tool calls are not fragmented** across chunks; each is complete. Do not reuse the index-merging logic.
3. **Ids are absent.** Synthesize `fmt.Sprintf("call_%d", n)` as the OpenAI path does for indexless servers, so `internal/agent` pairs results correctly.

`internal/provider/ollama_model.go` holds `Details` (merging `/api/tags` with `/api/ps`), and the existing `ContextLength`, `Status`, `Running`, `Warm`, `Pull`, moved out of `ollama.go` unchanged so the chat file stays readable.

- [ ] **Step 1: Write the failing tests**

```go
// TestNativeChatStreamsAndCarriesOptions: the normal path is native, the
// window we were told to use is on the wire, and deltas arrive as they
// stream rather than in one lump at the end.
func TestNativeChatStreamsAndCarriesOptions(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		fl, _ := w.(http.Flusher)
		for _, line := range []string{
			`{"message":{"content":"hel"},"done":false}`,
			`{"message":{"content":"lo"},"done":false}`,
			`{"message":{"content":""},"done":true,"done_reason":"stop","prompt_eval_count":11,"eval_count":2}`,
		} {
			w.Write([]byte(line + "\n"))
			fl.Flush()
		}
	}))
	defer srv.Close()
	p := NewOllama("t", srv.URL, "")
	p.SetOptions(Options{NumCtx: 32768})
	var deltas []string
	resp, err := p.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}},
		func(d string) { deltas = append(deltas, d) })
	if err != nil {
		t.Fatal(err)
	}
	if resp.Content != "hello" || resp.FinishReason != "stop" || resp.Usage.PromptTokens != 11 {
		t.Fatalf("resp: %+v", resp)
	}
	if len(deltas) < 2 {
		t.Fatalf("not streamed: %v", deltas)
	}
	opts, _ := gotBody["options"].(map[string]any)
	if opts["num_ctx"] != float64(32768) {
		t.Fatalf("num_ctx not sent: %v", gotBody["options"])
	}
	if gotBody["stream"] != true {
		t.Fatal("stream not requested")
	}
}

// TestNativeToolCallsDecodeObjectArguments: Ollama sends arguments as an
// object; the agent expects a JSON string. This is the conversion that
// breaks tool calling silently if it is wrong.
func TestNativeToolCallsDecodeObjectArguments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"message":{"tool_calls":[{"function":{"name":"read_file","arguments":{"path":"a.go"}}}]},"done":true,"done_reason":"stop"}` + "\n"))
	}))
	defer srv.Close()
	p := NewOllama("t", srv.URL, "")
	resp, err := p.Chat(context.Background(), ChatRequest{Model: "m",
		Tools: []ToolSpec{{Name: "read_file", Parameters: json.RawMessage(`{"type":"object"}`)}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "read_file" {
		t.Fatalf("tool calls: %+v", resp.ToolCalls)
	}
	if resp.ToolCalls[0].Arguments != `{"path":"a.go"}` {
		t.Fatalf("arguments: %q", resp.ToolCalls[0].Arguments)
	}
	if resp.ToolCalls[0].ID == "" {
		t.Fatal("a tool call with no id cannot be paired with its result")
	}
}

// TestFallsBackToOpenAIOnceWhenNativeIsUnsupported: an older server must
// degrade, not fail, and must not be re-probed on every request.
func TestFallsBackToOpenAIOnceWhenNativeIsUnsupported(t *testing.T) {
	var native, compat int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/chat":
			native++
			http.Error(w, "404 page not found", http.StatusNotFound)
		case "/v1/chat/completions":
			compat++
			w.Header().Set("Content-Type", "text/event-stream")
			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
		}
	}))
	defer srv.Close()
	p := NewOllama("t", srv.URL, "")
	for i := 0; i < 3; i++ {
		if _, err := p.Chat(context.Background(), ChatRequest{Model: "m"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if native != 1 || compat != 3 {
		t.Fatalf("native %d, compat %d; the fallback must latch", native, compat)
	}
}

// TestDetailsMergeTagsAndResident: /models needs size, quantization and
// whether the model is already loaded.
func TestDetailsMergeTagsAndResident(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			w.Write([]byte(`{"models":[{"name":"m","size":4700000000,"details":{"family":"qwen3","quantization_level":"Q4_K_M"}}]}`))
		case "/api/ps":
			w.Write([]byte(`{"models":[{"name":"m","model":"m","context_length":32768}]}`))
		}
	}))
	defer srv.Close()
	d, err := NewOllama("t", srv.URL, "").Details(context.Background())
	if err != nil || len(d) != 1 {
		t.Fatalf("details %+v err %v", d, err)
	}
	if !d[0].Resident || d[0].Window != 32768 || d[0].Quantization != "Q4_K_M" {
		t.Fatalf("row: %+v", d[0])
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/provider/ -run 'TestNative|TestFallsBack|TestDetails'`
Expected: FAIL, undefined: `SetOptions`, and the native path not taken.

- [ ] **Step 3: Implement**

Keep `TestOllamaNoThinkUsesNativeChat` and `TestOllamaDefaultStaysOnOpenAIPath` compiling by rewriting the second: the default is now native, so the test's name and assertion invert to `TestOllamaDefaultUsesNativeChat`. Say so in the commit message — a renamed test that asserts the opposite is exactly the kind of change a reviewer should see called out.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/provider/ -v && make -f build.mk verify`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/provider/
git commit -m "provider: native /api/chat is the normal Ollama path — streaming, tools, options; OpenAI stays as the fallback"
```

---

## Task 8: Config, the loader, and the consent gate

**Files:**
- Create: `internal/loader/loader.go`, `internal/loader/loader_test.go`
- Modify: `internal/config/config.go`, `internal/config/config_test.go`
- Modify: `cmd/root.go` (`applyBackendWindow` is replaced by the loader), `cmd/root_test.go`

**Interfaces:**
- Consumes: `provider.Ollama`, `provider.Options`, `tools.ApproveFunc`, `agent.Agent`.
- Produces:

```go
// config
type ProviderConfig struct {
	Type          string         `json:"type"`
	BaseURL       string         `json:"base_url"`
	APIKeyEnv     string         `json:"api_key_env,omitempty"`
	DefaultModel  string         `json:"default_model,omitempty"`
	ContextWindow int            `json:"context_window,omitempty"` // num_ctx the harness sends
	KeepAlive     string         `json:"keep_alive,omitempty"`
	Options       map[string]any `json:"options,omitempty"`
}
type ModelConfig struct {
	ContextWindow int            `json:"context_window,omitempty"`
	KeepAlive     string         `json:"keep_alive,omitempty"`
	Options       map[string]any `json:"options,omitempty"`
}
// on Config:
Models            map[string]ModelConfig `json:"models,omitempty"`
ReloadOnMismatch  string                 `json:"reload_on_mismatch,omitempty"` // ask | always | never; default "ask"
// EngineConfig gains:
ItemCap int `json:"item_cap"` // default 4096
NodeCap int `json:"node_cap"` // default 32768

// loader
type Params struct {
	Window    int
	KeepAlive time.Duration
	Options   map[string]any
}
type Loader struct {
	prov    provider.Provider
	cfg     *config.Config
	approve tools.ApproveFunc // nil means non-interactive: a refusal
	notice  func(string)
	mu      sync.Mutex
	agreed  map[string]bool // models the user has already said yes for
}
func New(p provider.Provider, cfg *config.Config, approve tools.ApproveFunc, notice func(string)) *Loader
func (l *Loader) Params(model string) Params
func (l *Loader) Apply(ctx context.Context, model string) (window int, err error)
func (l *Loader) OnEvicted(ctx context.Context, model string)
func (l *Loader) OnWindowChanged(model string, window int)
```

`Load()` fills `ItemCap` and `NodeCap` from zero the way it already rescues `Engine.Budget`, and defaults `ReloadOnMismatch` to `"ask"` when empty.

**`Apply` is the whole design in one function**, so its shape matters more than its length:

```go
// Apply resolves the model's parameters and makes them true on the server,
// asking first whenever that would change what another application on the
// box is using.
func (l *Loader) Apply(ctx context.Context, model string) (int, error) {
	p := l.Params(model)
	o, ok := l.prov.(*provider.Ollama)
	if !ok {
		return 0, nil
	}
	// An explicit window means no probe: the user told us the answer, and
	// probing can cost minutes when it has to load the model to find out.
	if p.Window > 0 {
		serverWindow, resident, _ := o.Status(ctx, model)
		switch {
		case !resident:
			// Nothing is holding the model, so loading it at our window
			// evicts nobody. No consent needed.
			o.SetOptions(provider.Options{NumCtx: p.Window, Extra: p.Options})
			return p.Window, nil
		case serverWindow == p.Window:
			o.SetOptions(provider.Options{NumCtx: p.Window, Extra: p.Options})
			return p.Window, nil
		default:
			return l.reconcile(ctx, model, serverWindow, p)
		}
	}
	// No configured window: read what the server has and fit ourselves to it.
	n, err := o.ContextLength(ctx, model)
	if err != nil || n == 0 {
		return 0, err
	}
	o.SetOptions(provider.Options{NumCtx: n, Extra: p.Options})
	return n, nil
}
```

`reconcile` is where consent lives. `never` keeps the server's window and notices once. `always` sends ours. `ask` calls `approve("model_reload", detail)` with the detail string

```
model %s is loaded with a %d-token window; config asks for %d.
Reloading evicts anything else on this server using that model.
```

and on refusal keeps the server's window for the session. **A nil `approve`, or a non-interactive run, is a refusal** — never a silent yes.

- [ ] **Step 1: Write the failing tests**

```go
// stub serves /api/ps and /api/show and counts what was asked of it.
func stub(t *testing.T, ps, show string) (*httptest.Server, *int32) {
	t.Helper()
	var shows int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/ps":
			w.Write([]byte(ps))
		case "/api/show":
			atomic.AddInt32(&shows, 1)
			w.Write([]byte(show))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &shows
}

const psLoaded8k = `{"models":[{"name":"m","model":"m","context_length":8192}]}`
const psEmpty = `{"models":[]}`

// TestExplicitWindowSkipsTheProbe: the user configured a window, so the
// loader must not ask the server what it should be — probing costs minutes
// when it has to load the model to find out.
func TestExplicitWindowSkipsTheProbe(t *testing.T) {
	srv, shows := stub(t, psEmpty, `{"parameters":"num_ctx 4096"}`)
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	l := New(provider.NewOllama("t", srv.URL, ""), cfg, refuse(t), func(string) {})
	w, err := l.Apply(context.Background(), "m")
	if err != nil || w != 32768 {
		t.Fatalf("window %d err %v", w, err)
	}
	if atomic.LoadInt32(shows) != 0 {
		t.Fatalf("probed %d times with an explicit window", *shows)
	}
}

// TestNotResidentNeedsNoConsent: nothing is holding the model, so loading it
// at our window evicts nobody and must not interrupt the user.
func TestNotResidentNeedsNoConsent(t *testing.T) {
	srv, _ := stub(t, psEmpty, `{}`)
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	l := New(provider.NewOllama("t", srv.URL, ""), cfg, refuse(t), func(string) {})
	if w, _ := l.Apply(context.Background(), "m"); w != 32768 {
		t.Fatalf("window %d", w)
	}
}

// TestMismatchAsksBeforeChangingASharedServer: the model is loaded at 8192
// and config wants 32768. Reloading would evict other applications, so the
// loader asks; a refusal keeps the server's window.
func TestMismatchAsksBeforeChangingASharedServer(t *testing.T) {
	srv, _ := stub(t, psLoaded8k, `{}`)
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	var asked string
	l := New(provider.NewOllama("t", srv.URL, ""), cfg,
		func(action, detail string) bool { asked = action + "|" + detail; return false },
		func(string) {})
	w, _ := l.Apply(context.Background(), "m")
	if !strings.HasPrefix(asked, "model_reload|") {
		t.Fatalf("did not ask: %q", asked)
	}
	if !strings.Contains(asked, "evicts") {
		t.Fatalf("the prompt must say what it costs: %q", asked)
	}
	if w != 8192 {
		t.Fatalf("a refusal must keep the server's window, got %d", w)
	}
}

// TestNonInteractiveNeverPrompts: a headless run has no one to ask, and a
// missing approver is a refusal, never a silent yes.
func TestNonInteractiveNeverPrompts(t *testing.T) {
	srv, _ := stub(t, psLoaded8k, `{}`)
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	l := New(provider.NewOllama("t", srv.URL, ""), cfg, nil, func(string) {})
	if w, _ := l.Apply(context.Background(), "m"); w != 8192 {
		t.Fatalf("window %d; a nil approver must mean no change", w)
	}
}

// TestUnsetWindowFitsTheServer: with nothing configured the server is the
// authority, which is 0.10.0's behaviour, now in one place.
func TestUnsetWindowFitsTheServer(t *testing.T) {
	srv, _ := stub(t, psLoaded8k, `{}`)
	l := New(provider.NewOllama("t", srv.URL, ""), config.Default(), refuse(t), func(string) {})
	if w, _ := l.Apply(context.Background(), "m"); w != 8192 {
		t.Fatalf("window %d", w)
	}
}

// TestPerModelOverridesTheProvider: the models map wins over the provider
// block, because parameters belong to the model.
func TestPerModelOverridesTheProvider(t *testing.T) {
	srv, _ := stub(t, psEmpty, `{}`)
	cfg := config.Default()
	cfg.Providers["ollama"] = config.ProviderConfig{Type: "ollama", BaseURL: srv.URL, ContextWindow: 16384}
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	l := New(provider.NewOllama("t", srv.URL, ""), cfg, refuse(t), func(string) {})
	if w, _ := l.Apply(context.Background(), "m"); w != 32768 {
		t.Fatalf("window %d", w)
	}
}

// TestWindowChangedByAnotherClientIsAdaptedTo: another client reloaded the
// model. We re-derive our budget and leave theirs alone — a reload war
// between two clients is the worst outcome available.
func TestWindowChangedByAnotherClientIsAdaptedTo(t *testing.T) {
	srv, _ := stub(t, psLoaded8k, `{}`)
	cfg := config.Default()
	cfg.Models = map[string]config.ModelConfig{"m": {ContextWindow: 32768}}
	var reloads int
	l := New(provider.NewOllama("t", srv.URL, ""), cfg,
		func(string, string) bool { reloads++; return true }, func(string) {})
	l.OnWindowChanged("m", 4096)
	if reloads != 0 {
		t.Fatalf("asked to reload %d times after someone else changed the window", reloads)
	}
}

// refuse fails the test if consent is ever requested.
func refuse(t *testing.T) tools.ApproveFunc {
	return func(action, detail string) bool {
		t.Fatalf("consent asked for when none was needed: %s %s", action, detail)
		return false
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/loader/ ./internal/config/`
Expected: FAIL, no such package `loader`.

- [ ] **Step 3: Implement config, then the loader, then rewire `cmd/root.go`**

Delete `applyBackendWindow` (root.go:345-377) and its startup call at root.go:262. In its place, `buildAgent` constructs the loader and calls `Apply` once for the resolved model. The four-minute timeout and the `loading %s to read its context window...` stall go with it: an explicit window never probes, and an unset one asks `/api/ps` and `/api/show` under a 10-second timeout, never `Warm`.

Keep the clamp warning's information, now only when the server wins:

```
warn: model %s runs with a %d-token window; budget clamped to %d.
      Set "context_window" for this model in config, or start the server with OLLAMA_CONTEXT_LENGTH=%d.
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/loader/ ./internal/config/ ./cmd/ -v && make -f build.mk verify`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/loader/ internal/config/ cmd/root.go cmd/root_test.go
git commit -m "loader: per-model parameters, an editable window, and consent before changing a shared server"
```

---

## Task 9: Model switching through the loader

**Files:**
- Modify: `internal/agent/loop.go` (`SetModel`), `internal/agent/resilience.go` (`checkBackend`), `internal/agent/handoff.go` (`ApplyWindow` call sites)
- Modify: `internal/tui/picker.go` (`/models` rows), `internal/ui/repl.go` (`/model`, `/models`)
- Modify: `cmd/root.go` (hand the loader to the agent)
- Test: `internal/agent/loader_test.go`, `internal/tui/picker_test.go`

**Interfaces:**
- Consumes: `loader.Loader` (Task 8).
- Produces:

```go
// agent
type ModelLoader interface {
	Apply(ctx context.Context, model string) (window int, err error)
	OnEvicted(ctx context.Context, model string)
	OnWindowChanged(model string, window int)
}
func (a *Agent) SetLoader(l ModelLoader)
```

`agent` takes an interface rather than importing `internal/loader`, the same import-cycle dodge `ReviewerFactory` and `CoworkerFactory` already use. `cmd/root.go` injects the real one.

**`SetModel` gains three lines and a goroutine.** The switch itself stays synchronous — profile, prompt, reserve, exactly as today — and the parameter resolution runs behind it:

```go
func (a *Agent) SetModel(model string) {
	a.applyModel(model)
	if a.History != nil {
		a.History.System.Content = a.composeSystem("")
		w := a.Window
		if w <= 0 {
			w = a.Cfg.ContextTokens
		}
		a.applyReserve(w)
	}
	// The loader may prompt and may reload a model, so it never runs on the
	// caller's goroutine: /model returns now, the window lands as a notice.
	if l := a.loader; l != nil {
		go a.resolveModel(l, model)
	}
}

func (a *Agent) resolveModel(l ModelLoader, model string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	w, err := l.Apply(ctx, model)
	if err != nil || w <= 0 {
		return
	}
	if a.ApplyWindow(w) {
		a.notice(fmt.Sprintf("%s runs with a %d-token window; budget now %d tokens", model, w, a.History.Budget))
	}
	// A smaller window than the conversation already occupies would make
	// the next request truncate silently: compact once, now, and say so.
	if a.History.Over() {
		a.notice("the new model's window is smaller than this conversation; compacting once")
		if err := a.Compact(ctx); err != nil {
			a.notice("compaction after the model switch failed: " + err.Error())
		}
	}
}
```

**`checkBackend` routes into the loader** instead of only adapting, per the spec's §10.1: an eviction calls `OnEvicted` (which re-applies our `num_ctx` for the reload that is coming anyway, no consent needed since nothing holds the model), and a window change calls `OnWindowChanged` (which adapts and only re-applies where consent already exists). The existing notice strings are unchanged.

**`/models` rows** use `provider.Details`: `fmt.Sprintf("%.1fGB %s %s · %s%s", gb, family, quant, windowText, residentText)` where `windowText` is `"32k ctx"` or `"ctx unknown"` and `residentText` is `" · loaded"` or `""`.

- [ ] **Step 1: Write the failing tests**

```go
// TestSetModelDoesNotBlockOnTheLoader: a loader that takes a second must
// not make /model take a second.
func TestSetModelDoesNotBlockOnTheLoader(t *testing.T) {
	ag := testAgent(t)
	ag.SetLoader(slowLoader{delay: time.Second})
	start := time.Now()
	ag.SetModel("other-model")
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("SetModel blocked for %s", d)
	}
}

// fakeLoader records what the agent asked of it and answers instantly.
type fakeLoader struct {
	window   int
	applied  []string
	evicted  []string
	changed  []int
	delay    time.Duration
}

func (f *fakeLoader) Apply(ctx context.Context, model string) (int, error) {
	time.Sleep(f.delay)
	f.applied = append(f.applied, model)
	return f.window, nil
}
func (f *fakeLoader) OnEvicted(ctx context.Context, model string) { f.evicted = append(f.evicted, model) }
func (f *fakeLoader) OnWindowChanged(model string, w int)         { f.changed = append(f.changed, w) }

// TestWindowFollowsTheModel: switching models re-derives the budget and the
// reserve from the new model's window, which 0.10.0 never did.
func TestWindowFollowsTheModel(t *testing.T) {
	ag := testAgent(t)
	ag.ApplyWindow(32768)
	l := &fakeLoader{window: 8192}
	ag.SetLoader(l)
	ag.SetModel("small-model")
	waitFor(t, func() bool { return ag.Window == 8192 })
	if ag.History.Budget > 8192 {
		t.Fatalf("budget %d exceeds the new window", ag.History.Budget)
	}
	if ag.History.Reserve == 0 {
		t.Fatal("reserve was not re-derived")
	}
}

// TestASmallerWindowCompactsOnce: rather than letting the next request
// truncate silently on the server.
func TestASmallerWindowCompactsOnce(t *testing.T) {
	ag := testAgent(t)
	ag.ApplyWindow(32768)
	fillHistory(t, ag, 20000) // tokens, comfortably over an 8k window
	var notices []string
	ag.Events.OnNotice = func(m string) { notices = append(notices, m) }
	ag.SetLoader(&fakeLoader{window: 8192})
	ag.SetModel("small-model")
	waitFor(t, func() bool { return !ag.History.Over() })
	if !containsAny(notices, "smaller than this conversation") {
		t.Fatalf("no notice about the compaction: %v", notices)
	}
}

// TestEvictionReloadsAtOurWindow: the backend-status trip routes through the
// loader, so the reload that is coming anyway uses our parameters. Nothing
// was holding the model, so this needs no consent.
func TestEvictionReloadsAtOurWindow(t *testing.T) {
	ag := testAgent(t)
	l := &fakeLoader{window: 32768}
	ag.SetLoader(l)
	ag.Provider = evictedProvider{} // Status reports loaded=false
	ag.checkBackend(context.Background())
	if len(l.evicted) != 1 {
		t.Fatalf("the loader was not told about the eviction: %+v", l)
	}
}

// TestAnotherClientsWindowChangeIsNotFought: we adapt to their window; we
// never reload back.
func TestAnotherClientsWindowChangeIsNotFought(t *testing.T) {
	ag := testAgent(t)
	l := &fakeLoader{window: 32768}
	ag.SetLoader(l)
	ag.ApplyWindow(32768)
	ag.Provider = windowChangedProvider{window: 4096} // Status reports 4096
	ag.checkBackend(context.Background())
	if ag.Window != 4096 {
		t.Fatalf("did not adapt: window %d", ag.Window)
	}
	if len(l.changed) != 1 || l.changed[0] != 4096 {
		t.Fatalf("loader not told: %+v", l.changed)
	}
	if len(l.applied) != 0 {
		t.Fatal("we reloaded back at our own window; that is a reload war")
	}
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./internal/agent/ -run 'TestSetModel|TestWindowFollows|TestASmaller|TestEviction|TestAnotherClients'`
Expected: FAIL, undefined: `SetLoader`.

- [ ] **Step 3: Implement**

- [ ] **Step 4: Run the tests**

Run: `go test -race ./internal/agent/ ./internal/tui/ ./cmd/ && make -f build.mk verify`
Expected: PASS. `-race` matters here: `resolveModel` writes the window from its own goroutine while the loop reads it.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/ internal/tui/picker.go internal/ui/repl.go cmd/root.go
git commit -m "agent: every model request goes through the loader — async switch, eviction reloads at our window"
```

---

## Task 10: End to end, docs, version

**Files:**
- Modify: `test/e2e/run_e2e.sh`, `test/e2e/mock_server.py`
- Modify: `README.md`, `CHANGELOG.md`, `build.mk`, `CLAUDE.md`
- Create: `docs/task-format.md`

**Interfaces:** none; this task proves and documents what Tasks 1–9 built.

- [ ] **Step 1: The end-to-end scenario**

Add a `task` scenario to `test/e2e/run_e2e.sh` in the existing style, in an isolated `HOME`. The scripted model, driven by `mock_server.py`:

1. Turn 1 — calls `task` with `{"action":"plan","text":"fix the parser","steps":["find the bug","fix it"]}`, then `{"action":"status","id":"1.1","status":"doing"}`, then a `shell` call whose scripted output contains `PARSER-SENTINEL-4F2A`.
2. The mock forces a compaction by returning an **empty** summary when asked, which is the VM failure this design is meant to survive.
3. Turn 2 — the scripted model asserts on its own system prompt: it prints `TREE:yes` only when the prompt contains both `fix the parser` and `PARSER-SENTINEL-4F2A`, proving the tree carried the work across a compaction that produced no summary.

The scenario must be non-vacuous, and the plan says how to prove it: run it once with `engine.enabled: false` in the scripted config and confirm it prints `TREE:no` and exits non-zero. A sentinel that appears in the guidance paragraph or in the echoed user message would pass without the tree, so **the sentinel must appear only in tool output**, never in a prompt or a request.

Assert `[PASS] task` and keep `E2E PASS`. Run the whole suite ten times consecutively; an intermittent pass is a fail.

- [ ] **Step 2: A second scenario for the native path**

Add a `native` scenario: a fake Ollama in `mock_server.py` that serves `/api/chat` as NDJSON and records the `options.num_ctx` it received. The scenario sets `context_window: 32768` in the scripted config and asserts the recorded value is `32768`, proving the configured window reaches the wire.

- [ ] **Step 3: Documentation**

`README.md` gains a **Task record** section (what `.be-code/tasks/` is, the five marks, `/task`, that it is untracked by default) and a **Context window** section (`context_window`, the `models` map, `reload_on_mismatch`, and the plain statement that changing a loaded model's window reloads it and evicts other users).

`docs/task-format.md` is the long form of the document format, which `.be-code/tasks/README.md` points at.

`CLAUDE.md`: replace the "Task engine (working memory)" section with one describing the tree, the recorder, the Markdown documents and the loader. Keep it factual about what the code does, in the voice of the surrounding sections.

`CHANGELOG.md`: a `## v0.11.0 — context handling` entry, one paragraph per spec section 4 through 10.

`build.mk`: `VERSION := 0.11.0`.

- [ ] **Step 4: Verify everything**

Run:
```bash
make -f build.mk verify && go test -race ./... && sh test/e2e/run_e2e.sh
```
Expected: green, `[PASS] task`, `[PASS] native`, `E2E PASS`.

- [ ] **Step 5: Commit**

```bash
git add test/e2e/ README.md CHANGELOG.md CLAUDE.md build.mk docs/task-format.md
git commit -m "docs/e2e: context handling (0.11.0) — a compaction with no summary keeps the thread"
```

---

## Notes for the executor

**Order matters.** Tasks 1 and 2 leave the package uncompilable until Task 3 replaces the old `store.go`; their test runs are deliberately scoped with `-run`. If you need a green build sooner, do Tasks 1 to 3 as one unit.

**The two tests that matter most** are the empty-summary end-to-end scenario (Task 10, Step 1) and `TestActiveWorkOutranksHistory` (Task 4). They encode the two failures this whole design exists to fix. If either becomes awkward, that is a signal about the design, not about the test.

**Every behaviour the spec's failure table names has a test.** If you find a row without one, write it rather than assuming it is covered elsewhere.
