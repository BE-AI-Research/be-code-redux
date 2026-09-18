package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// recTree records what the task tool asked the ledger to do, in the order
// it asked. It is the whole contract: the tool's job is to turn a small
// model's approximate JSON into exactly these five calls.
type recTree struct {
	plans   []string
	steps   [][]string
	adds    [][2]string
	status  [][3]string
	notes   []string
	showArg string
	err     error
}

func (r *recTree) Plan(text string, steps []string) string {
	r.plans = append(r.plans, text)
	r.steps = append(r.steps, steps)
	return "1"
}

func (r *recTree) Add(parent, text string) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	r.adds = append(r.adds, [2]string{parent, text})
	return "1.1", nil
}

func (r *recTree) SetStatusText(id, status, reason string) error {
	if r.err != nil {
		return r.err
	}
	r.status = append(r.status, [3]string{id, status, reason})
	return nil
}

func (r *recTree) Note(id, text, file string, decision, keep bool) error {
	if r.err != nil {
		return r.err
	}
	r.notes = append(r.notes, strings.Join([]string{id, text, file, boolStr(decision), boolStr(keep)}, "|"))
	return nil
}

func (r *recTree) ShowText(id string) string { r.showArg = id; return "tree text" }

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func runTask(t *testing.T, tool Tool, args string) Result {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(args), &m); err != nil {
		t.Fatalf("test args %s: %v", args, err)
	}
	return tool.Run(context.Background(), m)
}

// TestTaskVerbs: every verb reaches the ledger with its arguments intact.
func TestTaskVerbs(t *testing.T) {
	r := &recTree{}
	tool := NewTask(r)
	if res := runTask(t, tool, `{"action":"plan","text":"fix the parser","steps":["find it","fix it"]}`); res.IsError {
		t.Fatalf("plan: %+v", res)
	} else if res.Content != "task 1" {
		t.Fatalf("plan must return the new id so the model can address it: %q", res.Content)
	}
	if len(r.plans) != 1 || r.plans[0] != "fix the parser" || !reflect.DeepEqual(r.steps[0], []string{"find it", "fix it"}) {
		t.Fatalf("plan args: %v %v", r.plans, r.steps)
	}
	if res := runTask(t, tool, `{"action":"add","parent":"1","text":"read lexer.go"}`); res.IsError ||
		r.adds[0] != [2]string{"1", "read lexer.go"} {
		t.Fatalf("add: %+v %v", res, r.adds)
	} else if res.Content != "task 1.1" {
		t.Fatalf("add must return the new id: %q", res.Content)
	}
	if res := runTask(t, tool, `{"action":"status","id":"1.1","status":"dropped","reason":"not needed"}`); res.IsError ||
		r.status[0] != [3]string{"1.1", "dropped", "not needed"} {
		t.Fatalf("status: %+v %v", res, r.status)
	}
	if res := runTask(t, tool, `{"action":"note","id":"1.1","text":"the lexer is generated","file":"lexer.go","keep":true}`); res.IsError ||
		r.notes[0] != "1.1|the lexer is generated|lexer.go|0|1" {
		t.Fatalf("note: %+v %v", res, r.notes)
	}
	if res := runTask(t, tool, `{"action":"show","id":"1"}`); res.IsError || r.showArg != "1" {
		t.Fatalf("show: %+v %q", res, r.showArg)
	} else if res.Content != "tree text" {
		t.Fatalf("show must return what the ledger rendered: %q", res.Content)
	}
	// show with no id is the whole tree, not an error.
	if res := runTask(t, tool, `{"action":"show"}`); res.IsError || r.showArg != "" {
		t.Fatalf("show all: %+v %q", res, r.showArg)
	}
}

// TestTaskActionAliasesAndErrors: the forgiving half. A missing action says
// what the verbs are, and a ledger error reaches the model verbatim rather
// than being swallowed.
func TestTaskActionAliasesAndErrors(t *testing.T) {
	r := &recTree{}
	tool := NewTask(r)
	if res := runTask(t, tool, `{}`); !res.IsError ||
		res.Content != "task needs an action (plan, add, status, note, show)" {
		t.Fatalf("no action: %+v", res)
	}
	// decision and fact are note.
	runTask(t, tool, `{"action":"decision","text":"use cobra"}`)
	runTask(t, tool, `{"action":"fact","text":"flags live in cmd/root.go","file":"cmd/root.go"}`)
	if r.notes[0] != "|use cobra||1|0" || r.notes[1] != "|flags live in cmd/root.go|cmd/root.go|0|0" {
		t.Fatalf("note aliases: %v", r.notes)
	}
	// "step" is the 0.10.0 spelling of add; it must not be an error.
	if res := runTask(t, tool, `{"action":"add","text":"a top-level task"}`); res.IsError ||
		r.adds[len(r.adds)-1] != [2]string{"", "a top-level task"} {
		t.Fatalf("add without a parent: %+v %v", res, r.adds)
	}
	if res := runTask(t, tool, `{"action":"status","id":"1","status":"bogus"}`); !res.IsError ||
		!strings.Contains(res.Content, "status must be doing, done, blocked or dropped") {
		t.Fatalf("bad status: %+v", res)
	}
	r.err = fmt.Errorf("no node 9.9")
	if res := runTask(t, tool, `{"action":"status","id":"9.9","status":"done"}`); !res.IsError || res.Content != "no node 9.9" {
		t.Fatalf("ledger error: %+v", res)
	}
	if res := runTask(t, tool, `{"action":"note","id":"9.9","text":"x"}`); !res.IsError || res.Content != "no node 9.9" {
		t.Fatalf("ledger error on note: %+v", res)
	}
	if res := runTask(t, tool, `{"action":"add","text":"x"}`); !res.IsError || res.Content != "no node 9.9" {
		t.Fatalf("ledger error on add: %+v", res)
	}
}

// TestStatusAliasesSmallModelsActuallyEmit: a local model writes
// "completed" and "skipped" as often as the words we chose.
func TestStatusAliasesSmallModelsActuallyEmit(t *testing.T) {
	r := &recTree{}
	tool := NewTask(r)
	for _, in := range []struct{ raw, want string }{
		{"completed", "done"}, {"in_progress", "doing"}, {"skipped", "dropped"}, {"stuck", "blocked"},
		{"complete", "done"}, {"finished", "done"}, {"progress", "doing"}, {"started", "doing"},
		{"skip", "dropped"}, {"blocked_on", "blocked"}, {"DONE", "done"}, {" doing ", "doing"},
	} {
		tool.Run(context.Background(), map[string]any{"action": "status", "id": "1", "status": in.raw})
		if len(r.status) == 0 {
			t.Fatalf("%q reached nothing", in.raw)
		}
		got := r.status[len(r.status)-1][1]
		if got != in.want {
			t.Fatalf("%q became %q, want %q", in.raw, got, in.want)
		}
	}
}

// TestPlanAcceptsANewlineBlob: the model often sends one string, not an
// array, and the tool must not punish it for that. Markers come off; a
// bare leading digit is text, not a marker.
func TestPlanAcceptsANewlineBlob(t *testing.T) {
	r := &recTree{}
	NewTask(r).Run(context.Background(), map[string]any{
		"action": "plan", "text": "a task", "steps": "1. one\n2) 2FA setup\n- 404 handling\n* four\n3D printer\n",
	})
	if len(r.plans) != 1 {
		t.Fatalf("plans: %v", r.plans)
	}
	want := []string{"one", "2FA setup", "404 handling", "four", "3D printer"}
	if !reflect.DeepEqual(r.steps[0], want) {
		t.Fatalf("marker stripping: got %v want %v", r.steps[0], want)
	}
}

// TestTaskDescriptionAndSchema pins the words the model is given: the five
// verbs, the dotted ids, and the one rule that makes the record work.
func TestTaskDescriptionAndSchema(t *testing.T) {
	tool := NewTask(&recTree{})
	d := tool.Description()
	for _, want := range []string{"action=plan", "action=add", "action=status", "action=note", "action=show",
		"dotted paths like 2.1.3", "keep exactly one node doing"} {
		if !strings.Contains(d, want) {
			t.Fatalf("description lacks %q:\n%s", want, d)
		}
	}
	var sch struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(tool.Schema(), &sch); err != nil {
		t.Fatalf("schema is not JSON: %v", err)
	}
	for _, want := range []string{"action", "text", "steps", "id", "parent", "status", "reason", "file", "keep"} {
		if _, ok := sch.Properties[want]; !ok {
			t.Fatalf("schema lacks %q: %s", want, tool.Schema())
		}
	}
	if !reflect.DeepEqual(sch.Required, []string{"action"}) {
		t.Fatalf("required: %v", sch.Required)
	}
}
