package tools

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

var errNoteText = fmt.Errorf("note needs text")

type fakeLedger struct {
	task  string
	steps []string
	marks []string
	notes []string
	err   error
}

func (f *fakeLedger) SetPlan(task string, steps []string) { f.task, f.steps = task, steps }
func (f *fakeLedger) SetStep(i int, status string) error {
	f.marks = append(f.marks, strings.Repeat("x", i)+status)
	return f.err
}
func (f *fakeLedger) AddNote(text, file string, decision, keep bool) error {
	if text == "" {
		return errNoteText
	}
	f.notes = append(f.notes, text+"|"+file+"|"+boolStr(decision)+boolStr(keep))
	return nil
}
func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func TestTaskToolActionsAndAliases(t *testing.T) {
	f := &fakeLedger{}
	tool := NewTask(f)
	ctx := context.Background()
	if r := tool.Run(ctx, map[string]any{}); !r.IsError || r.Content != "task needs an action (plan, step, note)" {
		t.Fatalf("no action: %+v", r)
	}
	r := tool.Run(ctx, map[string]any{"action": "plan", "text": "add flag", "steps": "parse\nwire\n"})
	if r.IsError || f.task != "add flag" || len(f.steps) != 2 || !strings.Contains(r.Content, "2 steps") {
		t.Fatalf("plan: %+v %+v", r, f)
	}
	tool.Run(ctx, map[string]any{"action": "plan", "text": "t", "steps": []any{"a", "b", "c"}})
	if len(f.steps) != 3 {
		t.Fatal("array steps")
	}
	tool.Run(ctx, map[string]any{"action": "step", "step": 2, "status": "progress"})
	tool.Run(ctx, map[string]any{"action": "step", "step": "3", "status": "finished"})
	if f.marks[0] != "xxdoing" || f.marks[1] != "xxxdone" {
		t.Fatalf("marks %v", f.marks)
	}
	if r := tool.Run(ctx, map[string]any{"action": "step", "step": 1, "status": "bogus"}); !r.IsError || !strings.Contains(r.Content, "status must be doing, done or skip") {
		t.Fatalf("bad status: %+v", r)
	}
	tool.Run(ctx, map[string]any{"action": "decision", "text": "use cobra"})
	tool.Run(ctx, map[string]any{"action": "note", "text": "flags here", "file": "cmd/root.go", "remember": true})
	if f.notes[0] != "use cobra||10" || f.notes[1] != "flags here|cmd/root.go|01" {
		t.Fatalf("notes %v", f.notes)
	}
	if r := tool.Run(ctx, map[string]any{"action": "note"}); !r.IsError || r.Content != "note needs text" {
		t.Fatalf("empty note: %+v", r)
	}
	if !strings.Contains(tool.Description(), "plan") || !strings.Contains(string(tool.Schema()), `"action"`) {
		t.Fatal("description/schema")
	}
}
