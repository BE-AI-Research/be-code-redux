package tools

import (
	"context"
	"encoding/json"
	"regexp"
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
}

type taskTool struct{ l TaskLedger }

// NewTask builds the task tool over a ledger.
func NewTask(l TaskLedger) Tool { return &taskTool{l: l} }

func (t *taskTool) Name() string { return "task" }

func (t *taskTool) Description() string {
	return "Your task record. action=plan records a task and its steps; action=add adds a step or sub-step under one (parent: its id); action=status marks a node doing, done, blocked or dropped (with reason for the last two); action=note records a fact or decision against a node; action=show prints a node, a branch, or the whole tree. Ids are dotted paths like 2.1.3. What you do while a node is doing is recorded against it and survives compaction, so keep exactly one node doing."
}

func (t *taskTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"action":{"type":"string","enum":["plan","add","status","note","show"],"description":"plan | add | status | note | show"},
		"text":{"type":"string","description":"plan: the task in one line; add: the step; note: the fact or decision"},
		"steps":{"type":"array","items":{"type":"string"},"description":"plan: the steps in order"},
		"id":{"type":"string","description":"the node, a dotted path like 2.1.3"},
		"parent":{"type":"string","description":"add: the node to add under; omit for a new top-level task"},
		"status":{"type":"string","enum":["doing","done","blocked","dropped"],"description":"status: the new status"},
		"reason":{"type":"string","description":"status: why, for blocked and dropped"},
		"file":{"type":"string","description":"note: the file this note is about"},
		"keep":{"type":"boolean","description":"note: also remember this across sessions"}},
		"required":["action"]}`)
}

func (t *taskTool) Run(_ context.Context, args map[string]any) Result {
	action := strings.ToLower(strings.TrimSpace(argString(args, "action", "op", "do")))
	// "step" was 0.10.0's verb for both halves of what add and status now
	// do. Which one it meant is legible from the arguments: a status word
	// makes it a status change, anything else is a new node.
	if action == "step" {
		if argString(args, "status", "state") != "" {
			action = "status"
		} else {
			action = "add"
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
	}
	return Result{IsError: true, Content: "task needs an action (plan, add, status, note, show)"}
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
