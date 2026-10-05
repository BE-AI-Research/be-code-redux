// Package tools implements the agent's tool set: filesystem, search, and
// shell. The design goal is small-model friendliness — few tools, flat
// argument schemas, and forgiving argument parsing — because local models
// are far more likely to emit slightly-wrong tool calls than frontier ones.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brown-enterprises/be-code/internal/mcp"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/subagent"
)

const timeSecond = time.Second

// Result is a tool execution outcome.
type Result struct {
	Content string // returned to the model
	IsError bool
}

// Tool is one callable capability.
type Tool interface {
	Name() string
	Description() string
	Schema() json.RawMessage
	Run(ctx context.Context, args map[string]any) Result
}

// ApproveFunc asks the user to approve a dangerous action; returns false to deny.
type ApproveFunc func(action, detail string) bool

// ApproveCtxFunc is ApproveFunc for an asker that can give up on its own
// question — a model-parameter resolution under a deadline. A UI that
// implements it withdraws the prompt when ctx ends, through whatever
// withdrawal path it already has, so a question nobody answered does not
// sit on every attached terminal after the goroutine behind it has gone.
// Returning false on a withdrawal is right: nobody consented.
type ApproveCtxFunc func(ctx context.Context, action, detail string) bool

// ReviewDecision is the outcome of an editor-side review of a file change.
type ReviewDecision int

const (
	ReviewUnavailable ReviewDecision = iota // editor could not review: fall back to Approve
	ReviewAccept
	ReviewReject
	ReviewAcceptAll // accept and stop asking for the rest of the session
	// ReviewCancelled: this reviewer was withdrawn because the other place
	// answered first (see internal/review). Never returned to fs.go.
	ReviewCancelled
)

// Registry holds the active tool set, rooted at a workspace directory.
type Registry struct {
	Root string // absolute workspace root; all paths confined here
	// maxOutput bounds the bytes any single tool result returns to the
	// model. Scaled to the backend window by agent.ApplyWindow, which
	// since model switching went through the loader can run on a goroutine
	// of its own while tools are reading this — hence the atomic and the
	// accessors rather than a plain field.
	maxOutput  atomic.Int64
	mcpClients []*mcp.Client
	Approve    ApproveFunc
	// ApproveCtx, when set, is preferred by callers that have a context to
	// give — today the model loader. Tools use Approve: a tool call's
	// cancellation already reaches them another way.
	ApproveCtx ApproveCtxFunc
	// ApproveWrites gates write_file/edit_file behind a review of the
	// change before it lands. It is the single switch for BOTH review
	// paths: when set, the editor is asked first if ReviewWrite is wired
	// (an in-editor diff), otherwise — or when the editor cannot answer —
	// the terminal approval prompt (action "file_write") runs. Clearing
	// it therefore stops editor diffs as well as terminal prompts, which
	// is what "accept all / don't ask again" must do. Shell approval is
	// separate and always on unless the approver auto-approves.
	ApproveWrites bool
	// OnBeforeWrite runs after approval, before a file is modified —
	// the checkpoint hook. A returned error aborts the write.
	OnBeforeWrite func(absPath string) error
	// ReviewWrite, when set, is asked first for file changes (an editor
	// diff review). ReviewUnavailable falls back to Approve. ctx is the
	// tool call's context, so cancelling the run (Esc) also abandons a
	// review the user has left sitting in the editor.
	ReviewWrite func(ctx context.Context, rel, oldContent, newContent string) ReviewDecision
	// ReviewInvolvesEditor, when set, reports whether the next ReviewWrite
	// will actually put the change in front of the editor. It exists only so
	// the "reviewing change in VS Code…" status is not shown for a review
	// that never reaches VS Code (review mode "tui", or no editor attached).
	// nil means yes, preserving the behaviour for callers that wire
	// ReviewWrite to an editor and nothing else.
	ReviewInvolvesEditor func() bool
	// EditorName is what to call the editor a review is shown in ("Visual
	// Studio"); empty means VS Code, the only editor there was before lock
	// files said which one they belonged to.
	EditorName string
	// OnStatus receives short progress notes for the UI's status line.
	OnStatus func(msg string)
	// OnNotice, when set, puts a lasting line on the transcript from any
	// goroutine (the outcome of /browser attach).
	OnNotice func(msg string)
	// ShellAllow / ShellDeny are glob patterns matched against shell
	// commands. Deny wins; an allow match skips the approval prompt.
	ShellAllow []string
	ShellDeny  []string
	// Hooks: "post_write" commands run after a successful file write
	// ($FILE substituted); "pre_shell" run before an approved shell
	// command ($COMMAND substituted). Hook failures are reported to the
	// model but do not roll back the triggering action.
	Hooks map[string][]string

	// scoped is true only for a registry Scoped built. Both confinement
	// guards (checkScope, the shell tool's checks gate) key off this, not
	// off scope/checks being nil — a nil scope or nil checks on a scoped
	// registry must fail CLOSED (refuse everything), not fall open to the
	// main registry's behaviour. scope and checks confine a sub-agent's
	// registry (spec §2.4): writes only under scope, shell only for exactly
	// one of checks.
	scoped bool
	// scopeMu guards scope alone. A running sub-agent's scope is widened
	// from the agent goroutine (Agent.SetScope, answering an ask_main) while
	// the sub-agent's own goroutine is reading it in checkScope, so the
	// slice is replaced under this lock and never mutated in place.
	scopeMu sync.RWMutex
	scope   []string
	checks  []string
	// parent is the registry Scoped was built from: a sub-agent's check
	// consults the parent's untrusted-web flag, which is the request's.
	parent *Registry

	tools  []Tool
	byName map[string]Tool
	procs  *ProcessManager

	// onClose runs when the registry closes: a tool that owns something
	// outside the process (the browser tool's launched browser) registers
	// its teardown here from attach.
	onClose []func()
	// untrustedWeb is set when this request has shown the model a page from
	// a host not in the browser's allow tier; while it is set every shell
	// command asks as "shell_after_web" (spec §3.6, amended). Only a request
	// a person typed clears it (Agent.BeginTypedRequest); a resumed history
	// holding a page sets it again.
	untrustedWeb atomic.Bool
	// fired is a scheduled event's allowance while its turn runs
	// (allowance.go). Never copied by Subset or Scoped.
	fired atomic.Pointer[firedPolicy]
	// share is the share_page gate and its session grants (share.go), on
	// the primary registry only: Subset and Scoped read their parent's.
	share shareState
}

// NewRegistry builds the standard tool set rooted at dir.
func NewRegistry(dir string, approve ApproveFunc) (*Registry, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	r := &Registry{Root: abs, Approve: approve, byName: map[string]Tool{}, procs: NewProcessManager()}
	r.SetMaxOutput(defaultMaxOutput)
	r.add(
		&readFileTool{r},
		&writeFileTool{r},
		&editFileTool{r},
		&listDirTool{r},
		&searchTool{r},
		&shellTool{r},
		&processTool{r},
	)
	return r, nil
}

// Subset returns a registry sharing this one's root/approval but exposing
// only the named tools — used by plan mode for a read-only phase, and by
// Scoped to build a sub-agent's registry. ApproveCtx is deliberately left
// unset: a scratch or sub-agent registry drives no model-parameter
// resolution of its own.
func (r *Registry) Subset(names ...string) *Registry {
	sub := &Registry{
		Root: r.Root, Approve: r.Approve, ApproveWrites: r.ApproveWrites,
		OnBeforeWrite: r.OnBeforeWrite, ShellAllow: r.ShellAllow,
		ShellDeny: r.ShellDeny, Hooks: r.Hooks,
		ReviewWrite: r.ReviewWrite, ReviewInvolvesEditor: r.ReviewInvolvesEditor,
		EditorName: r.EditorName, OnStatus: r.OnStatus,
		// parent lets UntrustedWeb() (and, for a Scoped registry, the scoped
		// shell path) see the request's flag even though the browser tool
		// that sets it is never itself exposed through Subset or Scoped.
		parent: r,
		byName: map[string]Tool{}, procs: r.procs,
	}
	sub.maxOutput.Store(r.maxOutput.Load())
	for _, n := range names {
		t, ok := r.byName[n]
		if !ok {
			continue
		}
		// A built-in tool holds a pointer back to the registry it was
		// constructed with, so sharing the parent's instance would run every
		// call against the PARENT's fields (Approve, ShellAllow/Deny, and a
		// scheduled event's allowance) rather than this subset's own —
		// silently defeating the point of a read-only or restricted subset
		// the moment it is asked to expose one of these. Rebind it to sub;
		// anything else (an MCP tool, a git lookup, …) is still shared by
		// reference, as before.
		if rebuilt, ok := rebindBuiltinTool(n, sub); ok {
			t = rebuilt
		}
		sub.tools = append(sub.tools, t)
		sub.byName[n] = t
	}
	return sub
}

// rebindBuiltinTool constructs a fresh instance of one of the built-in tools
// bound to sub. Subset calls this for every name it is asked for — including
// on Scoped's behalf, since Scoped gets its registry from Subset too — so
// there is exactly one place that decides which tools must be rebound. ok is
// false for anything else (an MCP tool, a git lookup, …): those are shared
// by reference from the parent, which TestScopedToolsAreBoundOrReadOnlyAllowlisted
// pins to an explicit read-only allowlist for Scoped.
func rebindBuiltinTool(name string, sub *Registry) (Tool, bool) {
	switch name {
	case "read_file":
		return &readFileTool{r: sub}, true
	case "write_file":
		return &writeFileTool{r: sub}, true
	case "edit_file":
		return &editFileTool{r: sub}, true
	case "list_dir":
		return &listDirTool{r: sub}, true
	case "search":
		return &searchTool{r: sub}, true
	case "shell":
		return &shellTool{r: sub}, true
	case "process":
		return &processTool{r: sub}, true
	default:
		return nil, false
	}
}

// Scoped is a sub-agent's registry: reads anywhere, writes under scope,
// shell only for the project's own checks, and none of the tools that
// reach outside the workspace (process, consult, web, editor, MCP). It
// shares the main registry's approval seam, so a write is approved and
// checkpointed exactly as the main model's is.
func (r *Registry) Scoped(scope, checks []string, label string) *Registry {
	// Subset rebinds read_file/write_file/edit_file/list_dir/search/shell to
	// the returned registry (rebindBuiltinTool) so confinement reads
	// sub.scope; lookup/history/show/changes are not built-ins, so they come
	// back shared by reference from the PARENT registry — safe only because
	// all four are read-only today. If a future git tool can write, it must
	// be added to rebindBuiltinTool before it can be listed here — sharing
	// it by reference would let a sub-agent write through the parent's
	// confinement instead of its own. TestScopedToolsAreBoundOrReadOnlyAllowlisted
	// pins this.
	sub := r.Subset("read_file", "write_file", "edit_file", "list_dir", "search", "shell",
		"lookup", "history", "show", "changes")
	sub.scoped = true
	sub.scope, sub.checks = append([]string(nil), scope...), checks
	if r.Approve != nil {
		parent := r.Approve
		sub.Approve = func(action, detail string) bool {
			return parent(action, "sub-agent "+label+":\n"+detail)
		}
	}
	return sub
}

// checkScope refuses a write outside a scoped registry's scope. It keys
// off r.scoped, not off r.scope being non-nil: a scoped registry with a
// nil or empty scope must refuse every write (fail closed), never fall
// back to the main registry's unconfined behaviour, which is what a bare
// "r.scope == nil" check would do for a node with no assigned scope yet.
//
// It also resolves symlinks along the way. resolve() only checks the path
// textually, so a symlink already inside the scope (created before the
// sub-agent was scoped in, or by the sub-agent itself through a shell
// check) can point anywhere; os.WriteFile follows it. realExistingPath
// walks up to the nearest ancestor that actually exists (a write may be
// creating new path components, which cannot be resolved because they are
// not there yet) and resolves symlinks from there, so what gets checked
// against Root and scope is where the write actually lands.
func (r *Registry) checkScope(absPath string) error {
	if !r.scoped {
		return nil
	}
	scope := r.Scope()
	denied := fmt.Errorf("path is outside your scope (%s); use ask_main if you need it widened", strings.Join(scope, ", "))
	real, err := realExistingPath(absPath)
	if err != nil {
		// A dangling symlink is still a path outside what this sub-agent may
		// write; the raw lstat error reads as a harness bug to a model that
		// only needs to know it may not go there.
		return denied
	}
	root := r.Root
	if rr, err := filepath.EvalSymlinks(r.Root); err == nil {
		root = rr
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return denied
	}
	if !subagent.InScope(scope, filepath.ToSlash(rel)) {
		return denied
	}
	return nil
}

// Scope is a scoped registry's current write scope, copied out under the
// lock so a caller can hold it while the agent widens the real one.
func (r *Registry) Scope() []string {
	r.scopeMu.RLock()
	defer r.scopeMu.RUnlock()
	return append([]string(nil), r.scope...)
}

// SetScope replaces a scoped registry's write scope. It is the other half
// of spec §2.7's "task action: scope … widens the scope and resolves the
// ask": without it the registry was built once from a copy of the slice, so
// a widened scope changed nothing for the running sub-agent — which was
// then told its scope had been widened, wrote the file it had asked about,
// was refused, and had already spent its one ask.
func (r *Registry) SetScope(scope []string) {
	r.scopeMu.Lock()
	r.scope = append([]string(nil), scope...)
	r.scopeMu.Unlock()
}

// realExistingPath resolves symlinks in absPath, walking up to the nearest
// ancestor that exists (the target itself, or its parent directories, may
// not exist yet — a write can create them) and rejoining whatever of the
// original path had not been created yet onto the resolved ancestor. A
// path with no existing ancestor at all (unreachable in practice: Root
// itself always exists) is returned as given.
func realExistingPath(absPath string) (string, error) {
	p := absPath
	var tail []string
	for {
		if _, err := os.Lstat(p); err == nil {
			real, err := filepath.EvalSymlinks(p)
			if err != nil {
				return "", err
			}
			for i := len(tail) - 1; i >= 0; i-- {
				real = filepath.Join(real, tail[i])
			}
			return real, nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return absPath, nil
		}
		tail = append(tail, filepath.Base(p))
		p = parent
	}
}

// AddTool registers an externally-provided tool (e.g. an MCP server tool).
func (r *Registry) AddTool(t Tool) {
	if ra, ok := t.(registryAware); ok {
		ra.attach(r)
	}
	r.add(t)
}

// registryAware tools get a pointer to their registry on AddTool (for
// MaxOutput and confinement settings).
type registryAware interface{ attach(r *Registry) }

// MaxOutput is the per-call cap on bytes returned to the model.
func (r *Registry) MaxOutput() int { return int(r.maxOutput.Load()) }

// SetMaxOutput rescales that cap. Called from agent.applyReserve whenever
// the window changes, including from a model switch's own goroutine.
func (r *Registry) SetMaxOutput(n int) { r.maxOutput.Store(int64(n)) }

// Close stops background processes, runs the tools' own teardown (the
// browser's) and shuts down attached MCP servers.
func (r *Registry) Close() {
	r.procs.StopAll()
	for _, fn := range r.onClose {
		fn()
	}
	for _, c := range r.mcpClients {
		c.Close()
	}
	r.mcpClients = nil
}

// MarkUntrustedWeb records that this request has read an untrusted page.
func (r *Registry) MarkUntrustedWeb() { r.untrustedWeb.Store(true) }

// UntrustedWeb reports whether this request has read an untrusted page —
// its own flag, or (Subset and Scoped both set parent) its parent's: a
// Subset sharing shell/process with the main registry must still suspend
// auto-approval after the primary request read an untrusted page (browser
// spec §3.6), and the browser tool that sets the flag is never itself
// exposed through Subset or Scoped, so a subset registry can only ever
// learn this from its parent.
func (r *Registry) UntrustedWeb() bool {
	return r.untrustedWeb.Load() || (r.parent != nil && r.parent.UntrustedWeb())
}

// ClearUntrustedWeb puts the flag down. Agent.BeginTypedRequest is its one
// caller: a request a person typed, never a hand-back.
func (r *Registry) ClearUntrustedWeb() { r.untrustedWeb.Store(false) }

// Browser is the registered browser tool, or nil when it is off.
func (r *Registry) Browser() *BrowserTool {
	bt, _ := r.byName["browser"].(*BrowserTool)
	return bt
}

func (r *Registry) add(ts ...Tool) {
	for _, t := range ts {
		r.tools = append(r.tools, t)
		r.byName[t.Name()] = t
	}
}

// Specs returns provider-level tool specs.
func (r *Registry) Specs() []provider.ToolSpec {
	out := make([]provider.ToolSpec, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, provider.ToolSpec{
			Name: t.Name(), Description: t.Description(), Parameters: t.Schema(),
		})
	}
	return out
}

// Names lists tool names.
func (r *Registry) Names() []string {
	out := make([]string, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t.Name())
	}
	return out
}

// Dispatch executes a tool call, tolerating loosely-formed arguments.
func (r *Registry) Dispatch(ctx context.Context, call provider.ToolCall) Result {
	t, ok := r.byName[call.Name]
	if !ok {
		return Result{IsError: true, Content: fmt.Sprintf(
			"unknown tool %q; available tools: %s", call.Name, strings.Join(r.Names(), ", "))}
	}
	args, err := parseArgs(call.Arguments)
	if err != nil {
		return badArgs(call.Name, err)
	}
	if r.Fired() && !firedExempt(t) && !r.allowTool(call.Name) {
		if !r.ask(withoutInherit(ctx), "tool_call", toolCallDetail(call.Name, args), false) {
			return Result{IsError: true, Content: fmt.Sprintf(
				"%s needs a person's approval during a scheduled event, and none was given", call.Name)}
		}
	}
	return t.Run(ctx, args)
}

// ParseArgs decodes a tool call's arguments with exactly the tolerance
// Dispatch applies, so anything that inspects a call before or after it
// (the working-memory engine's lookup cache and its observer) sees the
// same arguments the tool itself ran with: empty or "null" is an empty
// object, and a JSON string that itself holds an object is unwrapped,
// because local models sometimes double-encode their arguments. ok is
// false only for arguments no tool could have run.
func ParseArgs(raw string) (map[string]any, bool) {
	args, err := parseArgs(raw)
	return args, err == nil
}

// parseArgs is ParseArgs keeping the decode error, which Dispatch quotes
// back to the model.
func parseArgs(raw string) (map[string]any, error) {
	args := map[string]any{}
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return args, nil
	}
	if err := json.Unmarshal([]byte(raw), &args); err != nil {
		// Local models sometimes double-encode arguments as a JSON string.
		var s string
		if err2 := json.Unmarshal([]byte(raw), &s); err2 != nil {
			return nil, err
		}
		if err3 := json.Unmarshal([]byte(s), &args); err3 != nil {
			return nil, err
		}
	}
	return args, nil
}

func badArgs(name string, err error) Result {
	return Result{IsError: true, Content: fmt.Sprintf(
		"tool %s: arguments were not valid JSON (%v). Re-issue the call with a single JSON object.", name, err)}
}

// resolve confines a user/model-supplied path inside the workspace root.
// This mirrors the path-traversal hardening lessons from BE-CLI.
func (r *Registry) resolve(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("path is required")
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(r.Root, p)
	}
	p = filepath.Clean(p)
	rel, err := filepath.Rel(r.Root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q is outside the workspace root %s", p, r.Root)
	}
	return p, nil
}

// ---- loose argument helpers ------------------------------------------------

func argString(args map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := args[k]; ok {
			switch t := v.(type) {
			case string:
				return t
			case float64:
				return fmt.Sprintf("%v", t)
			}
		}
	}
	return ""
}

func argInt(args map[string]any, def int, keys ...string) int {
	for _, k := range keys {
		if v, ok := args[k]; ok {
			switch t := v.(type) {
			case float64:
				return int(t)
			case int:
				return t
			case string:
				var n int
				if _, err := fmt.Sscanf(t, "%d", &n); err == nil {
					return n
				}
			}
		}
	}
	return def
}

func argBool(args map[string]any, def bool, keys ...string) bool {
	for _, k := range keys {
		if v, ok := args[k]; ok {
			switch t := v.(type) {
			case bool:
				return t
			case string:
				return t == "true" || t == "yes" || t == "1"
			case float64:
				// Small models often send 1/0 where the schema says boolean.
				return t != 0
			}
		}
	}
	return def
}

func schema(s string) json.RawMessage { return json.RawMessage(s) }

// runHooks executes the configured hook commands for an event, with one
// $VAR substituted. Returns a note describing failures ("" when clean).
func (r *Registry) runHooks(ctx context.Context, event, varName, varValue string) string {
	var notes []string
	for _, h := range r.Hooks[event] {
		cmd := strings.ReplaceAll(h, "$"+varName, varValue)
		out, err := RunShell(ctx, r.Root, cmd, 60*timeSecond)
		if err != nil {
			first := strings.SplitN(strings.TrimSpace(out+" "+err.Error()), "\n", 2)[0]
			notes = append(notes, fmt.Sprintf("[%s hook failed: %s: %s]", event, cmd, first))
		}
	}
	return strings.Join(notes, "\n")
}

// argStringPresent is argString that also reports whether any of the keys
// was actually supplied, so callers can tell "" from "missing".
func argStringPresent(args map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if v, ok := args[k]; ok && v != nil {
			return argString(args, k), true
		}
	}
	return "", false
}

// truncate limits tool output so a single call can't blow the context
// budget of a small local model.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + fmt.Sprintf("\n... [truncated %d of %d bytes; narrow the request to see more]", len(s)-max, len(s))
}

func (r *Registry) editorName() string {
	if r.EditorName == "" {
		return "VS Code"
	}
	return r.EditorName
}
