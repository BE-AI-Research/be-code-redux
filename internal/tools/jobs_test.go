package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeJobs struct {
	calls  []string
	states []JobState
	err    error
	waitD  time.Duration
	scope  string
	paths  []string
	waitS  string
}

func (f *fakeJobs) DispatchJob(_ context.Context, who, job, scope, extra string, p []string) (string, error) {
	f.calls = append(f.calls, "dispatch "+who+"|"+job+"|"+extra)
	f.scope, f.paths = scope, p
	return "1.2", f.err
}
func (f *fakeJobs) JobStates() []JobState { return f.states }
func (f *fakeJobs) JobWait(_ context.Context, id string, d time.Duration) (string, error) {
	f.calls = append(f.calls, "wait "+id)
	f.waitD = d
	return f.waitS, f.err
}
func (f *fakeJobs) JobStop(id, r string) error {
	f.calls = append(f.calls, "stop "+id+" "+r)
	return f.err
}
func (f *fakeJobs) JobReply(id, t string) error {
	f.calls = append(f.calls, "reply "+id+" "+t)
	return f.err
}
func (f *fakeJobs) JobExtend(id string) error { f.calls = append(f.calls, "extend "+id); return f.err }

func TestJobToolSchemasFlat(t *testing.T) {
	f := &fakeJobs{}
	for tl, want := range map[Tool][]string{
		NewDispatch(f, nil, 0): {"who", "job", "scope", "context", "wait"},
		NewJobs(f, nil, 0):     {"action", "id", "for", "reason", "text"},
	} {
		var s struct {
			Properties map[string]struct {
				Type  string
				Items map[string]any
			}
		}
		if err := json.Unmarshal(tl.Schema(), &s); err != nil {
			t.Fatal(err)
		}
		if len(s.Properties) != len(want) {
			t.Fatalf("%s: %v", tl.Name(), s.Properties)
		}
		for _, k := range want {
			p, ok := s.Properties[k]
			if !ok || p.Type == "object" {
				t.Fatalf("%s: %s", tl.Name(), k)
			}
		}
	}
}

func TestDispatchWaitChainsAndRefusals(t *testing.T) {
	f := &fakeJobs{waitS: "done: x"}
	d := NewDispatch(f, nil, time.Minute)
	r := d.Run(context.Background(), map[string]any{"who": "big", "job": "do", "scope": []any{"a/", "b.go"}, "wait": true})
	if r.IsError || !strings.Contains(r.Content, "done: x") || f.waitD != time.Minute || f.scope != "a/, b.go" || len(f.paths) != 2 {
		t.Fatalf("%+v %+v", r, f)
	}
	f.err = errors.New("nope is not a co-worker")
	r = d.Run(context.Background(), map[string]any{"who": "nope", "job": "do"})
	if !r.IsError || r.Content != "nope is not a co-worker" {
		t.Fatalf("%+v", r)
	}
}

func TestJobsActions(t *testing.T) {
	f := &fakeJobs{}
	j := NewJobs(f, nil, 0)
	run := func(a map[string]any) Result { return j.Run(context.Background(), a) }
	if r := run(map[string]any{"action": "bogus"}); !r.IsError || r.Content != "action must be status, wait, stop, reply or extend" {
		t.Fatal(r)
	}
	if r := run(map[string]any{"action": "stop"}); !r.IsError {
		t.Fatal("stop without id")
	}
	if r := run(map[string]any{"action": "status"}); r.Content != "no jobs this session" {
		t.Fatal(r)
	}
	if r := run(map[string]any{"action": "wait"}); r.Content != "nothing is running" || f.waitD != defaultJobWait {
		t.Fatal(r, f.waitD)
	}
	run(map[string]any{"action": "wait", "for": 5.0})
	if f.waitD != 5*time.Second {
		t.Fatal(f.waitD)
	}
	run(map[string]any{"action": "stop", "id": "1", "reason": "x"})
	run(map[string]any{"action": "reply", "id": "1", "text": "yes"})
	run(map[string]any{"action": "extend", "id": "1"})
	if got := strings.Join(f.calls[len(f.calls)-3:], ","); got != "stop 1 x,reply 1 yes,extend 1" {
		t.Fatal(got)
	}
	f.states = []JobState{
		{ID: "1", Owner: "big", State: "running", Elapsed: 61500 * time.Millisecond, Calls: 4, At: "1.1", LastTool: "write_file", LastActivity: time.Now().Add(-10 * time.Second)},
		{ID: "2", Owner: "big", State: "done", Calls: 2, Summary: "all good"},
	}
	out := run(map[string]any{"action": "status"}).Content
	lines := strings.Split(out, "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "1  big  running  1m2s  4 calls  at 1.1  last write_file") || !strings.HasPrefix(lines[2], "    all good") {
		t.Fatalf("%q", out)
	}
}

func TestJobsFiredMatrix(t *testing.T) {
	f := &fakeJobs{}
	fired := func() bool { return true }
	d, j := NewDispatch(f, fired, 0), NewJobs(f, fired, 0)
	if r := d.Run(context.Background(), map[string]any{"who": "a", "job": "b"}); !r.IsError {
		t.Fatal("dispatch allowed")
	}
	for _, a := range []string{"stop", "reply", "extend"} {
		if r := j.Run(context.Background(), map[string]any{"action": a, "id": "1", "text": "x"}); !r.IsError || !strings.Contains(r.Content, "scheduled event") {
			t.Fatal(a, r)
		}
	}
	for _, a := range []string{"status", "wait"} {
		if r := j.Run(context.Background(), map[string]any{"action": a}); r.IsError {
			t.Fatal(a, r)
		}
	}
	if len(f.calls) != 1 {
		t.Fatal(f.calls)
	}
}
