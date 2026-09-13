// Package tools implements the agent's tool set: filesystem, search, and
// shell. The design goal is small-model friendliness — few tools, flat
// argument schemas, and forgiving argument parsing — because local models
// are far more likely to emit slightly-wrong tool calls than frontier ones.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/mcp"
	"github.com/brown-enterprises/be-code/internal/provider"
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
	// MaxOutput bounds the bytes any single tool result returns to the
	// model. Scaled to the backend window by agent.ApplyWindow.
	MaxOutput  int
	mcpClients []*mcp.Client
	Approve    ApproveFunc
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
	r := &Registry{MaxOutput: defaultMaxOutput, Root: abs, Approve: approve, byName: map[string]Tool{}, procs: NewProcessManager()}
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
	args := map[string]any{}
	raw := strings.TrimSpace(call.Arguments)
	if raw != "" && raw != "null" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			// Local models sometimes double-encode arguments as a JSON string.
			var s string
			if err2 := json.Unmarshal([]byte(raw), &s); err2 == nil {
				if err3 := json.Unmarshal([]byte(s), &args); err3 != nil {
					return badArgs(call.Name, err)
				}
			} else {
				return badArgs(call.Name, err)
			}
		}
	}
	return t.Run(ctx, args)
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
