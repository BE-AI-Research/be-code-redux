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
	"sync/atomic"
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
	// observeFn is the recorder seam, swappable so the advisory discipline
	// above can be exercised against a store that panics or answers
	// nonsense without inventing one on disk. Nil means the real store.
	observeFn func(engine.Event) string
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

	projectNotes string
	handoff      string // briefing from the resumed session, kept in the system prompt
	// window is the backend context window when detected (0 = unknown),
	// read through Window(). It is atomic because it is written off the
	// agent goroutine — resolveModel lands a model switch's window from a
	// goroutine of its own — while a UI reads it to draw the context
	// wheel.
	window         atomic.Int64
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
	// nativeFallbackNotified keeps the native-endpoint downgrade to one
	// notice per session (see noteNativeFallback).
	nativeFallbackNotified bool
	repoMap                string
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

	// Model parameters (see the ModelLoader block below). modelMu guards
	// the model identity a switch rewrites — Model, Profile, compat,
	// knownTools — together with the loader and the switch generation,
	// because SetModel runs on a UI goroutine while resolveModel reads the
	// same fields from its own. It is never held across anything that can
	// block.
	modelMu  sync.Mutex
	loader   ModelLoader
	modelGen int
	// sessionMu guards the Session pointer against a UI reading it while
	// /clear or /resume swaps it from a goroutine of their own. It guards
	// the pointer, never what it points at.
	sessionMu sync.Mutex
	// turnMu is held for the whole of run(). It exists for exactly one
	// caller outside the loop: the compaction resolveModel does when a
	// model switch lands a window smaller than the conversation. That is
	// the only transcript rewrite that does not come from the tool loop,
	// and it must never interleave with one that does.
	turnMu sync.Mutex
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
	// NewHistory rescues an unset budget; the reserve must start from the
	// same number, or an unset context_tokens leaves the floor reserve
	// against a 16k budget until the real window arrives.
	budget, _, _ := a.History.Scalars()
	a.applyReserve(budget) // until a real window is detected
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

// applyModel re-derives the model profile and tool-call mode. It takes
// modelMu: a switch comes from a UI goroutine, and resolveModel reads the
// profile from its own to size the reserve.
func (a *Agent) applyModel(model string) {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	a.applyModelLocked(model)
}

// applyModelLocked is applyModel with modelMu already held, for SetModel,
// which has to rewrite the system prompt in the same critical section: the
// prompt is what a model resolution reads when it measures the
// conversation, and half a switch is not a state anything should measure.
func (a *Agent) applyModelLocked(model string) {
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

// SetProvider switches the backend mid-session. It exists so the notices
// that describe *a* backend cannot outlive the backend they described: a
// session that downgraded to the OpenAI path, or lost its model to an
// eviction, has said so once — and if the user then picks a different
// provider, the same thing happening there is news again. Assigning
// a.Provider directly leaves those latches set and the second downgrade
// silent, which is how a session ends up quietly unable to set its context
// window with nothing on screen to say so.
func (a *Agent) SetProvider(p provider.Provider) {
	a.Provider = p
	a.nativeFallbackNotified = false
	a.unloadedNotified = false
	// A loader speaks for one backend. Carrying the old one across a
	// provider switch would put another server's num_ctx on the wire — and
	// raise its consent question about a machine the user has just left —
	// while the session's own consent record described a different box
	// entirely. A fresh loader, or none at all if nothing can build one.
	if LoaderFactory != nil {
		a.SetLoader(LoaderFactory(a.Cfg, p))
	} else {
		a.SetLoader(nil)
	}
}

// LoaderFactory builds a model loader for one provider. Injected by cmd,
// the same import-cycle dodge ReviewerFactory and CoworkerFactory use; nil
// means a provider switch carries on with no parameter resolution, which is
// what a test or a scratch agent wants.
var LoaderFactory func(cfg *config.Config, p provider.Provider) ModelLoader

// SetModel switches models mid-session, refreshing the profile. The switch
// itself is synchronous — the next request uses the new model whatever the
// backend says — and the parameter resolution runs behind it.
func (a *Agent) SetModel(model string) {
	a.modelMu.Lock()
	a.applyModelLocked(model)
	if a.History != nil {
		a.History.System.Content = a.composeSystem("")
	}
	a.modelGen++
	gen, l := a.modelGen, a.loader
	a.modelMu.Unlock()
	if a.History != nil {
		// A thinking model needs a different reserve than a plain one.
		// Before a real window is known the budget is the best stand-in:
		// context_tokens is now routinely unset (it means "derive from the
		// window"), and reserving against 0 would drop a thinking model from
		// a 4096-token headroom to the 1024 floor.
		w := a.Window()
		if w <= 0 {
			w, _, _ = a.History.Scalars()
		}
		a.applyReserve(w) // takes modelMu itself, hence outside the block above
	}
	// The loader may prompt and may reload a model, so it never runs on the
	// caller's goroutine: /model returns now, the window lands as a notice.
	if l != nil {
		// Nothing goes on the wire in the meantime. Options belong to the
		// endpoint, not to a model, so until the resolution lands the
		// provider would still be carrying the *previous* model's num_ctx
		// — and a request sent in that gap reloads the new model at a
		// window nobody consented to, which is precisely what the gate
		// exists to prevent. Sending none at all leaves the server's own
		// choice alone. Only when there is a loader to put one back:
		// without one, nothing would ever restore it.
		a.clearWireWindow()
		a.goResolve(l, model, gen)
	}
}

// clearWireWindow takes the context window off the wire, leaving the
// passthrough options alone. It is the provider-agnostic half of "we do not
// know this model's window yet": an endpoint that carries no window has
// nothing to clear and does not implement the interface.
func (a *Agent) clearWireWindow() {
	if w, ok := a.Provider.(provider.WindowClearer); ok {
		w.ClearWindow()
	}
}

// goResolve starts one resolution on its own goroutine, under
// ModelResolveTimeout. Both callers come through here, because the last
// time they each carried their own copy of these three lines one of them
// lost the deadline — and a resolution that cannot time out holds the turn
// lock for the life of the process if its consent is never answered or its
// compaction never returns, wedging every later request, /clear and
// /compact behind it.
func (a *Agent) goResolve(l ModelLoader, model string, gen int) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), ModelResolveTimeout)
		defer cancel()
		a.resolveModel(ctx, l, model, gen)
	}()
}

// ModelLoader is the one path to a model's runtime parameters: it resolves
// them, puts them on the wire, and asks first whenever doing so would
// change what another application on a shared backend is using. The agent
// takes an interface rather than importing internal/loader — the same
// import-cycle dodge ReviewerFactory and CoworkerFactory use — and cmd
// injects the real one.
type ModelLoader interface {
	// Apply makes the model's parameters true on the backend and returns
	// the window the session should budget against; 0 means unknown.
	Apply(ctx context.Context, model string) (window int, err error)
	// OnEvicted is a model that is no longer resident. The next request
	// reloads it whatever we do, so the reload carries our parameters. No
	// consent: nothing was holding the model.
	OnEvicted(ctx context.Context, model string)
	// OnWindowChanged is another client having reloaded the model at a
	// different size. The loader adapts and never reloads back.
	OnWindowChanged(model string, window int)
	// KeepAlive is how long this model should stay resident: the models
	// entry, else the provider block, else the top-level setting. 0 means
	// nothing was configured, and the caller's own default applies.
	KeepAlive(model string) time.Duration
}

// ModelResolveTimeout bounds one parameter resolution, consent prompt
// included. It is generous because the question in the middle of it is one
// a person has to read; it exists so a session that is never answered does
// not carry a goroutine for the life of the process.
//
// Exported because the caller of ResolveModelNow has to apply it itself,
// with a context its own approval prompt also waits on (see there).
const ModelResolveTimeout = 2 * time.Minute

// SetLoader hands the agent the session's model loader. Nil disables the
// resolution entirely, which is what a test or a scratch agent wants.
func (a *Agent) SetLoader(l ModelLoader) {
	a.modelMu.Lock()
	a.loader = l
	a.modelMu.Unlock()
}

// ResolveModel resolves the current model's parameters through the loader,
// off the caller's goroutine, and lands the window it gets as a notice.
//
// Two callers: SetModel, and the UI once it has wired Registry.Approve.
// The second is why this is public. buildAgent runs before any UI exists,
// so the consent question spec 9.3 describes has nobody to put it to and is
// refused — correctly, but silently. Re-running it from runInteractive and
// runSessionHost is what turns that refusal back into a question.
func (a *Agent) ResolveModel() {
	a.modelMu.Lock()
	l, model := a.loader, a.Model
	a.modelGen++
	gen := a.modelGen
	a.modelMu.Unlock()
	if l == nil {
		return
	}
	a.goResolve(l, model, gen)
}

// ResolveModelNow is ResolveModel on the caller's goroutine. Plain mode
// needs it: its approval prompt reads the one terminal input stream the
// REPL loop is also reading, so a question raised from a second goroutine
// would race the user's own keystrokes. The REPL calls this from its own
// goroutine, once, before it starts reading lines.
//
// It applies no deadline of its own, deliberately. There is no goroutine
// here to leak, and a deadline applied here would be invisible to the
// prompt the loader raises through the caller's UI — a timeout that cannot
// withdraw the question it is timing is a lie. The caller bounds this with
// ModelResolveTimeout on a context its own prompt also waits on.
func (a *Agent) ResolveModelNow(ctx context.Context) {
	a.modelMu.Lock()
	l, model := a.loader, a.Model
	a.modelGen++
	gen := a.modelGen
	a.modelMu.Unlock()
	if l == nil {
		return
	}
	a.resolveModel(ctx, l, model, gen)
}

// resolveModel is the body of both, fenced against a loader that panics:
// model parameters are advisory, and a session that cannot learn its window
// still runs — at the budget it already had.
func (a *Agent) resolveModel(ctx context.Context, l ModelLoader, model string, gen int) {
	defer func() {
		if r := recover(); r != nil {
			a.notice("model parameters for %s could not be resolved: %v", model, r)
		}
	}()
	w, err := l.Apply(ctx, model)
	if err != nil || w <= 0 {
		// Every reason the loader has for declining is one it has already
		// explained in its own words; repeating it here would say it twice.
		return
	}
	// A switch that has been overtaken is not the session's model any more.
	// Its window must not land on the model the user actually chose: the
	// answers come back in whatever order the backend gives them.
	a.modelMu.Lock()
	stale := gen != a.modelGen
	a.modelMu.Unlock()
	if stale {
		return
	}
	prev := a.Window()
	if a.ApplyWindow(w) {
		budget, _, _ := a.History.Scalars()
		a.notice("%s runs with a %d-token window; budget now %d tokens", model, w, budget)
	}
	// A smaller window than the conversation already occupies would make
	// the next request truncate silently: compact once, now, and say so.
	//
	// Measuring the conversation means reading the transcript and the
	// system prompt, so it happens under both locks that can be rewriting
	// them: turnMu for a request in flight, modelMu for a switch landing
	// behind this one. A request holding turnMu is left alone entirely —
	// the tool loop compacts at the top of every model call, so nothing is
	// lost, and reading the transcript it is rewriting would be the race
	// this avoids.
	if !a.turnMu.TryLock() {
		if prev > 0 && w < prev {
			a.notice("%s has a smaller window (%d) than the model this request started with; the request in flight compacts if it needs to", model, w)
		}
		return
	}
	defer a.turnMu.Unlock()
	a.modelMu.Lock()
	tokens := a.History.Tokens() // the system prompt and the transcript
	a.modelMu.Unlock()
	if tokens <= a.History.Limit() { // the budget, read under its own lock
		return
	}
	a.notice("the new model's window is smaller than this conversation; compacting once")
	if err := a.Compact(ctx); err != nil {
		a.notice("compaction after the model switch failed: %v", err)
	}
}

// ClearHistory and CompactNow are the two transcript rewrites a *user* asks
// for — /clear and /compact — and they exist because the transcript now has
// more than one writer. Before model switching went through the loader, the
// tool loop was the only thing that touched History.Messages, and a UI was
// safe to touch it directly as long as it was not running. It is not the
// only thing any more: a switch resolves its window on a goroutine of its
// own and may compact on the spot, and the UI has no way to know that
// goroutine is there. Both of these take the same turn lock run() holds, so
// the three writers take turns instead of overlapping.
//
// **Neither may be called from inside a Bubble Tea Update.** The lock order
// in a served session is turn lock first, session lock second: everything
// that holds the turn lock — a request streaming deltas, a post-switch
// compaction emitting notices — writes to the session as it goes, and that
// takes the lock Update is holding. Waiting for the turn lock from inside
// Update inverts that and deadlocks the terminal. Both UIs call these from
// a goroutine of their own, with the UI in its busy state.
func (a *Agent) ClearHistory() {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	a.History.Messages = nil
}

// CompactNow is Compact under the turn lock, for a UI asking for it
// directly. The tool loop's own compaction is already inside run().
func (a *Agent) CompactNow(ctx context.Context) error {
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
	return a.Compact(ctx)
}

// modelLoader is the loader as the agent goroutine reads it (checkBackend).
func (a *Agent) modelLoader() ModelLoader {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	return a.loader
}

// Loader is this session's model loader, or nil. It is the read half of
// SetLoader and takes the same lock, because a provider switch replaces the
// loader from a UI goroutine while the tool loop reads it.
func (a *Agent) Loader() ModelLoader { return a.modelLoader() }

// Window is the backend context window this session budgets against, or 0
// when it is unknown.
func (a *Agent) Window() int { return int(a.window.Load()) }

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

// Notice delivers one message to whatever UI is wired and reports whether
// there was one. Startup wiring needs the answer: cmd builds the model
// loader before any UI exists, and a notice raised then has to fall back to
// stderr — but the same notice raised later belongs in the transcript,
// which under a TUI is the only place it can be read at all (stderr is
// wiped by the alt screen, or is a host log file).
func (a *Agent) Notice(msg string) bool {
	if a.Events.OnNotice == nil {
		return false
	}
	a.Events.OnNotice(msg)
	return true
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
	// Held for the whole request: the only other writer of the transcript
	// is resolveModel's post-switch compaction, which runs on a goroutine
	// of its own and steps aside rather than interleave with this.
	a.turnMu.Lock()
	defer a.turnMu.Unlock()
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
		// Evidence always has a home: if nothing is doing and no task is
		// open, the user's own message opens one. Everything a tool
		// returns from here on is recorded against a node the report can
		// later find it under.
		a.Engine.EnsureRoot(userInput)
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
	effort := a.Cfg.ReasoningEffort
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
			Model:           a.Model,
			Messages:        a.History.Prompt(),
			Temperature:     a.temperature(),
			MaxTokens:       a.Cfg.MaxTokens,
			ReasoningEffort: a.effortFor(effort),
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
					// Two levers, pulled together on the one retry: free
					// context, and ask for less thinking — the reasoning is
					// what ate the window, and a backend that honours
					// reasoning_effort (Ollama does) cuts it several-fold.
					freed := lengthRetries == 0 && a.freeContext(ctx)
					lowered := lengthRetries == 0 && effort != "low"
					if freed || lowered {
						lengthRetries++
						effort = "low"
						a.notice("model ran out of window while reasoning (%d chars of reasoning, no answer); freed context and retrying with reasoning_effort=low", len(resp.Reasoning))
						continue
					}
					a.autosave(userInput)
					return "", fmt.Errorf("model output was cut off (finish_reason=length) before it produced an answer: it spent the remaining window on reasoning (%d chars), even after a retry with reasoning_effort=low. Raise the backend window (OLLAMA_CONTEXT_LENGTH), set reasoning_effort: low in config, or lower context_tokens so more of the window is reserved for generation", len(resp.Reasoning))
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

// effortFor adapts the reasoning effort to the room left in the window:
// the configured level while the prompt is small, one level lower once
// the prompt fills more than half of it, because reasoning has to fit in
// what remains. "" (backend default) is left alone — there is no level to
// step down from.
func (a *Agent) effortFor(configured string) string {
	if configured == "" {
		return ""
	}
	if a.History.Tokens() <= a.History.Target() {
		return configured
	}
	switch configured {
	case "high":
		return "medium"
	case "medium":
		return "low"
	}
	return configured
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
	a.sessionMu.Lock()
	a.Session = s
	a.sessionMu.Unlock()
	a.saveDisabled, a.saveOwner, a.saveWarned = false, 0, false
}

// CurrentSession is the session as a goroutine that is not the agent's own
// reads it — a UI drawing the resume code in its header, or deciding
// whether a picked row is this program's own session.
//
// The agent's own reads of the field stay bare, and can: every one of them
// happens under the turn lock, which is also held by the two things that
// replace the session out of band (ClearHistory's caller, Resume). A UI
// holds no such lock, so it goes through here.
func (a *Agent) CurrentSession() *store.Session {
	a.sessionMu.Lock()
	defer a.sessionMu.Unlock()
	return a.Session
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
// the run down, so it is fenced, and what comes back is capped before it is
// appended to a tool result.
//
// Ruling T5-b: it is deliberately *not* bounded in time. A store wedged on
// its own mutex would make the very next composeSystem block inside Render
// anyway, so a timeout here saves nothing while costing a parked goroutine,
// a stall and a notice on every tool call. A bound that cannot hold is
// worse than none; bounding the store properly means timing every call
// behind one interface, which is later work.
func (a *Agent) observe(ev engine.Event) (footer string) {
	defer func() {
		if r := recover(); r != nil {
			a.notice("engine: %v; continuing without working memory", r)
			footer = ""
		}
	}()
	fn := a.observeFn
	if fn == nil {
		fn = a.Engine.Observe
	}
	return trimAtLine(fn(ev), maxObserveFooter)
}

// maxObserveFooter caps what the recorder may append to a tool result. The
// footer is one sentence by design; anything larger is a bug in the store,
// and the model should not pay for it in context.
const maxObserveFooter = 4096

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
	// The model and its profile are snapshotted rather than read where they
	// are used: this runs on the resolution's goroutine after a model
	// switch, and a *second* switch landing mid-compaction would otherwise
	// be read half-applied — a summary addressed to one model and stripped
	// as if it came from another.
	a.modelMu.Lock()
	model, stripThink := a.Model, a.Profile.StripThink
	a.modelMu.Unlock()
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
		Model: model,
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: compactSystemPrompt},
			{Role: provider.RoleUser, Content: u.String()},
		},
		Temperature: 0.1,
		NoThink:     true, // a summary does not need minutes of deliberation
	}, nil)
	if err != nil {
		// A summary that never arrived is the same situation as one that
		// arrived empty: the tree is still current, so the session keeps
		// working from it rather than falling through to blind trimming.
		// Except when the user cancelled — Esc, or an interrupted
		// /compact. That is not a backend failing to answer, it is a
		// person asking to stop, and rewriting history under them is the
		// opposite of what they asked for.
		if ctx.Err() != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "compaction: summary request failed: %v\n", err)
		if a.fromTaskRecord(ctx, tail) {
			return nil
		}
		return err
	}
	summary := resp.Content
	if stripThink {
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
		// A cancellation is not a backend failure: it reaches here as an
		// empty reply when the provider returns what it had, and the whole
		// cancelled path is silent — no diagnostic, no rewritten history.
		if ctx.Err() != nil {
			return ctx.Err()
		}
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
		if a.fromTaskRecord(ctx, tail) {
			return nil
		}
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

// fromTaskRecord is the branch this whole design turns on. When the model's
// summary call fails or comes back empty, the session continues from the
// task record instead of falling back to blind trimming: the tree is
// already current — the recorder wrote it as the work happened, without a
// model call — so the transcript is the only thing that needs cutting.
//
// It returns false when the request was cancelled, or when there is no
// record to continue from (no engine, or an empty block); those are the
// cases that still report an error.
func (a *Agent) fromTaskRecord(ctx context.Context, tail []provider.Message) bool {
	// A cancelled request is the user asking to stop, not a backend
	// failing to answer: rewriting history under them is the opposite of
	// what they asked for, so the caller's error stands.
	if ctx.Err() != nil || a.Engine == nil {
		return false
	}
	if strings.TrimSpace(a.Engine.Render(a.Cfg.Engine.Budget, a.inRepoMap)) == "" {
		return false
	}
	a.notice("compaction: the model returned no summary; continuing from the task record")
	// The block lives in the system prompt, so recomposing it is what puts
	// the current tree in front of the model in place of the turns being
	// dropped here.
	a.History.System.Content = a.composeSystem(a.lastGitInfo)
	a.History.Messages = append([]provider.Message{
		{Role: provider.RoleUser, Content: summaryPrefix + noSummaryNote},
	}, tail...)
	a.History.RepairOrphans()
	a.History.CollapseToolResults(0)
	return true
}

// noSummaryNote stands in for the summary in the transcript, and says where
// the state actually is so the model does not go looking for it in turns
// that are no longer there.
const noSummaryNote = "No summary of the earlier turns was produced. " +
	"The task record under \"Working memory:\" in the system prompt is current: " +
	"it is what those turns amounted to, and it is what to work from."

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
