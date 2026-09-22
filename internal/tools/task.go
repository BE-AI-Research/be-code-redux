package tools

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// stepMarkerRe matches a numbering or bullet marker at the start of a plan
// step line — "1. ", "2) ", "- ", "* " — but never a bare leading digit, so
// step text like "2FA setup" or "404 handling" survives intact.
var stepMarkerRe = regexp.MustCompile(`^\s*(?:\d+[.)]|[-*])\s+`)

// TaskLedger is what the task tool writes to: the engine's task tree,
// behind an interface so tools never imports engine. SetStatusText and
// ShowText exist so the interface speaks only strings — the tool never
// needs engine's Status type to satisfy it.
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

// taskRoots is an optional capability on a ledger: naming the task in
// flight is what lets the tool resolve a 0.10.0 step number against the
// right task. The engine's store provides it; a ledger that does not (the
// no-op one) simply leaves such a number as the model wrote it. Optional
// capabilities are declared as small interfaces and type-asserted, the way
// the agent treats a backend's extras.
type taskRoots interface{ ActiveRootID() string }

type taskTool struct {
	l     TaskLedger
	under string
}

// NewTask builds the task tool over a ledger.
func NewTask(l TaskLedger) Tool { return &taskTool{l: l} }

// NewTaskUnder is the task tool a sub-agent gets: add, status, note and
// show, only for nodes under root.
func NewTaskUnder(l TaskLedger, root string) Tool { return &taskTool{l: l, under: root} }

func (t *taskTool) Name() string { return "task" }

func (t *taskTool) Description() string {
	return "Your task record. action=plan records a task and its steps; action=add adds a step or sub-step under one (parent: its id); action=status marks a node doing, done, blocked or dropped (with reason for the last two); action=note records a fact or decision against a node; action=show prints a node, a branch, or the whole tree. Ids are dotted paths like 2.1.3. What you do while a node is doing is recorded against it and survives compaction, so keep exactly one node doing."
}

func (t *taskTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"action":{"type":"string","enum":["plan","add","status","note","show","owner","scope","reply"],"description":"plan | add | status | note | show | owner (assign a step to a sub-agent) | scope (paths it may write) | reply (answer a sub-agent's question)"},
		"text":{"type":"string","description":"plan: the task in one line; add: the step; note: the fact or decision; reply: the answer"},
		"steps":{"type":"array","items":{"type":"string"},"description":"plan: the steps in order"},
		"id":{"type":"string","description":"the node, a dotted path like 2.1.3"},
		"parent":{"type":"string","description":"add: the node to add under; omit for a new top-level task"},
		"status":{"type":"string","enum":["doing","done","blocked","dropped"],"description":"status: the new status"},
		"reason":{"type":"string","description":"status: why, for blocked and dropped"},
		"file":{"type":"string","description":"note: the file this note is about"},
		"decision":{"type":"boolean","description":"note: this is a decision, not just a fact"},
		"keep":{"type":"boolean","description":"note: also remember this across sessions"},
		"owner":{"type":"string","description":"owner: a sub-agent's name, or main"},
		"paths":{"type":"array","items":{"type":"string"},"description":"scope: workspace paths the owner may write"}},
		"required":["action"]}`)
}

func (t *taskTool) Run(_ context.Context, args map[string]any) Result {
	action := strings.ToLower(strings.TrimSpace(argString(args, "action", "op", "do")))
	// "step" was 0.10.0's verb for both halves of what add and status now
	// do. Which one it meant is legible from the arguments: a status word
	// makes it a status change, anything else is a new node.
	legacyStep := action == "step"
	if legacyStep {
		if argString(args, "status", "state") != "" {
			action = "status"
		} else {
			action = "add"
		}
	}
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
	switch action {
	case "plan":
		id := t.l.Plan(argString(args, "text", "task", "title"), planSteps(args))
		return Result{Content: "task " + id}
	case "add":
		id, err := t.l.Add(argString(args, "parent", "under", "parent_id"), argString(args, "text", "step", "task", "title"))
		if err != nil {
			return Result{IsError: true, Content: err.Error()}
		}
		return Result{Content: "task " + id}
	case "status":
		status, ok := normalizeStatus(argString(args, "status", "state"))
		if !ok {
			return Result{IsError: true, Content: "status must be doing, done, blocked or dropped"}
		}
		id := argString(args, "id", "node", "step", "task")
		if legacyStep {
			id = t.resolveStepNumber(id)
		}
		if strings.TrimSpace(id) == "" {
			return Result{IsError: true, Content: "status needs an id (a dotted path like 2.1.3)"}
		}
		if err := t.l.SetStatusText(id, status, argString(args, "reason", "why", "note")); err != nil {
			return Result{IsError: true, Content: err.Error()}
		}
		return Result{Content: id + ": " + status}
	case "note", "decision", "fact":
		keep := argBool(args, false, "keep", "remember", "durable")
		err := t.l.Note(
			argString(args, "id", "node", "step", "task"),
			argString(args, "text", "note", "fact", "decision"),
			argString(args, "file", "path"),
			action == "decision" || argBool(args, false, "decision"),
			keep,
		)
		if err != nil {
			return Result{IsError: true, Content: err.Error()}
		}
		return Result{Content: "noted"}
	case "show", "list", "tree":
		return Result{Content: t.l.ShowText(argString(args, "id", "node", "step", "task"))}
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
		switch {
		case len(paths) == 0:
			// argStrings only reads a JSON array; a plain comma list arrives
			// as one string, which argString does read.
			if s := argString(args, "paths", "scope", "files"); s != "" {
				paths = splitCommaList(s)
			}
		case len(paths) == 1 && strings.Contains(paths[0], ","):
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
	}
	return Result{IsError: true, Content: "task needs an action (plan, add, status, note, show)"}
}

// splitCommaList splits a comma-separated string, trimming and dropping
// empty entries — the plain-string form of "paths" a small model sends
// alongside the JSON-array form the schema documents.
func splitCommaList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// resolveStepNumber turns a 0.10.0 step number into the dotted id of that
// step under the task in flight. It only ever fires for a step-shaped call
// carrying a bare number: to the tree "2" is root 2, which is some other
// task entirely, so marking it done would close the wrong work. Without a
// task in flight — or a ledger that can name one — the number is left as
// written rather than guessed at.
func (t *taskTool) resolveStepNumber(id string) string {
	id = strings.TrimSpace(id)
	if id == "" || strings.ContainsRune(id, '.') {
		return id
	}
	if _, err := strconv.Atoi(id); err != nil {
		return id
	}
	r, ok := t.l.(taskRoots)
	if !ok {
		return id
	}
	root := r.ActiveRootID()
	if root == "" {
		return id
	}
	return root + "." + id
}

// normalizeStatus folds what a small model actually emits onto the words
// the tree knows. It is deliberately wider than the schema's enum: a model
// that writes "completed" meant done, and refusing it only costs a turn.
func normalizeStatus(s string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "doing", "progress", "in_progress", "in-progress", "started", "start", "active":
		return "doing", true
	case "done", "finished", "complete", "completed", "ok":
		return "done", true
	case "blocked", "stuck", "blocked_on", "blocked-on":
		return "blocked", true
	case "dropped", "drop", "skip", "skipped", "cancelled", "canceled", "abandoned":
		return "dropped", true
	case "todo", "open", "pending", "reopen":
		return "todo", true
	}
	return "", false
}

// planSteps reads the steps of a plan. The model is as likely to send one
// newline-separated string as a JSON array, so both are accepted and the
// numbering or bullet it wrote in front of each line is stripped.
func planSteps(args map[string]any) []string {
	if steps := argStrings(args, "steps", "plan", "items"); len(steps) > 0 {
		out := make([]string, 0, len(steps))
		for _, s := range steps {
			if s = strings.TrimSpace(stepMarkerRe.ReplaceAllString(s, "")); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	var out []string
	for _, line := range strings.Split(argString(args, "steps", "plan", "items"), "\n") {
		if line = strings.TrimSpace(stepMarkerRe.ReplaceAllString(line, "")); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// argStrings reads a string array argument (a single string is not one).
func argStrings(args map[string]any, keys ...string) []string {
	for _, k := range keys {
		if v, ok := args[k].([]any); ok {
			var out []string
			for _, e := range v {
				if s, ok := e.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, strings.TrimSpace(s))
				}
			}
			return out
		}
	}
	return nil
}
