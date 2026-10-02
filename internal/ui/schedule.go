package ui

import (
	"errors"
	"strings"

	"github.com/brown-enterprises/be-code/internal/agent"
	"github.com/brown-enterprises/be-code/internal/schedule"
)

const scheduleUsage = "usage: /schedule add <name> <when> -- <instruction> [allow <grant>; <grant>…]"

// ParseScheduleAdd reads "<name> <when> -- <instruction> [allow g; g]". The
// allow clause is the last " allow " followed by a grant kind, so an
// instruction may itself use the word "allow".
func ParseScheduleAdd(rest string) (schedule.Request, error) {
	head, instr, ok := strings.Cut(rest, " -- ")
	f := strings.Fields(head)
	if !ok || len(f) < 2 || strings.TrimSpace(instr) == "" {
		return schedule.Request{}, errors.New(scheduleUsage)
	}
	r := schedule.Request{Name: f[0], When: strings.Join(f[1:], " ")}
	for i := strings.LastIndex(instr, " allow "); i >= 0; i = strings.LastIndex(instr[:i], " allow ") {
		tail := strings.TrimSpace(instr[i+len(" allow "):])
		if strings.HasPrefix(tail, "shell:") || strings.HasPrefix(tail, "write:") || strings.HasPrefix(tail, "browser:") {
			for _, g := range strings.Split(tail, ";") {
				if g = strings.TrimSpace(g); g != "" {
					r.Allow = append(r.Allow, g)
				}
			}
			instr = instr[:i]
			break
		}
	}
	r.Instruction = strings.TrimSpace(instr)
	return r, nil
}

// IsScheduleBlocking reports subcommands that can raise a prompt, which a
// TUI must run off its Update goroutine.
func IsScheduleBlocking(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "add", "resume", "pause", "cancel", "run":
		return true
	}
	return false
}

// ScheduleCommandLines runs /schedule and returns what to print. args is
// everything after "/schedule", unsplit (the instruction keeps its spacing).
func ScheduleCommandLines(ag *agent.Agent, args string) []string {
	args = strings.TrimSpace(args)
	sub, rest, _ := strings.Cut(args, " ")
	rest = strings.TrimSpace(rest)
	switch sub {
	case "":
		return ag.ScheduleLines()
	case "add":
		req, err := ParseScheduleAdd(rest)
		if err != nil {
			return []string{err.Error()}
		}
		msg, err := ag.AddSchedule(req, "person")
		if err != nil {
			return []string{err.Error()}
		}
		return []string{msg}
	case "show":
		lines, err := ag.ScheduleShow(rest)
		if err != nil {
			return []string{err.Error()}
		}
		return lines
	case "pause", "resume", "cancel", "run":
		msg, err := ag.ScheduleAction(sub, rest, "person")
		if err != nil {
			return []string{err.Error()}
		}
		return []string{msg}
	}
	return []string{scheduleUsage, "       /schedule [show <name>|pause|resume|cancel|run <name>]"}
}
