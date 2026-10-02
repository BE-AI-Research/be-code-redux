package tools

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/schedule"
)

type fakeScheduler struct {
	added   []schedule.Request
	by      []string
	actions []string
}

func (f *fakeScheduler) AddSchedule(r schedule.Request, by string) (string, error) {
	f.added, f.by = append(f.added, r), append(f.by, by)
	return "scheduled " + r.Name, nil
}
func (f *fakeScheduler) ScheduleAction(action, name, by string) (string, error) {
	f.actions = append(f.actions, action+" "+name+" "+by)
	if name == "missing" {
		return "", errors.New("no schedule named missing")
	}
	return name + ": done", nil
}
func (f *fakeScheduler) ScheduleLines() []string { return []string{"NAME", "nightly"} }

func schedReg(t *testing.T) (*Registry, *fakeScheduler) {
	r, err := NewRegistry(t.TempDir(), func(a, d string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeScheduler{}
	r.AddTool(NewScheduleTool(f))
	return r, f
}

func TestScheduleToolAdd(t *testing.T) {
	r, f := schedReg(t)
	res := r.Dispatch(context.Background(), provider.ToolCall{ID: "1", Name: "schedule",
		Arguments: `{"action":"add","name":"check","when":"in 20m","instruction":"look at the build","task":"2","allow":["shell: go test ./..."]}`})
	if res.IsError || !strings.Contains(res.Content, "scheduled check") {
		t.Fatalf("%+v", res)
	}
	if len(f.added) != 1 || f.by[0] != "agent" || f.added[0].Allow[0] != "shell: go test ./..." || f.added[0].Task != "2" {
		t.Fatalf("%+v %v", f.added, f.by)
	}
}

func TestScheduleToolAcceptsAllowAsString(t *testing.T) {
	r, f := schedReg(t)
	r.Dispatch(context.Background(), provider.ToolCall{ID: "1", Name: "schedule",
		Arguments: `{"action":"add","name":"c","when":"in 5m","instruction":"x","allow":"shell: go test ./...; write: docs"}`})
	if len(f.added) != 1 || len(f.added[0].Allow) != 2 || f.added[0].Allow[1] != "write: docs" {
		t.Fatalf("%+v", f.added)
	}
}

func TestScheduleToolRefusedAfterUntrustedPage(t *testing.T) {
	r, f := schedReg(t)
	r.MarkUntrustedWeb()
	res := r.Dispatch(context.Background(), provider.ToolCall{ID: "1", Name: "schedule",
		Arguments: `{"action":"add","name":"c","when":"in 5m","instruction":"x"}`})
	if !res.IsError || len(f.added) != 0 || !strings.Contains(res.Content, "/schedule add") {
		t.Fatalf("%+v", res)
	}
	res = r.Dispatch(context.Background(), provider.ToolCall{ID: "2", Name: "schedule", Arguments: `{"action":"resume","name":"c"}`})
	if !res.IsError || len(f.actions) != 0 {
		t.Fatalf("resume widens too: %+v", res)
	}
	res = r.Dispatch(context.Background(), provider.ToolCall{ID: "3", Name: "schedule", Arguments: `{"action":"pause","name":"c"}`})
	if res.IsError {
		t.Fatalf("narrowing is still allowed: %+v", res)
	}
}

func TestScheduleToolListAndErrors(t *testing.T) {
	r, _ := schedReg(t)
	res := r.Dispatch(context.Background(), provider.ToolCall{ID: "1", Name: "schedule", Arguments: `{"action":"list"}`})
	if res.IsError || !strings.Contains(res.Content, "nightly") {
		t.Fatalf("%+v", res)
	}
	res = r.Dispatch(context.Background(), provider.ToolCall{ID: "2", Name: "schedule", Arguments: `{"action":"cancel","name":"missing"}`})
	if !res.IsError || !strings.Contains(res.Content, "no schedule") {
		t.Fatalf("%+v", res)
	}
	res = r.Dispatch(context.Background(), provider.ToolCall{ID: "3", Name: "schedule", Arguments: `{"action":"run","name":"x"}`})
	if !res.IsError {
		t.Fatal("run is a person's command, not the model's")
	}
}

func TestScheduleToolNotInSubsets(t *testing.T) {
	r, _ := schedReg(t)
	for _, sub := range []*Registry{r.Subset("read_file", "list_dir", "search"), r.Scoped(nil, nil, "s")} {
		for _, n := range sub.Names() {
			if n == "schedule" {
				t.Fatal("plan mode and sub-agents never get the schedule tool")
			}
		}
	}
}
