// Package agent implements the BE-Code agentic loop: model turns, tool
// dispatch, context budgeting, and the verification/repair cycle.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/checkpoint"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/gitctx"
	"github.com/brown-enterprises/be-code/internal/profiles"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/repomap"
	"github.com/brown-enterprises/be-code/internal/store"
	"github.com/brown-enterprises/be-code/internal/tools"
	"github.com/brown-enterprises/be-code/internal/verify"
)

// Events lets the UI observe the loop. Any callback may be nil.
type Events struct {
	OnDelta     func(text string)                   // streaming assistant text
	OnToolStart func(name, args string)             // before a tool runs
	OnToolEnd   func(name string, res tools.Result) // after a tool runs
	OnNotice    func(msg string)                    // loop-level notices
	// OnTransient receives short-lived status notices (waiting for the
	// backend, context budgeting) that a UI may show briefly instead of
	// keeping in the transcript. When nil they arrive through OnNotice.
	OnTransient func(msg string)
	OnReasoning func(text string) // hidden model reasoning deltas (thinking models)
}

// Stats accumulates per-session usage for /stats and the status bar.
type Stats struct {
	PromptTokens     int
	CompletionTokens int
	Requests         int // model round-trips
	ToolCalls        int
	Elapsed          time.Duration
}

// Agent binds a provider, tool registry, and conversation history.
type Agent struct {
	Cfg      *config.Config
	Provider provider.Provider
	Model    string
	Tools    *tools.Registry
	History  *History
	Events   Events
	// Session, when set, is auto-saved after every completed request.
	Session *store.Session
	// Checkpoints, when set, snapshots files before each turn's edits.
	Checkpoints *checkpoint.Checkpointer
	// Profile is the active model-family tuning.
	Profile profiles.Profile
	// Stats is cumulative session usage.
	Stats Stats
	// ContextProvider, when set, returns a short note about what the user
	// is looking at in their editor; it is prepended to each new request.
	ContextProvider func(ctx context.Context) string
	// Guidance is extra system-prompt text (editor tools, etc.). Exported
	// so later wiring can read it, but set it via SetGuidance so the
	// composed system prompt is refreshed immediately.
	Guidance string
	// IDEName is the connected editor's name (e.g. "vscode"), or "" when
	// no editor is connected.
	IDEName string
	// IDETools is how many editor tools were attached (0 when none), so a
	// UI can report the connection once it owns the screen.
	IDETools int

	projectNotes   string
	handoff        string // briefing from the resumed session, kept in the system prompt
	Window         int    // backend context window when detected (0 = unknown)
	systemOverride string // plan mode: replaces the base coding prompt
	reqTouched     bool   // a tool that can change files ran during this request
	repoDirty      bool   // files were written; rebuild the repo map before the next request

	inbox Inbox // mid-task user messages (see inbox.go)

	// Backend resilience (see resilience.go).
	retryBase        time.Duration // first retry delay; doubles per attempt
	stallAfter       time.Duration // silence before a "waiting for backend" notice
	unloadedNotified bool          // one notice per eviction, not per turn
	repoMap          string
	// saveDisabled latches on when another live process is found to own the
	// session file; saveOwner is its pid and saveWarned keeps the warning to
	// one line per run (see SaveGuard, autosave).
	saveDisabled bool
	saveOwner    int
	saveWarned   bool
	knownTools   map[string]bool
	compat       bool // current session uses embedded tool calls
}

// New creates an agent. projectNotes is the optional BECODE.md content.
func New(cfg *config.Config, p provider.Provider, model string, reg *tools.Registry, projectNotes string) *Agent {
	a := &Agent{
		Cfg:          cfg,
		Provider:     p,
		Model:        model,
		Tools:        reg,
		projectNotes: projectNotes,
	}
	if cfg.RepoMap {
		a.repoMap = repomap.Build(reg.Root, cfg.RepoMapBudget)
	}
	a.retryBase = 2 * time.Second
	a.stallAfter = 45 * time.Second
	if cfg.StallNoticeSeconds > 0 {
		a.stallAfter = time.Duration(cfg.StallNoticeSeconds) * time.Second
	}
	a.applyModel(model)
	a.History = NewHistory(a.composeSystem(""), cfg.ContextTokens)
	a.applyReserve(cfg.ContextTokens) // until a real window is detected
	if reg.OnBeforeWrite == nil {
		reg.OnBeforeWrite = func(abs string) error { return a.Checkpoints.Record(abs) }
	}
	return a
}

// applyModel re-derives the model profile and tool-call mode.
func (a *Agent) applyModel(model string) {
	a.Model = model
	a.Profile = profiles.Detect(model)
	switch a.Cfg.CompatToolCalls {
	case "always":
		a.compat = true
	case "never":
		a.compat = false
	default: // "auto": the profile decides the starting point
		a.compat = a.Profile.Compat == "always"
	}
	a.knownTools = map[string]bool{}
	for _, n := range a.Tools.Names() {
		a.knownTools[n] = true
	}
}

// SetModel switches models mid-session, refreshing the profile.
func (a *Agent) SetModel(model string) {
	a.applyModel(model)
	if a.History != nil {
		a.History.System.Content = a.composeSystem("")
		// A thinking model needs a different reserve than a plain one.
		w := a.Window
		if w <= 0 {
			w = a.Cfg.ContextTokens
		}
		a.applyReserve(w)
	}
}

// SetGuidance sets extra system-prompt text and recomposes the prompt so it
// takes effect on the next call even when nothing else triggers a refresh.
func (a *Agent) SetGuidance(g string) {
	a.Guidance = g
	if a.History != nil {
		a.History.System.Content = a.composeSystem("")
	}
}

// RefreshSystem recomposes the system prompt after tools or guidance
// changed (used once at startup when the editor bridge attaches).
func (a *Agent) RefreshSystem() {
	a.knownTools = map[string]bool{}
	for _, n := range a.Tools.Names() {
		a.knownTools[n] = true
	}
	a.History.System.Content = a.composeSystem("")
}

// composeSystem builds the full system prompt: base + repo map + git state.
func (a *Agent) composeSystem(gitInfo string) string {
	sys := a.systemOverride
	if sys == "" {
		sys = BuildSystemPrompt(a.Tools.Specs(), a.compat || a.Cfg.CompatToolCalls == "auto", a.projectNotes)
	}
	if a.repoMap != "" && a.systemOverride == "" {
		sys += "\n\nRepository map (file: symbols):\n" + a.repoMap
	}
	if a.handoff != "" {
		sys += "\n\nHandoff from the previous session (honor its requirements and decisions):\n" + a.handoff
	}
	if a.Guidance != "" {
		sys += "\n\n" + a.Guidance
	}
	if gitInfo != "" {
		sys += "\n\n" + gitInfo
	}
	return sys
}

// RepoMap returns the current outline (for /map).
func (a *Agent) RepoMap() string { return a.repoMap }

// RefreshRepoMap rebuilds the outline (files changed since session start).
func (a *Agent) RefreshRepoMap() {
	if a.Cfg.RepoMap {
		a.repoMap = repomap.Build(a.Tools.Root, a.Cfg.RepoMapBudget)
	}
}

func (a *Agent) notice(format string, args ...any) {
	if a.Events.OnNotice != nil {
		a.Events.OnNotice(fmt.Sprintf(format, args...))
	}
}

// transient emits a status notice that need not be kept: it goes to
// OnTransient when the UI provides one, otherwise to OnNotice.
func (a *Agent) transient(format string, args ...any) {
	if a.Events.OnTransient != nil {
		a.Events.OnTransient(fmt.Sprintf(format, args...))
		return
	}
	a.notice(format, args...)
}

// stallSecondStage is when the second "still waiting" notice fires.
func stallSecondStage(first time.Duration) time.Duration { return 4 * first }

// Run processes one user request through the tool loop and returns the
// final assistant reply.
func (a *Agent) Run(ctx context.Context, userInput string) (string, error) {
	answer, err := a.run(ctx, userInput, true)
	a.refreshKeepAlive()
	return answer, err
}

// run is Run with control over checkpointing: repair rounds pass
// newTurn=false so the whole request (first attempt plus repairs) is one
// undo unit and one changed-files set for the reviewer.
func (a *Agent) run(ctx context.Context, userInput string, newTurn bool) (string, error) {
	start := time.Now()
	defer func() { a.Stats.Elapsed += time.Since(start) }()

	if newTurn {
		a.Checkpoints.BeginTurn(store.TitleFrom(userInput))
		a.reqTouched = false
	}
	if a.repoDirty {
		a.repoDirty = false
		a.RefreshRepoMap()
		a.History.System.Content = a.composeSystem("")
	}
	if gi := gitctx.Summary(ctx, a.Tools.Root); gi != "" {
		a.History.System.Content = a.composeSystem(gi)
	}
	expanded := ExpandMentions(a.Tools.Root, userInput)
	if newTurn && a.ContextProvider != nil {
		if note := a.ContextProvider(ctx); note != "" {
			a.notice("%s", strings.SplitN(note, "\n", 2)[0])
			expanded = note + "\n\n" + expanded
		}
	}
	a.History.Add(provider.Message{Role: provider.RoleUser, Content: expanded})

	emptyRetries, lengthRetries := 0, 0
	for turn := 0; turn < a.Cfg.MaxTurns; turn++ {
		// Anything the user typed while tools were running goes in now,
		// after the results the model was waiting on.
		a.deliverInbox()
		// Another client may have evicted or reloaded the model with a
		// different window since the last call; adapt before prompting.
		a.checkBackend(ctx)
		// Compact inside the tool loop too: one long agentic request can
		// blow the window on its own, long before the next user message.
		a.maybeCompact(ctx)
		req := provider.ChatRequest{
			Model:       a.Model,
			Messages:    a.History.Prompt(),
			Temperature: a.temperature(),
			MaxTokens:   a.Cfg.MaxTokens,
		}
		a.History.Extra = 0
		if !a.compat {
			req.Tools = a.Tools.Specs()
			a.History.Extra = a.specsTokens()
		}

		resp, err := a.chatWithRetry(ctx, req)
		if err != nil {
			// Some servers reject the tools field outright — fall back to
			// embedded tool calls for the rest of the session.
			if !a.compat && a.Cfg.CompatToolCalls != "never" && looksLikeToolsUnsupported(err) {
				a.compat = true
				a.notice("backend rejected native tool calls; switching to embedded format")
				continue
			}
			a.autosave(userInput) // keep the progress made before the failure
			return "", err
		}

		calls := resp.ToolCalls
		content := resp.Content
		if len(calls) == 0 && a.Cfg.CompatToolCalls != "never" {
			content, calls = ParseEmbeddedCalls(content, a.knownTools)
		}

		if len(calls) == 0 {
			if strings.TrimSpace(content) == "" {
				// Nothing usable came back. A length cutoff with no output
				// means the model exhausted the window — on a reasoning
				// model, usually while still thinking. Free context and
				// retry once; if nothing can be freed, explain precisely.
				if resp.FinishReason == "length" {
					if lengthRetries == 0 && a.freeContext(ctx) {
						lengthRetries++
						a.notice("model ran out of window while reasoning (%d chars of reasoning, no answer); freed context and retrying", len(resp.Reasoning))
						continue
					}
					a.autosave(userInput)
					return "", fmt.Errorf("model output was cut off (finish_reason=length) before it produced an answer: it spent the remaining window on reasoning (%d chars). Raise the backend window (OLLAMA_CONTEXT_LENGTH) or lower context_tokens so more of the window is reserved for generation", len(resp.Reasoning))
				}
				if emptyRetries == 0 {
					emptyRetries++
					a.notice("model returned an empty reply; asking it to continue")
					a.History.Add(provider.Message{Role: provider.RoleUser,
						Content: "Your previous reply was empty. Continue the task: either call a tool or give your final answer."})
					continue
				}
				a.autosave(userInput)
				return "", fmt.Errorf("model returned an empty reply twice in a row (backend may be truncating the prompt; check its context window against context_tokens)")
			}
			if resp.FinishReason == "length" {
				a.notice("reply was cut off by the output limit (max_tokens or the backend's context window)")
			}
			// Final answer.
			a.History.Add(provider.Message{Role: provider.RoleAssistant, Content: resp.Content})
			a.autosave(userInput)
			return strings.TrimSpace(resp.Content), nil
		}

		if len(resp.ToolCalls) > 0 {
			a.runNativeCalls(ctx, resp)
		} else {
			a.runEmbeddedCalls(ctx, resp.Content, content, calls)
		}
	}
	a.autosave(userInput)
	return "", fmt.Errorf("stopped after %d tool turns without a final answer (raise max_turns in config, or simplify the request)", a.Cfg.MaxTurns)
}

// freeContext makes room after a length cutoff: collapse every tool result
// but the newest, then summarize with the model if configured. Reports
// whether the prompt actually shrank.
func (a *Agent) freeContext(ctx context.Context) bool {
	h := a.History
	before := h.Tokens()
	h.CollapseToolResults(1)
	if a.Cfg.CompactWithModel && len(h.Messages) > 4 {
		if err := a.Compact(ctx); err != nil {
			a.notice("compaction failed (%v)", err)
		}
	}
	return h.Tokens() < before
}

// temperature returns the sampling temperature: the model profile's value
// when the user left config at its default, otherwise the configured one.
func (a *Agent) temperature() float64 {
	if a.Cfg.Temperature == config.Default().Temperature && a.Profile.Temperature > 0 {
		return a.Profile.Temperature
	}
	return a.Cfg.Temperature
}

// mayHaveChangedFiles reports whether this request could have modified the
// workspace: a file or shell tool ran, or the checkpointer recorded edits.
// Questions and explanations skip the (slow) verification suite.
func (a *Agent) mayHaveChangedFiles() bool {
	if a.reqTouched {
		return true
	}
	return a.Checkpoints != nil && len(a.Checkpoints.ChangedLast()) > 0
}

// specsTokens estimates the per-request cost of the native tools schema.
func (a *Agent) specsTokens() int {
	b, err := json.Marshal(a.Tools.Specs())
	if err != nil {
		return 0
	}
	return a.History.est(string(b))
}

// PIDAlive reports whether a pid is a running process. It is injected from
// cmd (live.Alive), like ReviewerFactory, so this package does not depend on
// the live-session registry. The default answers no: a build that never
// wires it up simply has no session hosts to collide with.
var PIDAlive = func(pid int) bool { return false }

// LiveOwner returns the pid of the advertised live host for a session code,
// and whether there is one. Injected from cmd over ~/.be-code/live.
var LiveOwner = func(code string) (pid int, ok bool) { return 0, false }

// SaveGuard reports whether a live session host owns this session's file,
// and which. Two programs owning one session file is what resuming an
// already-live session used to produce: each is blind to the other's turns
// and overwrites its saves. Every entry point that loads a session now
// joins the live one instead (see cmd/live.go:decideStart), so this is the
// last line of defence and should never fire in practice.
//
// Ownership takes two matching answers, not one: the file's pid stamp must
// be alive *and* be the pid the live registry advertises as this code's
// host. A bare pid is not evidence — pids are recycled, and an unrelated
// process inheriting the number of a host that crashed would otherwise lock
// a session out of its own file for good. A stamp that names no live host
// is taken over.
//
// The answer latches: a file that changes hands mid-run cannot un-block a
// program that already stood down from it.
func (a *Agent) SaveGuard() (blocked bool, owner int) {
	if a.Session == nil {
		return false, 0
	}
	if a.saveDisabled {
		return true, a.saveOwner
	}
	on, err := store.Load(a.Session.ID)
	if err != nil || on.HostPID == 0 || on.HostPID == os.Getpid() || !PIDAlive(on.HostPID) {
		return false, 0
	}
	if host, ok := LiveOwner(a.Session.ResumeCode()); !ok || host != on.HostPID {
		return false, 0
	}
	a.saveDisabled, a.saveOwner = true, on.HostPID
	return true, on.HostPID
}

// SetSession installs a session, clearing the save-guard latch: the new
// session is a different file with a different owner, and a run that stood
// down from one session must still be able to save the next (/clear, a
// resume after a blocked save).
func (a *Agent) SetSession(s *store.Session) {
	a.Session = s
	a.saveDisabled, a.saveOwner, a.saveWarned = false, 0, false
}

// autosave persists the conversation; failures are non-fatal by design.
// A session file a live host owns is never written (see SaveGuard); this
// program's own pid is stamped on every save it does make.
func (a *Agent) autosave(userInput string) {
	if a.Session == nil {
		return
	}
	if blocked, owner := a.SaveGuard(); blocked {
		if !a.saveWarned {
			a.saveWarned = true
			a.notice("session file is owned by live host %d; autosave disabled for this session", owner)
		}
		return
	}
	a.Session.HostPID = os.Getpid()
	if a.Session.Title == "" {
		a.Session.Title = store.TitleFrom(userInput)
	}
	a.Session.Model = a.Model
	a.Session.Messages = a.History.Messages
	if err := a.Session.Save(); err != nil {
		a.notice("session save failed: %v", err)
	}
}

// runNativeCalls executes provider-native tool calls with proper pairing.
func (a *Agent) runNativeCalls(ctx context.Context, resp *provider.ChatResponse) {
	a.History.Add(provider.Message{
		Role: provider.RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls,
	})
	for _, call := range resp.ToolCalls {
		res := a.dispatch(ctx, call)
		a.History.Add(provider.Message{
			Role: provider.RoleTool, ToolCallID: call.ID, Name: call.Name, Content: res.Content,
		})
	}
}

// runEmbeddedCalls executes prompt-format calls; results return as user
// messages, which every OpenAI-compatible server accepts.
func (a *Agent) runEmbeddedCalls(ctx context.Context, rawContent, _ string, calls []provider.ToolCall) {
	a.History.Add(provider.Message{Role: provider.RoleAssistant, Content: rawContent})
	var b strings.Builder
	for _, call := range calls {
		res := a.dispatch(ctx, call)
		status := "ok"
		if res.IsError {
			status = "error"
		}
		fmt.Fprintf(&b, "<tool_result name=%q status=%q>\n%s\n</tool_result>\n", call.Name, status, res.Content)
	}
	a.History.Add(provider.Message{Role: provider.RoleUser, Content: b.String()})
}

func (a *Agent) dispatch(ctx context.Context, call provider.ToolCall) tools.Result {
	a.Stats.ToolCalls++
	switch call.Name {
	case "write_file", "edit_file":
		a.reqTouched, a.repoDirty = true, true
	case "shell", "process":
		a.reqTouched = true
	}
	if a.Events.OnToolStart != nil {
		a.Events.OnToolStart(call.Name, call.Arguments)
	}
	res := a.Tools.Dispatch(ctx, call)
	if a.Events.OnToolEnd != nil {
		a.Events.OnToolEnd(call.Name, res)
	}
	return res
}

// chatFiltered runs one completion, applying the think-filter to streamed
// deltas and stored content when the model family emits reasoning blocks,
// and folding usage into session stats.
func (a *Agent) chatFiltered(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	onDelta := a.Events.OnDelta
	if a.Profile.StripThink && onDelta != nil {
		f := &ThinkFilter{}
		inner := onDelta
		onDelta = func(d string) {
			if out := f.Feed(d); out != "" {
				inner(out)
			}
		}
		defer func() {
			if tail := f.Flush(); tail != "" {
				inner(tail)
			}
		}()
	}
	onDelta, onReasoning, stopWatch := a.watchForStall(onDelta, a.Events.OnReasoning)
	req.OnReasoning = onReasoning
	resp, err := a.Provider.Chat(ctx, req, onDelta)
	stopWatch()
	if err != nil {
		return nil, err
	}
	if a.Profile.StripThink {
		resp.Content = StripThink(resp.Content)
	}
	a.Stats.Requests++
	if resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0 {
		a.Stats.PromptTokens += resp.Usage.PromptTokens
		a.Stats.CompletionTokens += resp.Usage.CompletionTokens
		// The server's count is ground truth for the prompt we just sent.
		a.History.Calibrate(resp.Usage.PromptTokens - a.History.Extra)
	} else {
		// Backend didn't report usage; estimate.
		for _, m := range req.Messages {
			a.Stats.PromptTokens += a.History.MessageTokens(m)
		}
		a.Stats.CompletionTokens += a.History.est(resp.Content)
	}
	return resp, nil
}

// maybeCompact summarizes old conversation with the model when the history
// exceeds its budget — model-written summaries preserve far more task
// state than dropping turns (the fallback when compaction fails).
func (a *Agent) maybeCompact(ctx context.Context) {
	h := a.History
	if !h.Over() || len(h.Messages) < 6 {
		return
	}
	before := h.Tokens()
	// Step 1: drop old tool output bodies and big call arguments, aiming
	// for Target (half the limit) so the run gets real runway. Usually
	// enough, and it keeps every conversational turn intact.
	if h.CollapseOldToolResults() {
		a.transient("context at %d of %d tokens; collapsed old tool traffic (now %d, target %d)", before, h.Limit(), h.Tokens(), h.Target())
		return
	}
	if !a.Cfg.CompactWithModel {
		return
	}
	// Step 2: still above target — summarize with the model.
	a.transient("context at %d of %d tokens (%.1f chars/token); compacting with the model", h.Tokens(), h.Limit(), h.CharsPerToken)
	if err := a.Compact(ctx); err != nil {
		a.notice("compaction failed (%v); falling back to trimming", err)
		return
	}
	a.transient("compacted to %d tokens (target %d)", h.Tokens(), h.Target())
}

// Compact replaces all but the newest messages with a model-written summary.
// The summary request always carries the original task, any previous
// summary, and the NEWEST part of the transcript — that is where the
// current state lives, so truncation drops the oldest lines first.
func (a *Agent) Compact(ctx context.Context) error {
	// Keep only the newest exchange verbatim: the summary carries the
	// rest, and a big tail defeats the point of compacting.
	const keepTail = 2
	const transcriptCap = 24 * 1024
	if len(a.History.Messages) <= keepTail {
		return fmt.Errorf("nothing to compact")
	}
	head := a.History.Messages[:len(a.History.Messages)-keepTail]
	tail := a.History.Messages[len(a.History.Messages)-keepTail:]

	task, prior := "", ""
	var b strings.Builder
	for i, m := range head {
		if i == 0 && strings.HasPrefix(m.Content, summaryPrefix) {
			prior = strings.TrimPrefix(m.Content, summaryPrefix)
			continue
		}
		if task == "" && m.Role == provider.RoleUser && !isToolResult(m) {
			task = m.Content
		}
		fmt.Fprintf(&b, "[%s] %.600s\n", m.Role, m.Content)
		for _, tc := range m.ToolCalls {
			fmt.Fprintf(&b, "  (called %s %.200s)\n", tc.Name, tc.Arguments)
		}
	}
	transcript := b.String()
	if len(transcript) > transcriptCap {
		cut := transcript[len(transcript)-transcriptCap:]
		if nl := strings.IndexByte(cut, '\n'); nl >= 0 {
			cut = cut[nl+1:]
		}
		transcript = "[earlier transcript omitted; see previous summary]\n" + cut
	}
	if len(task) > 2000 {
		task = task[:2000] + "..."
	}

	var u strings.Builder
	fmt.Fprintf(&u, "Original task:\n%s\n\n", task)
	if prior != "" {
		fmt.Fprintf(&u, "Previous summary:\n%s\n\n", prior)
	}
	fmt.Fprintf(&u, "Transcript (most recent last):\n%s", transcript)

	resp, err := a.Provider.Chat(ctx, provider.ChatRequest{
		Model: a.Model,
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: compactSystemPrompt},
			{Role: provider.RoleUser, Content: u.String()},
		},
		Temperature: 0.1,
		NoThink:     true, // a summary does not need minutes of deliberation
	}, nil)
	if err != nil {
		return err
	}
	summary := resp.Content
	if a.Profile.StripThink {
		summary = StripThink(summary)
	}
	if strings.TrimSpace(summary) == "" {
		return fmt.Errorf("empty summary")
	}
	a.History.Messages = append([]provider.Message{
		{Role: provider.RoleUser, Content: summaryPrefix + summary},
	}, tail...)
	a.History.RepairOrphans()
	// A tool result in the kept tail would leave the prompt nearly as big
	// as before; the summary already describes what it contained.
	a.History.CollapseToolResults(0)
	return nil
}

const summaryPrefix = "[Conversation summary — earlier turns compacted]\n"

const compactSystemPrompt = "Summarize this coding-agent conversation for context compression. Preserve, in this order: the original task; every requirement, constraint or convention the user stated; key decisions and why; files created or modified and how; current state; outstanding work. Under 400 words. Plain text."

func looksLikeToolsUnsupported(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "tool") &&
		(strings.Contains(s, "not support") || strings.Contains(s, "unsupported") ||
			strings.Contains(s, "invalid") || strings.Contains(s, "unknown field"))
}

// ReviewedReport bundles verification and (optional) reviewer results.
type ReviewedReport struct {
	Verify       *verify.Report
	Reviewed     bool
	ReviewIssues string // "" when approved (or review skipped)
}

// ReviewerFactory builds the reviewer provider when routing is configured.
// Injected by cmd to avoid an import cycle; nil disables review.
var ReviewerFactory func(cfg *config.Config) (provider.Provider, string, error)

// RunFull runs the request, then the verify→repair cycle, then (when
// configured) a second-model review with one repair round. This pipeline is
// the quality multiplier when the underlying model is a small local one.
func (a *Agent) RunFull(ctx context.Context, userInput string) (string, *ReviewedReport, error) {
	answer, err := a.Run(ctx, userInput)
	if err != nil {
		return "", nil, err
	}
	rep := &ReviewedReport{}
	if a.Cfg.VerifyOnDone && a.mayHaveChangedFiles() {
		proj := verify.Detect(a.Tools.Root)
		if len(proj.Checks) > 0 {
			for attempt := 0; attempt <= a.Cfg.MaxRepairs; attempt++ {
				rep.Verify = verify.RunChecks(ctx, a.Tools.Root, proj)
				if rep.Verify.Passed() || attempt == a.Cfg.MaxRepairs {
					break
				}
				a.notice("verification failed (%s); repair attempt %d/%d",
					rep.Verify.FailSummary(), attempt+1, a.Cfg.MaxRepairs)
				repairPrompt := fmt.Sprintf(
					"Verification failed. Fix ONLY these failures, then stop.\n\n%s\n\nRules: read the failing files before editing; make the smallest fix that makes the checks pass; do not refactor unrelated code.",
					rep.Verify.ModelSummary())
				answer, err = a.run(ctx, repairPrompt, false)
				if err != nil {
					return "", rep, err
				}
			}
			if rep.Verify != nil && !rep.Verify.Passed() {
				return answer, rep, nil // don't review broken code
			}
		}
	}

	// Reviewer routing: a second (usually larger) model critiques the diff.
	if a.Cfg.ReviewOnDone && a.Cfg.Reviewer.Model != "" && ReviewerFactory != nil {
		reviewer, reviewerModel, rerr := ReviewerFactory(a.Cfg)
		if rerr != nil {
			a.notice("reviewer unavailable: %v", rerr)
			return answer, rep, nil
		}
		a.notice("review pass: %s", reviewerModel)
		issues, rerr := a.Review(ctx, reviewer, reviewerModel)
		if rerr != nil {
			a.notice("review failed: %v", rerr)
			return answer, rep, nil
		}
		rep.Reviewed = true
		if issues != "" {
			rep.ReviewIssues = issues
			a.notice("reviewer raised issues; one repair round")
			answer, err = a.run(ctx, "A code reviewer raised these issues with your changes. Address the valid ones with minimal edits, then stop.\n\n"+issues, false)
			if err != nil {
				return "", rep, err
			}
			if a.Cfg.VerifyOnDone {
				proj := verify.Detect(a.Tools.Root)
				if len(proj.Checks) > 0 {
					rep.Verify = verify.RunChecks(ctx, a.Tools.Root, proj)
				}
			}
		}
	}
	return answer, rep, nil
}
