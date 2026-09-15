package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// TaskLedger is what the task tool writes to: the engine's store, behind an
// interface so tools never imports engine.
type TaskLedger interface {
	SetPlan(task string, steps []string)
	SetStep(i int, status string) error
	AddNote(text, file string, decision, keep bool) error
}

type taskTool struct{ l TaskLedger }

// NewTask builds the task tool over a ledger.
func NewTask(l TaskLedger) Tool { return &taskTool{l: l} }

func (t *taskTool) Name() string { return "task" }

func (t *taskTool) Description() string {
	return "Keep your working memory. action=plan records the task and its steps before a multi-step change; action=step marks a step doing/done/skip as you go; action=note records a fact or decision (with file: what matters in that file, so you need not read it again; with keep: remember it for future sessions). The ledger is always shown to you under Working memory."
}

func (t *taskTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"action":{"type":"string","enum":["plan","step","note"],"description":"plan | step | note"},
		"text":{"type":"string","description":"plan: the task in one line; note: the fact or decision"},
		"steps":{"type":"array","items":{"type":"string"},"description":"plan: the steps in order"},
		"step":{"type":"integer","description":"step: 1-based step number"},
		"status":{"type":"string","enum":["doing","done","skip"],"description":"step: the new status"},
		"file":{"type":"string","description":"note: the file this note is about"},
		"keep":{"type":"boolean","description":"note: also remember this across sessions"}},
		"required":["action"]}`)
}

func (t *taskTool) Run(_ context.Context, args map[string]any) Result {
	action := strings.ToLower(strings.TrimSpace(argString(args, "action", "op", "do")))
	switch action {
	case "plan":
		steps := argStrings(args, "steps")
		if len(steps) == 0 {
			if s := argString(args, "steps"); s != "" {
				for _, line := range strings.Split(s, "\n") {
					if line = strings.TrimSpace(strings.TrimLeft(line, "-*0123456789.) ")); line != "" {
						steps = append(steps, line)
					}
				}
			}
		}
		t.l.SetPlan(argString(args, "text", "task", "title"), steps)
		return Result{Content: fmt.Sprintf("plan recorded (%d steps)", len(steps))}
	case "step":
		i := argInt(args, 0, "step", "n", "index")
		status := strings.ToLower(strings.TrimSpace(argString(args, "status", "state")))
		switch status {
		case "doing", "progress", "in_progress", "started":
			status = "doing"
		case "done", "finished", "complete", "completed":
			status = "done"
		case "skip", "skipped":
			status = "skip"
		default:
			return Result{IsError: true, Content: "status must be doing, done or skip"}
		}
		if err := t.l.SetStep(i, status); err != nil {
			return Result{IsError: true, Content: err.Error()}
		}
		return Result{Content: fmt.Sprintf("step %d: %s", i, status)}
	case "note", "decision", "fact":
		keep := argBool(args, false, "keep", "remember", "durable")
		if err := t.l.AddNote(argString(args, "text", "note", "fact"), argString(args, "file", "path"), action == "decision", keep); err != nil {
			return Result{IsError: true, Content: err.Error()}
		}
		return Result{Content: "noted"}
	}
	return Result{IsError: true, Content: "task needs an action (plan, step, note)"}
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
