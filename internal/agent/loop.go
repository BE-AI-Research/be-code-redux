// Package agent implements the BE-Code agentic loop: model turns, tool
// dispatch, context budgeting, and the verification/repair cycle.
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/brown-enterprises/be-code/internal/checkpoint"
	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
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
	// Co-working (see cowork.go): a consultation starting, the co-worker
	// reading files, and its result. All optional.
	OnConsultStart    func(name, question, origin string)
	OnConsultProgress func(name string, filesRead int)
	OnConsultEnd      func(res ConsultResult, err error)
}

// Stats accumulates per-session usage for /stats and the status bar.
type Stats struct {
	PromptTokens     int
	CompletionTokens int
	Requests         int // model round-trips
	ToolCalls        int
	Elapsed          time.Duration
}

// add folds another usage record into s.
func (s *Stats) add(d Stats) {
	s.PromptTokens += d.PromptTokens
	s.CompletionTokens += d.CompletionTokens
	s.Requests += d.Requests
	s.ToolCalls += d.ToolCalls
	s.Elapsed += d.Elapsed
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
	// Stats is cumulative session usage. The agent goroutine writes it
	// through addStats; a consultation issued from a UI while a run is in
	// progress writes it from that UI's goroutine, and every reader that
	// is not the agent goroutine itself takes Usage() — statsMu keeps the
	// two apart.
	Stats   Stats
	statsMu sync.Mutex
	// Engine is the working-memory store (nil when disabled): what the
	// model has read, looked up and decided, kept by the harness and put
	// back in the system prompt after compaction. See internal/engine.
	// The field itself is set before a run starts (SetEngine) and read
	// only on the agent goroutine; the store's own mutex guards its
	// contents for the UI. Toggling the engine mid-run would have to go
	// through the run state, not through this field.
	Engine *engine.Store
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
	lastGitInfo    string // this request's git summary, for the per-turn prompt recompose

	// lastUserInput and lastFailingTool feed Agent.RecentContext (see
	// cowork.go): the current request and the newest failing tool result,
	// both reset at the start of each new turn.
	lastUserInput   string
	lastFailingTool string

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

	// Co-working state (see cowork.go). coworkers is the usable co-worker
	// list, resolved once at New and read-only thereafter; consults is the
	// current run's budget spend, reset by RunFull; consultCount is
	// per-session usage for /coworkers; coworkAllowed is session-wide
	// consent for an online co-worker.
	//
	// Two locks, deliberately: consultMu serialises whole consultations
	// (one at a time, held across the approval prompt and the co-worker's
	// run), while coworkMu guards only the three counters, which a UI
	// goroutine reads and writes — /coworkers and the approval modal's
	// "a" — while the agent goroutine is inside Consult. coworkMu is
	// never held across a call that can block.
	coworkers     []config.CoworkerConfig
	consults      int
	consultCount  map[string]int
	coworkAllowed map[string]bool
	consultMu     sync.Mutex
	coworkMu      sync.Mutex

	// The harness's own consultation triggers (see autoConsult). These
	// three are touched only on the agent goroutine — inside run,
	// dispatch and RunFull — and so need no lock, unlike the counters
	// above. autoVerifyUsed keeps the verify trigger to once per request;
	// toolFailStreak counts consecutive failures of one tool and fires at
	// exactly three; pendingAdvice parks the answer until the loop is
	// back at the top, so advice lands as a user note after the tool
	// results the model was waiting on rather than in the middle of them.
	autoVerifyUsed bool
	toolFailStreak toolFailStreak
	pendingAdvice  string
}

// toolFailStreak is one run of consecutive failures of the same tool:
// its name, how many in a row, and the newest three error texts, which
// are what the co-worker is actually shown.
type toolFailStreak struct {
	name string
	n    int
	last []string
}

// New creates an agent. projectNotes is the optional BECODE.md content.
func New(cfg *config.Config, p provider.Provider, model string, reg *tools.Registry, projectNotes string) *Agent {
	a := &Agent{
		Cfg:          cfg,
		Provider:     p,
		Model:        model,
		Tools:        reg,
		projectNotes: TrimProjectNotes(projectNotes),
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
	// Co-workers: the warnings belong to cmd, which calls ValidCoworkers
	// itself and prints them once at startup.
	a.coworkers, _ = cfg.ValidCoworkers()
	a.consultCount = map[string]int{}
	a.coworkAllowed = map[string]bool{}
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

// MaxProjectNotes caps the project notes (BECODE.md/CLAUDE.md content) that
// enter the system prompt, whether loaded at startup (cmd/root.go's
// loadProjectNotes) or written live by /init (SetProjectNotes) — one
// definition so a session never carries more than a restart would load.
const MaxProjectNotes = 8 * 1024

// TrimProjectNotes cuts notes down to MaxProjectNotes, at the last line
// boundary that fits so the prompt never ends mid-sentence — and never
// mid-rune, which a plain byte slice would risk (an 8 KiB boundary landing
// inside a multi-byte character leaves the model reading U+FFFD). Every
// path that feeds project notes into the prompt goes through here:
// SetProjectNotes, cmd's loadProjectNotes, and agent.New.
func TrimProjectNotes(s string) string { return trimAtLine(s, MaxProjectNotes) }

// trimAtLine cuts s to max bytes at the last line boundary that fits, and
// failing that at the last whole rune.
func trimAtLine(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := s[:max]
	if i := strings.LastIndexByte(cut, '\n'); i > 0 {
		return cut[:i+1]
	}
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1] // back off to the last whole rune
	}
	return cut
}

// SetProjectNotes replaces the BECODE.md content and recomposes the system
// prompt (used after init writes a new file). Notes are trimmed to
// MaxProjectNotes so a long-lined document or fact-sheet fallback can't
// enter the live prompt any larger than what a restart would load.
func (a *Agent) SetProjectNotes(notes string) {
	a.projectNotes = TrimProjectNotes(notes)
	a.RefreshSystem()
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
		if a.Engine == nil {
			// No store, no Working memory block: keep the git sentences (the
			// tools exist) but drop the paragraph that points at the block.
			if full := engineGuidance(a.Tools.Specs()); full != "" {
				sys = strings.TrimSuffix(sys, "\n\n"+full)
				if g := guidanceFor(a.Tools.Specs(), false); g != "" {
					sys += "\n\n" + g
				}
			}
		}
	}
	if a.repoMap != "" && a.systemOverride == "" {
		sys += "\n\nRepository map (file: symbols):\n" + a.repoMap
	}
	if a.Engine != nil && a.systemOverride == "" {
		if wm := a.Engine.Render(a.Cfg.Engine.Budget, a.inRepoMap); wm != "" {
			sys += "\n\nWorking memory:\n" + wm
		}
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

// SetEngine attaches the working-memory store and recomposes the prompt.
func (a *Agent) SetEngine(s *engine.Store) {
	a.Engine = s
	if a.History != nil {
		a.History.System.Content = a.composeSystem("")
	}
}

// inRepoMap reports whether the repository map lists a file's symbols.
func (a *Agent) inRepoMap(path string) bool {
	return a.repoMap != "" && (strings.HasPrefix(a.repoMap, path+":") || strings.Contains(a.repoMap, "\n"+path+":"))
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
	defer func() { a.addStats(Stats{Elapsed: time.Since(start)}) }()
	a.lastGitInfo = ""
	defer func() {
		if a.Engine != nil {
			if err := a.Engine.Flush(); err != nil {
				a.notice("engine: %v; continuing without working memory", err)
			}
		}
	}()

	// A streak belongs to one stretch of tool calls; a repair round is a
	// fresh start, and advice from a previous round has either been
	// delivered or been overtaken by events.
	a.toolFailStreak = toolFailStreak{}
	a.pendingAdvice = ""

	if newTurn {
		a.Checkpoints.BeginTurn(store.TitleFrom(userInput))
		a.reqTouched = false
		a.lastUserInput = userInput
		a.lastFailingTool = ""
	}
	if newTurn && a.Engine != nil {
		a.Engine.EnsureTask(userInput)
	}
	if a.repoDirty {
		a.repoDirty = false
		a.RefreshRepoMap()
		a.History.System.Content = a.composeSystem("")
	}
	if gi := gitctx.Summary(ctx, a.Tools.Root); gi != "" {
		a.lastGitInfo = gi
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
		if a.Engine != nil {
			a.Engine.NextTurn()
		}
		// Anything the user typed while tools were running goes in now,
		// after the results the model was waiting on.
		a.deliverInbox()
		// A co-worker's answer to a repeated tool failure goes in the
		// same way and for the same reason: after the tool results, as
		// plain user text the model cannot mistake for its own.
		if a.pendingAdvice != "" {
			a.History.Add(provider.Message{Role: provider.RoleUser, Content: a.pendingAdvice})
			a.pendingAdvice = ""
		}
		// Another client may have evicted or reloaded the model with a
		// different window since the last call; adapt before prompting.
		a.checkBackend(ctx)
		// Recompose before compacting, not only once per request: the
		// working-memory block has to reflect the reads made earlier in
		// this same turn, and compaction has to measure the prompt it is
		// actually about to send.
		a.History.System.Content = a.composeSystem(a.lastGitInfo)
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
	a.addStats(Stats{ToolCalls: 1})
	switch call.Name {
	case "write_file", "edit_file":
		a.reqTouched, a.repoDirty = true, true
	case "shell", "process":
		a.reqTouched = true
	}
	if a.Events.OnToolStart != nil {
		a.Events.OnToolStart(call.Name, call.Arguments)
	}
	var res tools.Result
	served := false
	if a.Engine != nil && (call.Name == "search" || call.Name == "lookup" || call.Name == "history") {
		if args, ok := tools.ParseArgs(call.Arguments); ok {
			if cached, hit := a.Engine.Cached(call.Name, args); hit {
				res, served = tools.Result{Content: cached}, true
			}
		}
	}
	if !served {
		res = a.Tools.Dispatch(ctx, call)
	}
	if a.Engine != nil && !served {
		// The same tolerant parse Dispatch used, so a double-encoded call
		// is observed exactly as it ran; arguments no tool could run are
		// simply not observed.
		if args, ok := tools.ParseArgs(call.Arguments); ok {
			if footer := a.observe(engine.Event{Tool: call.Name, Args: args, Content: res.Content, IsError: res.IsError}); footer != "" {
				res.Content = strings.TrimRight(res.Content, "\n") + "\n" + footer
			}
		}
	}
	// The automatic tool-failure consultation's question, decided here but
	// asked below, after OnToolEnd has put the failure on screen.
	consult := ""
	if res.IsError {
		a.lastFailingTool = call.Name + ": " + res.Content
		// Three failures of the same tool in a row is the signature of a
		// small model that has stopped reading the error and started
		// guessing. Ask a co-worker once per streak; a fourth failure is
		// the same stuck state, not new information.
		if a.toolFailStreak.name == call.Name {
			a.toolFailStreak.n++
		} else {
			a.toolFailStreak = toolFailStreak{name: call.Name, n: 1}
		}
		a.toolFailStreak.last = append(a.toolFailStreak.last, res.Content)
		if len(a.toolFailStreak.last) > 3 {
			a.toolFailStreak.last = a.toolFailStreak.last[1:]
		}
		if a.toolFailStreak.n == 3 && a.Cfg.Cowork.Auto && len(a.coworkers) > 0 && call.Name != "consult" {
			consult = fmt.Sprintf("the tool %s has failed three times in a row with these arguments:\n%s\n\nerrors:\n- %s",
				call.Name, call.Arguments, strings.Join(a.toolFailStreak.last, "\n- "))
		}
	} else {
		a.toolFailStreak = toolFailStreak{}
	}
	if a.Events.OnToolEnd != nil {
		a.Events.OnToolEnd(call.Name, res)
	}
	// After OnToolEnd, not before: the consultation announces itself
	// ("consulting big…") and renders its question as it goes, and a
	// question that answers a failure has to appear below that failure
	// rather than above the line it is about.
	if consult != "" {
		if name, advice, ok := a.autoConsult(ctx, "auto:tool", consult, nil); ok {
			a.pendingAdvice = fmt.Sprintf("A co-worker (%s) looked at the repeated %s failure and advises:\n\n%s",
				name, call.Name, advice)
		}
	}
	return res
}

// observe hands a tool result to the engine; a panic there must not take
// the run down, so it is fenced.
func (a *Agent) observe(ev engine.Event) (footer string) {
	defer func() {
		if r := recover(); r != nil {
			a.notice("engine: %v; continuing without working memory", r)
			footer = ""
		}
	}()
	return a.Engine.Observe(ev)
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
	used := Stats{Requests: 1}
	if resp.Usage.PromptTokens > 0 || resp.Usage.CompletionTokens > 0 {
		used.PromptTokens = resp.Usage.PromptTokens
		used.CompletionTokens = resp.Usage.CompletionTokens
		// The server's count is ground truth for the prompt we just sent.
		a.History.Calibrate(resp.Usage.PromptTokens - a.History.Extra)
	} else {
		// Backend didn't report usage; estimate.
		for _, m := range req.Messages {
			used.PromptTokens += a.History.MessageTokens(m)
		}
		used.CompletionTokens = a.History.est(resp.Content)
	}
	a.addStats(used)
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
	// tool call id → path, read_file calls only; a backend that omits ids
	// falls back to call_<index>, which is not unique across turns, so
	// every call (not just read_file) rewrites its id's entry and a result
	// clears it once consumed — a reused id can then never mis-stub an
	// unrelated result as a digested read.
	pending := map[string]string{}
	for i, m := range head {
		if i == 0 && strings.HasPrefix(m.Content, summaryPrefix) {
			prior = strings.TrimPrefix(m.Content, summaryPrefix)
			continue
		}
		if task == "" && m.Role == provider.RoleUser && !isToolResult(m) {
			task = m.Content
		}
		if a.Engine != nil {
			for _, tc := range m.ToolCalls {
				path := ""
				if tc.Name == "read_file" {
					if args, ok := tools.ParseArgs(tc.Arguments); ok {
						// read_file takes any of these spellings, and an
						// absolute path inside the workspace: DigestKey
						// folds them onto the key the digest is under, so
						// the stub is substituted for the read either way.
						for _, k := range []string{"path", "file", "filename"} {
							if v, _ := args[k].(string); strings.TrimSpace(v) != "" {
								path = a.Engine.DigestKey(v)
								break
							}
						}
					}
				}
				pending[tc.ID] = path
			}
		}
		if a.Engine != nil && m.Role == provider.RoleTool {
			if p, ok := pending[m.ToolCallID]; ok {
				delete(pending, m.ToolCallID)
				if p != "" {
					if r, has := a.Engine.HasDigest(p); has {
						fmt.Fprintf(&b, "[tool] (read %s lines %d–%d; digested)\n", p, r.From, r.To)
						continue
					}
				}
			}
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
	if a.Engine != nil {
		if wm := a.Engine.Render(a.Cfg.Engine.Budget, a.inRepoMap); wm != "" {
			fmt.Fprintf(&u, "Working memory:\n%s\n\n", wm)
		}
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
	filesOnly := false
	if a.Engine != nil {
		body, files := engine.SplitFilesBlock(summary)
		if files != "" {
			a.Engine.ApplyFileNotes(files)
			filesOnly = strings.TrimSpace(body) == ""
		}
		summary = body
		if err := a.Engine.Flush(); err != nil {
			a.notice("engine: %v; continuing without working memory", err)
		}
	}
	if strings.TrimSpace(summary) == "" {
		// An empty summary is the one compaction failure a user actually
		// sees, and the reply's shape is the only clue to why: say what
		// the backend reported, and keep the raw head on stderr (the host
		// log) where it survives the session.
		why := fmt.Sprintf("finish=%s, %d prompt tokens, %d completion tokens, reasoning %d chars, raw reply %d chars",
			resp.FinishReason, resp.Usage.PromptTokens, resp.Usage.CompletionTokens, len(resp.Reasoning), len(resp.Content))
		if filesOnly {
			why = "files block only; " + why
		}
		head := resp.Content
		if len(head) > 400 {
			head = head[:400]
		}
		fmt.Fprintf(os.Stderr, "compaction: empty summary (%s); reply head: %q\n", why, head)
		return fmt.Errorf("empty summary: %s", why)
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

// SummaryPrefix marks the user-role message a compaction leaves in place
// of the turns it summarised; UIs use it to render that message as a
// summary block rather than as something the person typed.
const SummaryPrefix = "[Conversation summary — earlier turns compacted]\n"

const summaryPrefix = SummaryPrefix

const compactSystemPrompt = "Summarize this coding-agent conversation for context compression. Preserve, in this order: the original task; every requirement, constraint or convention the user stated; key decisions and why; files created or modified and how; current state; outstanding work. Under 400 words. Plain text. Do not restate anything already in Working memory. End with a line `files:` followed by one line per file that mattered, as `- path — what matters in it`."

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
	a.resetConsults() // the consultation budget is per request
	a.autoVerifyUsed = false
	if a.Engine != nil {
		// A new request gets a fresh task line unless a plan is still in
		// flight; mid-request repair rounds go through run, which only
		// fills an empty one.
		a.Engine.StartTask(userInput)
		if head := gitctx.Head(ctx, a.Tools.Root); head != "" {
			// The porcelain text itself, not a hash of it: the changes tool
			// names the files that were already dirty when the task began,
			// and this is the only moment that list can be observed. Capped
			// like project notes so a repository mid-rebase cannot put a
			// megabyte of status into the ledger.
			dirty := trimAtLine(gitctx.Porcelain(ctx, a.Tools.Root), MaxProjectNotes)
			a.Engine.SetBaseline(engine.Baseline{Head: head, Dirty: dirty})
		}
	}
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
			// The repair budget is spent and the check still fails: the
			// primary has run out of ideas, which is exactly when a
			// co-worker is worth the wait. One extra round, once per
			// request — if that does not fix it, a second opinion on the
			// same evidence would not either.
			if rep.Verify != nil && !rep.Verify.Passed() && a.Cfg.Cowork.Auto && len(a.coworkers) > 0 && !a.autoVerifyUsed {
				a.autoVerifyUsed = true
				files := a.changedFiles()
				if name, advice, ok := a.autoConsult(ctx, "auto:verify",
					"the change still fails this check; what is wrong and what minimal edit fixes it\n\n"+rep.Verify.ModelSummary(), files); ok {
					a.notice("co-worker %s advised; one more repair round", name)
					answer, err = a.run(ctx, fmt.Sprintf("A co-worker (%s) reviewed the failing check and advises:\n\n%s\n\nApply the minimal fix, then stop.", name, advice), false)
					if err != nil {
						return "", rep, err
					}
					rep.Verify = verify.RunChecks(ctx, a.Tools.Root, proj)
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

// addStats records usage under statsMu.
func (a *Agent) addStats(d Stats) {
	a.statsMu.Lock()
	a.Stats.add(d)
	a.statsMu.Unlock()
}

// Usage is a snapshot of Stats safe to read while the agent is working.
func (a *Agent) Usage() Stats {
	a.statsMu.Lock()
	defer a.statsMu.Unlock()
	return a.Stats
}

// usageTokens is what a scratch agent (plan, consultation) hands back to
// its parent: its tokens and round-trips, not its tool calls or elapsed
// time, which the parent's own turn already accounts for.
func (a *Agent) usageTokens() Stats {
	u := a.Usage()
	return Stats{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens, Requests: u.Requests}
}
