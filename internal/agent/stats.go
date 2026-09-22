package agent

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/engine"
)

// StatsReport is the /stats screen: what this session has cost and where it
// stands, as a block of aligned lines both UIs print as they are. clients
// are the attached terminals' labels (none for an unhosted session).
func (a *Agent) StatsReport(clients []string) string {
	s := a.Usage()
	var b strings.Builder
	row := func(k string, format string, args ...any) {
		fmt.Fprintf(&b, "  %-22s %s\n", k, fmt.Sprintf(format, args...))
	}
	head := func(t string) { fmt.Fprintf(&b, "%s\n", t) }

	head("Session")
	row("model", "%s", a.Model)
	row("running for", "%s", engine.ShortDuration(time.Since(a.started)))
	if len(clients) > 0 {
		row("terminals", "%d (%s)", len(clients), strings.Join(clients, ", "))
	}
	if st := a.engine(); st != nil {
		open, done, blocked := 0, 0, 0
		var doing string
		t := st.Tree()
		t.Walk(func(n *engine.Node, _ int) {
			switch n.Status {
			case engine.StatusDone:
				done++
			case engine.StatusBlocked, engine.StatusDropped:
				blocked++
			case engine.StatusDoing:
				doing = n.ID + " " + n.Text
				open++
			default:
				open++
			}
		})
		row("tasks", "%d done · %d open · %d blocked or dropped", done, open, blocked)
		if doing != "" {
			if len(doing) > 60 {
				doing = doing[:57] + "…"
			}
			row("current step", "%s", doing)
		}
	}

	head("Context")
	tokens, limit, floor := a.History.Tokens(), a.History.Limit(), a.History.Floor()
	budget, reserve, _ := a.History.Scalars()
	pct := 0
	if limit > 0 {
		pct = tokens * 100 / limit
	}
	row("in use", "%d of %d usable tokens (%d%%)", tokens, limit, pct)
	row("fixed prompt", "%d tokens (system prompt, tools, notes, map; never compacted)", floor)
	if w := a.Window(); w > 0 {
		row("window", "%d tokens on the server · budget %d · reply reserve %d", w, budget, reserve)
	} else {
		row("budget", "%d · reply reserve %d", budget, reserve)
	}
	row("compactions", "%d", s.Compactions)

	head("Model")
	row("requests", "%d", s.Requests)
	row("prompt tokens", "%d", s.PromptTokens)
	row("completion tokens", "%d (plus %dk chars of hidden reasoning)", s.CompletionTokens, s.ReasoningChars/1000)
	if s.Requests > 0 {
		row("per request", "%d prompt · %d completion", s.PromptTokens/s.Requests, s.CompletionTokens/s.Requests)
	}
	row("time in requests", "%s", engine.ShortDuration(s.Elapsed))
	if s.PromptTime > 0 || s.LoadTime > 0 {
		row("server prompt reading", "%s · model loading %s", s.PromptTime.Round(100*time.Millisecond), s.LoadTime.Round(100*time.Millisecond))
		row("prompt cache misses", "%d of %d requests", s.SlowReads, s.Requests)
	}

	head("Tools")
	row("calls", "%d · %d verification repair rounds", s.ToolCalls, s.Repairs)
	if len(s.ToolsByName) > 0 {
		type kv struct {
			k string
			v int
		}
		var kvs []kv
		for k, v := range s.ToolsByName {
			kvs = append(kvs, kv{k, v})
		}
		sort.Slice(kvs, func(i, j int) bool {
			if kvs[i].v != kvs[j].v {
				return kvs[i].v > kvs[j].v
			}
			return kvs[i].k < kvs[j].k
		})
		var parts []string
		for i, e := range kvs {
			if i == 8 {
				parts = append(parts, fmt.Sprintf("+%d more", len(kvs)-i))
				break
			}
			parts = append(parts, fmt.Sprintf("%s %d", e.k, e.v))
		}
		row("by tool", "%s", strings.Join(parts, " · "))
	}
	return strings.TrimRight(b.String(), "\n")
}
