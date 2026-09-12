package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
	a.Session.Handoff = strings.TrimSpace(h)
	return a.Session.Handoff, nil
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
	if prior := priorBriefing(a.handoff); prior != "" {
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
	if prior := priorBriefing(a.handoff); prior != "" {
		fmt.Fprintf(&b, "Previous briefing:\n%s\n\n", prior)
	}
	task, last := "", ""
	seen := map[string]bool{}
	var files []string
	for _, m := range a.History.Messages {
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
func (a *Agent) Resume(s *store.Session) {
	a.SetSession(s)
	a.History.Messages = append([]provider.Message(nil), s.Messages...)
	a.handoff = s.Handoff
	a.History.System.Content = a.composeSystem("")
}

// Handoff returns the briefing carried over from the resumed session.
func (a *Agent) Handoff() string { return a.handoff }

// ApplyWindow clamps the history budget to the backend's real context
// window and reserves generation headroom. Returns true when the configured
// budget was larger than the window and had to be reduced.
func (a *Agent) ApplyWindow(window int) bool {
	if window <= 0 {
		return false
	}
	a.Window = window
	// The budget follows the window both ways, never above the configured
	// context_tokens: a shrunken window clamps it, a restored one gives it
	// back.
	target := window
	if a.Cfg.ContextTokens > 0 && target > a.Cfg.ContextTokens {
		target = a.Cfg.ContextTokens
	}
	clamped := a.History.Budget > window
	a.History.Budget = target
	a.applyReserve(window)
	return clamped
}

// reserveFor is the generation headroom kept free below the window.
// Reasoning models spend a large, unpredictable share of the window
// thinking before the first answer token, so they get a third of it;
// plain models a quarter. An explicit max_tokens wins.
func (a *Agent) reserveFor(window int) int {
	if a.Cfg.MaxTokens > 0 {
		return a.Cfg.MaxTokens
	}
	if a.Profile.StripThink {
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
	a.History.Reserve = a.reserveFor(window)
	if a.Tools != nil {
		capBytes := a.History.Limit() * 3 / 4
		if capBytes > 24*1024 {
			capBytes = 24 * 1024
		}
		if capBytes < 4*1024 {
			capBytes = 4 * 1024
		}
		a.Tools.MaxOutput = capBytes
	}
}
