package agent

import (
	"fmt"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/engine"
)

// Time awareness (config time_awareness, on by default). A local model has no
// clock: it cannot tell a step that took forty seconds from one that took
// forty minutes, a test suite creeping toward its timeout from a quick one,
// or a user who answered at once from one who came back the next morning.
//
// The time goes where text is appended anyway — the end of a tool result, the
// end of a user message — and never into the system prompt. The system prompt
// is the front of every request, so a line there that changes each turn makes
// the server re-process the whole conversation behind it; a footer on the
// newest message costs its own fifteen tokens and nothing else.

// timeNow is the agent's clock, replaceable in tests.
var timeNow = time.Now

// timeFooter is the last line of a tool result as the model sees it:
//
//	[14:32:07 · took 3.2s · step 3.2 open 14m · context 61%]
func (a *Agent) timeFooter(took time.Duration) string {
	if a.Cfg == nil || !a.Cfg.TimeAwareness {
		return ""
	}
	now := timeNow()
	parts := []string{now.Format("15:04:05"), "took " + tookText(took)}
	a.engineDo("clock", func(st *engine.Store) {
		if id, started, ok := st.DoingClock(); ok {
			parts = append(parts, fmt.Sprintf("step %s open %s", id, engine.ShortDuration(now.Sub(started))))
		}
	})
	if a.History != nil {
		if limit := a.History.Limit(); limit > 0 {
			parts = append(parts, fmt.Sprintf("context %d%%", a.History.Tokens()*100/limit))
		}
	}
	return "[" + strings.Join(parts, " · ") + "]"
}

func tookText(d time.Duration) string {
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	default:
		return engine.ShortDuration(d)
	}
}

// stampUser marks a user message with when it arrived. A suffix, so whatever
// reads the head of the message — a session title, the original task in a
// compaction — still reads the user's own words.
func (a *Agent) stampUser(text string) string {
	if a.Cfg == nil || !a.Cfg.TimeAwareness {
		return text
	}
	return text + "\n\n[sent " + timeNow().Format("Mon 2 Jan 2006 15:04") + "]"
}

// PromptCostLine is the second line of /stats: what the server spent reading
// prompts and loading the model, and how often its prompt cache missed. Empty
// for a backend that does not report it.
func PromptCostLine(s Stats) string {
	if s.PromptTime == 0 && s.LoadTime == 0 {
		return ""
	}
	return fmt.Sprintf("server: prompt_processing=%s model_loading=%s uncached_prompt_reads=%d of %d",
		s.PromptTime.Round(100*time.Millisecond), s.LoadTime.Round(100*time.Millisecond), s.SlowReads, s.Requests)
}
