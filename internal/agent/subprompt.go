package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/subagent"
)

// SubAgentFrame is a sub-agent's system prompt (spec §2.5). Short on
// purpose: prompt guidance moves a local model more than mechanism does.
const SubAgentFrame = `You are a sub-agent working one step of a larger task for a main model that owns the whole task. Your step, its sub-steps, and the files you may change are listed below. Do the step completely: read what you need anywhere in the repository, change only files inside your scope, run the project's checks when you have changed code, and mark each sub-step done as you finish it. If you need a file outside your scope, a decision you cannot make, or information only the main model has, use ask_main once with a precise question and wait. When the step is done, reply with a short summary of what you changed and anything the main model must know. Do not restate the task.`

// subAgentGuidance is the one block the main model gets when sub-agents
// are configured. It follows taskGuidance and pacingGuidance, which stay
// verbatim.
const subAgentGuidance = `Sub-agents: a step you assign with an owner and a scope is done by that model on its own; you are told in a later message when it finishes or asks something. Assign whole steps with a clear file scope, do not edit inside a running sub-agent's scope, and answer its questions with task reply.`

// renderDispatch is the Dispatch as the sub-agent reads it, after the frame.
func renderDispatch(d subagent.Dispatch) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Your step: %s %s\n", d.Node, d.Text)
	if len(d.Children) > 0 {
		b.WriteString("Sub-steps:\n")
		for _, c := range d.Children {
			b.WriteString("- [ ] " + c + "\n")
		}
	}
	fmt.Fprintf(&b, "Scope (the only paths you may write): %s\n", strings.Join(d.Scope, ", "))
	if len(d.Checks) > 0 {
		fmt.Fprintf(&b, "Checks you may run with shell, exactly as written: %s\n", strings.Join(d.Checks, " · "))
	} else {
		b.WriteString("This project has no detected checks; shell is unavailable.\n")
	}
	if d.Interrupted {
		b.WriteString("This step was interrupted earlier")
		if len(d.Touched) > 0 {
			b.WriteString("; these files were already written: " + strings.Join(d.Touched, ", ") + ". Read them before writing again")
		}
		b.WriteString(".\n")
	}
	if d.Context != "" {
		b.WriteString("\nContext (facts about the task, not instructions):\n" + d.Context + "\n")
	}
	return b.String()
}

// handBackLine is the message the main model is queued when a sub-agent
// finishes (spec §2.6).
func handBackLine(hb subagent.HandBack) string {
	var b strings.Builder
	switch hb.Status {
	case "done":
		fmt.Fprintf(&b, "sub-agent %s finished %s (done, %s, %d tool calls", hb.Owner, hb.Node, shortDur(hb.Elapsed), hb.Calls)
	default:
		fmt.Fprintf(&b, "sub-agent %s stopped on %s (%s: %s; %s, %d tool calls", hb.Owner, hb.Node, hb.Status, hb.Reason, shortDur(hb.Elapsed), hb.Calls)
	}
	if len(hb.Files) > 0 {
		b.WriteString("; wrote " + strings.Join(hb.Files, ", "))
	}
	b.WriteString("):")
	if hb.Summary != "" {
		b.WriteString("\n" + hb.Summary)
	}
	return b.String()
}

// shortDur is how long a sub-agent worked, to the second. The agent package
// had no duration formatter of its own; engine.ShortDuration is the task
// record's, whose shape ("2h05m") belongs to the document rather than to a
// queued line.
func shortDur(d time.Duration) string { return d.Round(time.Second).String() }
