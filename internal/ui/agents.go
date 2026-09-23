package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/agent"
)

// IsTaskVerb reports whether TaskVerb handles these arguments, without
// running them. A UI that must run TaskVerb off its own goroutine — the
// TUI, whose Update holds the session lock that TaskVerb's callees may
// block on — has to know that before it runs anything.
func IsTaskVerb(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "assign", "scope", "reply":
		return true
	}
	return false
}

// TaskVerb handles the sub-agent verbs of /task — assign, scope, reply —
// for both UIs. handled is false for every other /task form.
//
// **It may block.** `assign` on a sub-agent parked on an ask stops that run
// and waits for its goroutine to finish; `scope` and `assign` both schedule,
// which can raise a notice. A caller holding a lock the agent's own
// callbacks take must run this somewhere else (see the TUI's taskVerbCmd).
func TaskVerb(ag *agent.Agent, args []string) (lines []string, handled bool) {
	if len(args) == 0 {
		return nil, false
	}
	switch args[0] {
	case "assign":
		if len(args) != 3 {
			return []string{"usage: /task assign <id> <owner|main>"}, true
		}
		id, owner := args[1], args[2]
		if owner == "main" {
			owner = ""
		}
		if err := ag.AssignOwner(id, owner, true); err != nil {
			return []string{err.Error()}, true
		}
		if owner == "" {
			return []string{id + " is the main model's again"}, true
		}
		return []string{id + " assigned to " + owner + " (pinned); set a scope with /task scope"}, true
	case "scope":
		if len(args) < 3 {
			return []string{"usage: /task scope <id> <path>[, <path>…]"}, true
		}
		paths := splitPaths(strings.Join(args[2:], " "))
		if err := ag.SetScope(args[1], paths); err != nil {
			return []string{err.Error()}, true
		}
		return []string{args[1] + " scope: " + strings.Join(paths, ", ")}, true
	case "reply":
		if len(args) < 3 {
			return []string{"usage: /task reply <id> <text>"}, true
		}
		if err := ag.ReplyAsk(args[1], strings.Join(args[2:], " ")); err != nil {
			return []string{err.Error()}, true
		}
		return []string{"reply delivered to the sub-agent on " + args[1]}, true
	}
	return nil, false
}

func splitPaths(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// IsAgentsBlocking reports whether these /agents arguments dispatch
// sub-agent work and so must run off the Update goroutine, the same reason
// IsTaskVerb exists: "start" clears the startup decline flag and calls
// ScheduleSubAgents, whose dispatch fence can raise a notice under the
// session lock (see taskVerbCmd's comment on /task scope, which schedules
// the same way).
func IsAgentsBlocking(args []string) bool {
	return len(args) == 1 && args[0] == "start"
}

// AgentLines is /agents: the model cards and what each is doing, /agents
// stop <name>, or /agents start.
func AgentLines(ag *agent.Agent, args []string) []string {
	if !ag.SubAgentsEnabled() {
		return []string{`no sub-agents configured (set "sub_agent": true on a coworkers entry; see README "Sub-agents")`}
	}
	if len(args) == 2 && args[0] == "stop" {
		if err := ag.StopSubAgent(args[1]); err != nil {
			return []string{err.Error()}
		}
		return []string{"stopping " + args[1]}
	}
	if len(args) == 1 && args[0] == "start" {
		started := ag.AllowSubAgentStart()
		if len(started) == 0 {
			return []string{"no sub-agent work is waiting"}
		}
		plural := ""
		if len(started) != 1 {
			plural = "s"
		}
		return []string{fmt.Sprintf("started %d sub-agent step%s: %s", len(started), plural, strings.Join(started, ", "))}
	}
	if len(args) > 0 {
		return []string{"usage: /agents [stop <name>|start]"}
	}
	var lines []string
	for _, s := range ag.SubAgentStates() {
		if s.Model == "" { // a waiting row
			lines = append(lines, fmt.Sprintf("  %s  %s  %s", s.Name, s.Node, s.State))
			continue
		}
		where := "local"
		if s.Online {
			where = "online"
		}
		line := fmt.Sprintf("  %s  %s/%s  %s  sub-agent", s.Name, s.Provider, s.Model, where)
		if len(s.MaxScope) > 0 {
			line += "  max_scope: " + strings.Join(s.MaxScope, ", ")
		}
		if s.Node != "" {
			line += fmt.Sprintf("  %s  %s (%d tool calls, %s)", s.Node, s.State, s.Calls, time.Since(s.Since).Round(time.Minute))
		} else {
			line += "  " + s.State
		}
		lines = append(lines, line)
	}
	return lines
}
