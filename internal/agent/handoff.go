package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/store"
)

// handoffSystemPrompt asks for the briefing a fresh session needs to carry
// on without loss of fidelity: what was asked, what the user insisted on,
// what was decided, what changed, what is left.
const handoffSystemPrompt = `Write a handoff briefing for the next coding session that continues this work. Include, with these headings:
Task: the original request in one or two sentences.
Requirements: every requirement, constraint, preference or convention the user stated, verbatim where possible.
Decisions: key technical decisions and why.
Changes: files created or modified and what changed in each.
State: what works now and what is verified.
Next: outstanding work, known issues, and the next concrete step.
Under 400 words. Plain text, no markdown fences.`

// WriteHandoff produces the session briefing, stores it on the session, and
// returns it. With withModel the model writes it (falling back to a
// heuristic transcript summary if it fails); without, only the heuristic
// runs, so headless scripts never wait on a second model call.
func (a *Agent) WriteHandoff(ctx context.Context, withModel bool) (string, error) {
	if a.Session == nil {
		return "", fmt.Errorf("no session")
	}
	if len(a.History.Messages) == 0 {
		return "", nil
	}
	h, err := "", error(nil)
	if withModel {
		h, err = a.modelHandoff(ctx)
	}
	if err != nil || strings.TrimSpace(h) == "" {
		if err != nil {
			a.notice("handoff via model failed (%v); using transcript summary", err)
		}
		h = a.heuristicHandoff()
	}
	if a.engine() != nil {
		// Drop the previous exit's line first: a resumed briefing carries
		// one, and appending would stack a copy per exit.
		h = stripStoppedAt(h)
		at := ""
		a.engineDo("stopped at", func(st *engine.Store) { at = st.StoppedAt() })
		if at != "" {
			h += "\n\nStopped at: " + at
		}
	}
	a.Session.Handoff = strings.TrimSpace(h)
	return a.Session.Handoff, nil
}

const stoppedAtPrefix = "Stopped at: "

// stripStoppedAt removes a trailing "Stopped at: …" line from a briefing.
func stripStoppedAt(h string) string {
	h = strings.TrimRight(h, " \t\n")
	if strings.HasPrefix(h, stoppedAtPrefix) && !strings.Contains(h, "\n") {
		return ""
	}
	i := strings.LastIndex(h, "\n"+stoppedAtPrefix)
	if i < 0 || strings.Contains(h[i+1:], "\n") {
		return h
	}
	return strings.TrimRight(h[:i], " \t\n")
}

// priorBriefing returns the innermost real briefing carried by a handoff,
// unwrapping a heuristic "Previous briefing:" wrapper so repeated exits
// never nest briefings inside each other.
func priorBriefing(h string) string {
	const hdr = "Previous briefing:\n"
	for strings.HasPrefix(h, hdr) {
		h = h[len(hdr):]
		if i := strings.Index(h, "\n\nTask: "); i >= 0 {
			h = h[:i]
		}
	}
	return h
}

func (a *Agent) modelHandoff(ctx context.Context) (string, error) {
	// A 27B thinking model can spend minutes on this; the caller offers
	// Ctrl-C to skip, and the heuristic fallback covers a timeout.
	ctx, cancel := context.WithTimeout(ctx, 6*time.Minute)
	defer cancel()
	const capBytes = 16 * 1024
	var b strings.Builder
	if prior := priorBriefing(a.Handoff()); prior != "" {
		fmt.Fprintf(&b, "Briefing from the session before this one:\n%s\n\n", prior)
	}
	b.WriteString("Transcript (most recent last):\n")
	var t strings.Builder
	for _, m := range a.History.Messages {
		if isToolResult(m) {
			fmt.Fprintf(&t, "[tool result] %.300s\n", m.Content)
			continue
		}
		fmt.Fprintf(&t, "[%s] %.1200s\n", m.Role, m.Content)
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&t, "  (called %s %.300s)\n", tc.Name, tc.Arguments)
		}
	}
	tr := t.String()
	if len(tr) > capBytes {
		cut := tr[len(tr)-capBytes:]
		if nl := strings.IndexByte(cut, '\n'); nl >= 0 {
			cut = cut[nl+1:]
		}
		tr = "[earlier transcript omitted]\n" + cut
	}
	b.WriteString(tr)
	a.awaitWindow(ctx) // never send with no window on the wire
	resp, err := a.Provider.Chat(ctx, provider.ChatRequest{
		Model: a.Model,
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: handoffSystemPrompt},
			{Role: provider.RoleUser, Content: b.String()},
		},
		Temperature: 0.1,
		NoThink:     true, // seconds instead of minutes on thinking models
	}, nil)
	if err != nil {
		return "", err
	}
	out := resp.Content
	if a.Profile.StripThink {
		out = StripThink(out)
	}
	return out, nil
}

// heuristicHandoff is the no-model fallback: task, files touched, last reply.
func (a *Agent) heuristicHandoff() string {
	var b strings.Builder
	if prior := priorBriefing(a.Handoff()); prior != "" {
		fmt.Fprintf(&b, "Previous briefing:\n%s\n\n", prior)
	}
	task, last := "", ""
	seen := map[string]bool{}
	var files []string
	for _, m := range a.History.Messages {
		m.Content = StripHarnessState(m.Content)
		switch {
		case m.Role == provider.RoleUser && !isToolResult(m) && !strings.HasPrefix(m.Content, summaryPrefix):
			if task == "" {
				task = m.Content
			}
		case m.Role == provider.RoleAssistant && strings.TrimSpace(m.Content) != "":
			last = m.Content
		}
		for _, tc := range m.ToolCalls {
			if tc.Name != "write_file" && tc.Name != "edit_file" {
				continue
			}
			var args struct {
				Path string `json:"path"`
			}
			if json.Unmarshal([]byte(tc.Arguments), &args) == nil && args.Path != "" && !seen[args.Path] {
				seen[args.Path] = true
				files = append(files, args.Path)
			}
		}
	}
	if len(task) > 1500 {
		task = task[:1500] + "..."
	}
	if len(last) > 1500 {
		last = last[:1500] + "..."
	}
	fmt.Fprintf(&b, "Task: %s\n", task)
	if len(files) > 0 {
		fmt.Fprintf(&b, "Changes: %s\n", strings.Join(files, ", "))
	}
	fmt.Fprintf(&b, "Last assistant reply: %s\n", last)
	return b.String()
}

// Resume loads a saved conversation into this agent's history, carrying the
// previous session's handoff into the system prompt.
//
// It takes the turn lock, and it must: replacing the transcript is the
// largest rewrite there is, and it is no longer the only one that can be in
// flight. A post-switch compaction parked in its model call is holding the
// old conversation's messages; without this lock it would finish *after*
// the resume and write that conversation's summary over the transcript the
// user just loaded — which the next autosave would then commit to the
// resumed session's own file. Cross-session corruption, from two commands
// that look unrelated.
//
// Like ClearHistory and CompactNow it must therefore never be called from
// inside a Bubble Tea Update; see the note on those.
func (a *Agent) Resume(s *store.Session) {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	a.SetSession(s)
	a.History.Messages = append([]provider.Message(nil), s.Messages...)
	a.sessionMu.Lock()
	a.handoff = s.Handoff
	a.sessionMu.Unlock()
	// modelMu for the system prompt, in the order everything else takes
	// them: turn lock first, then this one.
	a.modelMu.Lock()
	a.History.System.Content = a.composeSystem("")
	a.modelMu.Unlock()
}

// Handoff returns the briefing carried over from the resumed session.
//
// Under sessionMu, with every other read of the field: /handoff is
// busy-safe, so a UI asks for it whenever it likes, while /resume writes it
// from a goroutine of its own.
func (a *Agent) Handoff() string {
	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()
	return a.handoff
}

// ApplyWindow clamps the history budget to the backend's real context
// window and reserves generation headroom. Returns true when the configured
// budget was larger than the window and had to be reduced.
func (a *Agent) ApplyWindow(window int) bool {
	if window <= 0 {
		return false
	}
	a.window.Store(int64(window))
	// The budget follows the window both ways, never above the configured
	// context_tokens: a shrunken window clamps it, a restored one gives it
	// back.
	target := window
	if a.Cfg.ContextTokens > 0 && target > a.Cfg.ContextTokens {
		target = a.Cfg.ContextTokens
	}
	// Both scalars in one critical section, and the reserve computed before
	// it (reserveFor takes modelMu, and the order is always modelMu then
	// this one). Two separate sections would leave a window in which the
	// new budget stands beside the old reserve — or, going the other way, a
	// reserve larger than the budget, which Limit would report as its 512
	// floor to whatever UI or notice happened to read it just then.
	reserve := a.reserveFor(window)
	a.History.mu.Lock()
	clamped := a.History.Budget > window
	a.History.Budget = target
	a.History.Reserve = reserve
	limit := a.History.Budget - a.History.Reserve
	a.History.mu.Unlock()
	a.capToolOutput(limit)
	return clamped
}

// ApplyResolvedWindow is ApplyWindow for a window the loader has just
// resolved — at startup, after a model switch, after a consent answer. The
// server does not hold it yet: a model is reloaded by the first request that
// carries the new num_ctx, so until one succeeds checkBackend must not read
// the old window as another client's change and adapt back down to it.
func (a *Agent) ApplyResolvedWindow(window int) bool {
	if window > 0 {
		a.windowUnconfirmed.Store(true)
	}
	return a.ApplyWindow(window)
}

// reserveFor is the generation headroom kept free below the window.
// Reasoning models spend a large, unpredictable share of the window
// thinking before the first answer token, so they get a third of it;
// plain models a quarter. An explicit max_tokens wins.
//
// It takes modelMu because the profile it reads is what a model switch
// rewrites, and the window it is sizing for may be arriving on
// resolveModel's goroutine while the switch itself ran on a UI's.
func (a *Agent) reserveFor(window int) int {
	if a.Cfg.MaxTokens > 0 {
		return a.Cfg.MaxTokens
	}
	a.modelMu.Lock()
	thinking := a.Profile.StripThink
	a.modelMu.Unlock()
	if thinking {
		r := window / 3
		if r < 4096 {
			r = 4096
		}
		if r > 16384 {
			r = 16384
		}
		return r
	}
	r := window / 4
	if r < 1024 {
		r = 1024
	}
	if r > 4096 {
		r = 4096
	}
	return r
}

// applyReserve sets the reserve for window and scales per-call tool output
// to the usable limit: a 24KB result is ~7k tokens, which on a small window
// forces compaction every couple of calls. Cap at three-quarters of the
// limit in bytes, between 4KB and the 24KB default.
func (a *Agent) applyReserve(window int) {
	reserve := a.reserveFor(window)
	a.History.mu.Lock()
	a.History.Reserve = reserve
	limit := a.History.Budget - reserve
	a.History.mu.Unlock()
	a.capToolOutput(limit)
}

// capToolOutput scales the per-call tool-output cap to the usable limit. It
// takes the limit as a number rather than calling History.Limit, so its
// caller can read it in the same critical section that wrote the scalars it
// is derived from.
func (a *Agent) capToolOutput(limit int) {
	if limit < 512 {
		limit = 512 // History.Limit's floor: never trim into nothing
	}
	if a.Tools == nil {
		return
	}
	capBytes := limit * 3 / 4
	if capBytes > 24*1024 {
		capBytes = 24 * 1024
	}
	if capBytes < 4*1024 {
		capBytes = 4 * 1024
	}
	a.Tools.SetMaxOutput(capBytes)
}
