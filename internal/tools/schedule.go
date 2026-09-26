package tools

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/brown-enterprises/be-code/internal/schedule"
)

// Scheduler is what the schedule tool needs from the agent (which tools
// may not import): implemented by *agent.Agent.
type Scheduler interface {
	AddSchedule(req schedule.Request, by string) (string, error)
	ScheduleAction(action, name, by string) (string, error)
	ScheduleLines() []string
}

type scheduleTool struct {
	s Scheduler
	r *Registry
}

// NewScheduleTool is the model's handle on scheduled events (schedules
// spec §4.1). Registered only in interactive sessions; never in Subset or
// Scoped registries, which list their tools by name.
func NewScheduleTool(s Scheduler) Tool { return &scheduleTool{s: s} }

func (t *scheduleTool) attach(r *Registry) { t.r = r }

func (t *scheduleTool) Name() string { return "schedule" }

func (t *scheduleTool) Description() string {
	return "Schedule a request for yourself later in this session, or keep a recurring one for the project. " +
		"Use it for follow-ups (\"check the build again in 20m\") and recurring upkeep a person asked for — " +
		"never to put off work the current request asks for now. A person approves every schedule and its " +
		"allowance before it exists; ask for the narrowest allowance that does the job. " +
		"when: in 20m · at 09:00 · at 2026-09-27 09:00 · every 30m · daily 09:00 · weekdays 09:00 · mon,thu 14:30 · cron. " +
		"allow: grants like \"shell: go test ./...\", \"write: docs\", \"browser: example.com\"; anything else asks a person when it fires."
}

func (t *scheduleTool) Schema() json.RawMessage {
	return schema(`{"type":"object","properties":{
		"action":{"type":"string","enum":["add","list","pause","resume","cancel"]},
		"name":{"type":"string","description":"lowercase-with-dashes"},
		"when":{"type":"string"},
		"instruction":{"type":"string","description":"what to do when it fires, written as a request to yourself"},
		"task":{"type":"string","description":"optional task id to work under, e.g. 3.2"},
		"allow":{"type":"array","items":{"type":"string"}}},
		"required":["action"]}`)
}

func (t *scheduleTool) Run(_ context.Context, args map[string]any) Result {
	action := strings.ToLower(strings.TrimSpace(argString(args, "action")))
	name := strings.TrimSpace(argString(args, "name"))
	untrusted := t.r != nil && t.r.UntrustedWeb()
	var msg string
	var err error
	switch action {
	case "add":
		if untrusted {
			return Result{IsError: true, Content: "a web page was read during this request, so schedules cannot be created until a person's next request; tell the user what you wanted to schedule — they can add it with /schedule add"}
		}
		allow := argStrings(args, "allow")
		if allow == nil {
			for _, g := range strings.FieldsFunc(argString(args, "allow"), func(r rune) bool { return r == ';' || r == '\n' }) {
				if g = strings.TrimSpace(g); g != "" {
					allow = append(allow, g)
				}
			}
		}
		msg, err = t.s.AddSchedule(schedule.Request{Name: name, When: argString(args, "when"),
			Instruction: argString(args, "instruction"), Task: argString(args, "task"), Allow: allow}, "agent")
	case "list":
		return Result{Content: strings.Join(t.s.ScheduleLines(), "\n")}
	case "resume":
		if untrusted {
			return Result{IsError: true, Content: "a web page was read during this request; a person can resume it with /schedule resume"}
		}
		msg, err = t.s.ScheduleAction(action, name, "agent")
	case "pause", "cancel":
		msg, err = t.s.ScheduleAction(action, name, "agent")
	default:
		return Result{IsError: true, Content: "action must be one of add, list, pause, resume, cancel"}
	}
	if err != nil {
		return Result{IsError: true, Content: err.Error()}
	}
	return Result{Content: msg}
}
