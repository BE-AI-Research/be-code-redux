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

	tools  []Tool
	byName map[string]Tool
	procs  *ProcessManager
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
// only the named tools — used by plan mode for a read-only phase.
func (r *Registry) Subset(names ...string) *Registry {
	sub := &Registry{
		Root: r.Root, Approve: r.Approve, ApproveWrites: r.ApproveWrites,
		OnBeforeWrite: r.OnBeforeWrite, ShellAllow: r.ShellAllow,
		ShellDeny: r.ShellDeny, Hooks: r.Hooks,
		byName: map[string]Tool{}, procs: r.procs,
	}
	for _, n := range names {
		if t, ok := r.byName[n]; ok {
			sub.tools = append(sub.tools, t)
			sub.byName[n] = t
		}
	}
	return sub
}

// Scoped is a sub-agent's registry: reads anywhere, writes under scope,
// shell only for the project's own checks, and none of the tools that
// reach outside the workspace (process, consult, web, editor, MCP). It
// shares the main registry's approval seam, so a write is approved and
// checkpointed exactly as the main model's is.
func (r *Registry) Scoped(scope, checks []string, label string) *Registry {
	sub := r.Subset("read_file", "write_file", "edit_file", "list_dir", "search", "shell",
		"lookup", "history", "show", "changes")
	// A scoped registry drives no model-parameter resolution of its own: it
	// runs no loader. Leaving ApproveCtx set would let a caller bypass the
	// "sub-agent <label>:" prefix below by asking through that seam instead.
	sub.ApproveCtx = nil
	sub.ReviewWrite = r.ReviewWrite
	sub.ReviewInvolvesEditor = r.ReviewInvolvesEditor
	sub.EditorName = r.EditorName
	sub.OnStatus = r.OnStatus
	sub.scoped = true
	sub.scope, sub.checks = append([]string(nil), scope...), checks
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
	// lookup/history/show/changes are shared by reference, bound to the
	// PARENT registry, not sub: that is safe only because all four are
	// read-only today. If a future git tool can write, it must be rebound
	// to sub (like the six above) before it can be added here — sharing it
	// by reference would let a sub-agent write through the parent's
	// confinement instead of its own. TestScopedToolsAreBoundOrReadOnlyAllowlisted
	// pins this.
	for _, n := range []string{"lookup", "history", "show", "changes"} {
		if t, ok := r.byName[n]; ok {
			sub.add(t)
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

// Close shuts down background resources (running processes).
// Close stops background processes and shuts down attached MCP servers.
func (r *Registry) Close() {
	r.procs.StopAll()
	for _, c := range r.mcpClients {
		c.Close()
	}
	r.mcpClients = nil
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
