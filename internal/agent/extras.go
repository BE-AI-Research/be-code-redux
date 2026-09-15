package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/brown-enterprises/be-code/internal/gitctx"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// ---- plan mode -------------------------------------------------------------

// Plan runs the read-only planning phase and returns the plan text. The UI
// shows it for approval, then calls ExecutePlan.
func (a *Agent) Plan(ctx context.Context, input string) (string, error) {
	scratch := a.planAgent()
	plan, err := scratch.Run(ctx, input)
	a.addStats(scratch.usageTokens())
	return plan, err
}

// planAgent builds the read-only scratch agent for plan mode. Its system
// prompt is pinned via systemOverride so the per-turn git refresh in run()
// cannot swap it for the normal coding prompt.
func (a *Agent) planAgent() *Agent {
	readOnly := a.Tools.Subset("read_file", "list_dir", "search", "web_search", "web_fetch", "consult")
	scratch := &Agent{
		Cfg: a.Cfg, Provider: a.Provider, Model: a.Model, Tools: readOnly,
		Profile: a.Profile, compat: a.compat, projectNotes: a.projectNotes,
		repoMap: a.repoMap, Events: a.Events, Window: a.Window,
	}
	scratch.knownTools = map[string]bool{}
	for _, n := range readOnly.Names() {
		scratch.knownTools[n] = true
	}
	sys := planSystemPrompt
	if scratch.compat || a.Cfg.CompatToolCalls == "auto" {
		full := BuildSystemPrompt(readOnly.Specs(), true, "") // tool format guidance
		sys = planSystemPrompt + "\n\n" + full[strings.Index(full, "Tool calling format"):]
	}
	if a.repoMap != "" {
		sys += "\n\nRepository map:\n" + a.repoMap
	}
	scratch.systemOverride = sys
	scratch.History = NewHistory(sys, a.History.Budget)
	scratch.History.Reserve = a.History.Reserve
	scratch.History.CharsPerToken = a.History.CharsPerToken
	return scratch
}

// ExecutePlan runs the approved plan through the normal loop.
func (a *Agent) ExecutePlan(ctx context.Context, request, plan string) (string, *ReviewedReport, error) {
	return a.RunFull(ctx, fmt.Sprintf(planExecutePrefix, request, plan))
}

// ---- git commit ------------------------------------------------------------

// GenerateCommit writes a commit message with the model and commits all
// changes. Returns the one-line commit log entry.
func (a *Agent) GenerateCommit(ctx context.Context) (string, error) {
	diff := gitctx.DiffStat(ctx, a.Tools.Root)
	if strings.TrimSpace(diff) == "" {
		return "", fmt.Errorf("no changes to commit (or not a git repository)")
	}
	resp, err := a.Provider.Chat(ctx, provider.ChatRequest{
		Model: a.Model,
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: "Write a single-line git commit message (max 72 chars, imperative mood, conventional-commits style when it fits) for this diff. Output ONLY the message."},
			{Role: provider.RoleUser, Content: diff},
		},
		Temperature: 0.1,
	}, nil)
	if err != nil {
		return "", err
	}
	msg := strings.TrimSpace(StripThink(resp.Content))
	msg = strings.Trim(strings.SplitN(msg, "\n", 2)[0], "`\" ")
	if msg == "" {
		msg = "be-code: automated change"
	}
	return gitctx.Commit(ctx, a.Tools.Root, msg)
}

// ---- undo ------------------------------------------------------------------

// Undo rolls back the last turn's file changes and tells the model.
func (a *Agent) Undo() ([]string, error) {
	if a.Checkpoints == nil {
		return nil, fmt.Errorf("checkpoints not enabled")
	}
	restored, err := a.Checkpoints.Undo()
	if len(restored) > 0 {
		a.History.Add(provider.Message{Role: provider.RoleUser,
			Content: "[The user rolled back your last changes to: " + strings.Join(restored, ", ") + ". The files are back to their prior state.]"})
	}
	return restored, err
}

// ---- reviewer routing ------------------------------------------------------

// Review asks the configured reviewer model to critique this session's
// changes. Returns "" when the reviewer approves.
func (a *Agent) Review(ctx context.Context, reviewer provider.Provider, reviewerModel string) (string, error) {
	changed := []string{}
	if a.Checkpoints != nil {
		changed = a.Checkpoints.ChangedLast()
	}
	if len(changed) == 0 {
		return "", nil
	}
	var b strings.Builder
	for _, rel := range changed {
		data, err := os.ReadFile(filepath.Join(a.Tools.Root, rel))
		if err != nil {
			continue
		}
		if len(data) > 12*1024 {
			data = data[:12*1024]
		}
		fmt.Fprintf(&b, "=== %s ===\n%s\n", rel, string(data))
	}
	if b.Len() == 0 {
		return "", nil
	}
	resp, err := reviewer.Chat(ctx, provider.ChatRequest{
		Model: reviewerModel,
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: "You are a strict senior code reviewer. Review the changed files below for bugs, security issues, and broken edge cases. If the code is acceptable, reply with exactly APPROVED. Otherwise list the concrete problems (max 5, most severe first) with file names."},
			{Role: provider.RoleUser, Content: b.String()},
		},
		Temperature: 0.1,
	}, nil)
	if err != nil {
		return "", err
	}
	verdict := strings.TrimSpace(StripThink(resp.Content))
	if strings.HasPrefix(strings.ToUpper(verdict), "APPROVED") {
		return "", nil
	}
	return verdict, nil
}
