package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// JobState is one job as the jobs tool reports it (dispatch-jobs spec §4):
// a running job's live state, or a finished one's outcome. Declared here,
// not in internal/agent, because the agent imports tools and never the
// reverse; Agent.JobStates returns it.
type JobState struct {
	// ID is the job's step id; Owner the co-worker running it. State is
	// running | waiting (<reason>) | asking | stalled | done | blocked |
	// stopped.
	ID, Owner, State string
	Elapsed          time.Duration
	Calls            int
	// At is the doing node inside the job's subtree (the job's own id when
	// nothing narrower is known); LastTool and LastActivity are its last
	// sign of life.
	At, LastTool string
	LastActivity time.Time
	// Summary is a finished job's hand-back summary, or its reason when it
	// ended without one.
	Summary string
}

// JobRunner is what the dispatch and jobs tools drive; *agent.Agent
// satisfies it (tools never imports agent).
type JobRunner interface {
	DispatchJob(ctx context.Context, who, job, scope, extra string, scopePaths []string) (string, error)
	JobStates() []JobState
	JobWait(ctx context.Context, id string, d time.Duration) (string, error)
	JobStop(id, reason string) error
	JobReply(id, text string) error
	JobExtend(id string) error
}

// errFiredJobs is the sentence task owner gives during a scheduled event.
const errFiredJobs = "sub-agents cannot be assigned or scoped during a scheduled event; ask the person to do it after this event"

const defaultJobWait = 30 * time.Minute

type dispatchTool struct {
	r       JobRunner
	fired   func() bool
	waitFor time.Duration
}

// NewDispatch builds the dispatch tool; waitFor bounds a wait: true call
// (0 means 30 minutes).
func NewDispatch(r JobRunner, fired func() bool, waitFor time.Duration) Tool {
	if waitFor <= 0 {
		waitFor = defaultJobWait
	}
	return &dispatchTool{r: r, fired: fired, waitFor: waitFor}
}

func (t *dispatchTool) Name() string { return "dispatch" }

func (t *dispatchTool) Description() string {
	return "Hand a scoped job to a co-worker, which runs it on its own and reports back in a later message. Name the co-worker, the job, and the paths it may write. Do not edit inside a running job's scope. Use jobs to look at, wait for, stop, steer or extend it."
}

func (t *dispatchTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"who":{"type":"string","description":"Co-worker name"},
		"job":{"type":"string","description":"What to do, complete enough to work from"},
		"scope":{"type":"array","items":{"type":"string"},"description":"Workspace paths the co-worker may write"},
		"context":{"type":"string","description":"Extra background for the co-worker"},
		"wait":{"type":"boolean","description":"Wait for the job to finish before returning"}},
		"required":["who","job"]}`)
}

func (t *dispatchTool) Run(ctx context.Context, args map[string]any) Result {
	if t.fired != nil && t.fired() {
		return Result{IsError: true, Content: errFiredJobs}
	}
	who := strings.TrimSpace(argString(args, "who", "name", "coworker"))
	job := strings.TrimSpace(argString(args, "job", "task"))
	if who == "" || job == "" {
		return Result{IsError: true, Content: "dispatch needs who and job"}
	}
	scope := argStrings(args, "scope")
	id, err := t.r.DispatchJob(ctx, who, job, strings.Join(scope, ", "), argString(args, "context"), scope)
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	if !argBool(args, false, "wait") {
		return Result{Content: "dispatched job " + id + " to " + who}
	}
	out, err := t.r.JobWait(ctx, id, t.waitFor)
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	return Result{Content: "dispatched job " + id + " to " + who + "\n" + out}
}

type jobsTool struct {
	r       JobRunner
	fired   func() bool
	waitFor time.Duration
}

// NewJobs builds the jobs tool; waitFor is the default for wait.
func NewJobs(r JobRunner, fired func() bool, waitFor time.Duration) Tool {
	if waitFor <= 0 {
		waitFor = defaultJobWait
	}
	return &jobsTool{r: r, fired: fired, waitFor: waitFor}
}

func (t *jobsTool) Name() string { return "jobs" }

func (t *jobsTool) Description() string {
	return "Look at and steer jobs handed to co-workers. status lists them; wait blocks until one reports (id optional, for = seconds); stop ends one (reason); reply answers its question (text); extend gives a quiet or capped job more time."
}

func (t *jobsTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"action":{"type":"string","enum":["status","wait","stop","reply","extend"]},
		"id":{"type":"string","description":"Job id"},
		"for":{"type":"integer","description":"Seconds to wait (wait)"},
		"reason":{"type":"string","description":"Why (stop)"},
		"text":{"type":"string","description":"The answer (reply)"}},
		"required":["action"]}`)
}

func (t *jobsTool) Run(ctx context.Context, args map[string]any) Result {
	action := strings.ToLower(strings.TrimSpace(argString(args, "action")))
	id := strings.TrimSpace(argString(args, "id"))
	switch action {
	case "status", "wait":
	case "stop", "reply", "extend":
		if t.fired != nil && t.fired() {
			return Result{IsError: true, Content: errFiredJobs}
		}
	default:
		return Result{IsError: true, Content: "action must be status, wait, stop, reply or extend"}
	}
	fail := func(err error) Result { return Result{IsError: true, Content: err.Error()} }
	switch action {
	case "status":
		states := t.r.JobStates()
		if len(states) == 0 {
			return Result{Content: "no jobs this session"}
		}
		var b strings.Builder
		for i, s := range states {
			if i > 0 {
				b.WriteByte('\n')
			}
			fmt.Fprintf(&b, "%s  %s  %s  %s  %d calls", s.ID, s.Owner, s.State, s.Elapsed.Round(time.Second), s.Calls)
			if s.At != "" {
				b.WriteString("  at " + s.At)
			}
			if s.LastTool != "" {
				ago := ""
				if !s.LastActivity.IsZero() {
					ago = " " + time.Since(s.LastActivity).Round(time.Second).String() + " ago"
				}
				b.WriteString("  last " + s.LastTool + ago)
			}
			if s.Summary != "" {
				b.WriteString("\n    " + strings.ReplaceAll(strings.TrimSpace(s.Summary), "\n", "\n    "))
			}
		}
		return Result{Content: b.String()}
	case "wait":
		d := t.waitFor
		if n := argInt(args, 0, "for"); n > 0 {
			d = time.Duration(n) * time.Second
		}
		out, err := t.r.JobWait(ctx, id, d)
		if err != nil {
			return fail(err)
		}
		if out == "" {
			out = "nothing is running"
		}
		return Result{Content: out}
	case "stop":
		if id == "" {
			return Result{IsError: true, Content: "jobs stop needs an id"}
		}
		if err := t.r.JobStop(id, argString(args, "reason")); err != nil {
			return fail(err)
		}
		return Result{Content: "stopped " + id}
	case "reply":
		text := argString(args, "text")
		if id == "" || strings.TrimSpace(text) == "" {
			return Result{IsError: true, Content: "jobs reply needs an id and text"}
		}
		if err := t.r.JobReply(id, text); err != nil {
			return fail(err)
		}
		return Result{Content: "replied to " + id}
	default:
		if id == "" {
			return Result{IsError: true, Content: "jobs extend needs an id"}
		}
		if err := t.r.JobExtend(id); err != nil {
			return fail(err)
		}
		return Result{Content: "extended " + id}
	}
}
