package agent

import (
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/repomap"
	"github.com/brown-enterprises/be-code/internal/subagent"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// The working-memory store is advisory (spec §3): nothing it does may fail a
// turn, block the loop or panic a session. Every call the agent makes into
// it therefore goes through engineDo, the one fence. Before, only observe
// recovered — so a store that panicked inside Render took the session down
// from composeSystem, which runs before every model call, and one that
// panicked inside Flush took it down from a deferred call on the way out of
// a request that had otherwise succeeded.

// engine is the store as the agent uses it: nil when there is none, and nil
// once a panic has detached it for the session. The exported field stays as
// it was for the UIs' own /task and /notes commands; they read it from their
// own goroutines, which is why detaching is a flag beside the field and not
// a write to it.
func (a *Agent) engine() *engine.Store {
	if a.engineOff.Load() {
		return nil
	}
	return a.Engine
}

// engineDo runs one call into the store behind the fence. A panic raises one
// notice and detaches the engine for the rest of the session: a store that
// has panicked once has state nobody can vouch for, and a notice per tool
// call is how an advisory component ends up louder than the work.
//
// Ruling T5-b stands: this bounds panics, not time. A store wedged on its
// own mutex still parks the caller.
func (a *Agent) engineDo(op string, fn func(st *engine.Store)) {
	st := a.engine()
	if st == nil {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			if a.engineOff.CompareAndSwap(false, true) {
				a.notice("engine: %s failed (%v); continuing without working memory for the rest of this session", op, r)
			}
		}
	}()
	if a.engineFault != nil {
		a.engineFault(op)
	}
	fn(st)
}

// memoryShare is the part of the usable window the Working memory block may
// take, and memoryFloor the least it is ever given. Ruling F-1.
const (
	memoryShare = 0.25
	memoryFloor = 2048
)

// memoryBudget is the byte cap on the Working memory block: about a quarter
// of the live History.Limit(), converted to bytes at the calibrated ratio,
// floored at 2 KB and never above engine.budget. It follows the window, so a
// switch to a smaller model shrinks the block along with everything else —
// a fixed 6144 bytes is a sixth of a 16k window and most of a 4k one.
func (a *Agent) memoryBudget() int {
	max := a.Cfg.Engine.Budget
	if max <= 0 {
		max = engine.DefaultBudget
	}
	if a.History == nil {
		return max
	}
	_, _, ratio := a.History.Scalars()
	if ratio <= 0 {
		ratio = defaultCharsPerToken
	}
	b := int(float64(a.History.Limit()) * ratio * memoryShare)
	if b < memoryFloor {
		b = memoryFloor
	}
	if b > max {
		b = max
	}
	return b
}

const (
	// repoMapShare is the repo map's slice of the usable context. With the
	// working-memory block's quarter that leaves the conversation at least
	// half, which is the half compaction has to work with.
	repoMapShare = 0.20
	repoMapFloor = 2048
)

// repoMapBudget is the byte cap the repo map is built to: repo_map_budget is
// a ceiling, and the window decides how much of it there is room for. A fixed
// 32 KB at a 32k window was 70% of the usable context before the first
// message.
func (a *Agent) repoMapBudget() int {
	max := a.Cfg.RepoMapBudget
	if max <= 0 || a.History == nil {
		return max
	}
	_, _, ratio := a.History.Scalars()
	if ratio <= 0 {
		ratio = defaultCharsPerToken
	}
	b := int(float64(a.History.Limit()) * ratio * repoMapShare)
	if b < repoMapFloor {
		b = repoMapFloor
	}
	if b > max {
		b = max
	}
	return b
}

// fitRepoMap rebuilds the repo map when the budget it was built to no longer
// matches the window, and says once when the configured budget was cut. It
// runs on the agent goroutine at the top of a request: a rebuild walks the
// workspace, which has no business on a UI thread.
func (a *Agent) fitRepoMap() {
	if !a.Cfg.RepoMap {
		return
	}
	want := a.repoMapBudget()
	if want == a.repoMapBuilt {
		return
	}
	a.repoMap = repomap.Build(a.Tools.Root, want)
	a.repoMapBuilt = want
	if a.History != nil {
		a.History.System.Content = a.composeSystem(a.lastGitInfo)
	}
	if want < a.Cfg.RepoMapBudget && want != a.repoMapNoticed {
		a.repoMapNoticed = want
		a.notice("repo map built to %d bytes, not the configured repo_map_budget of %d: this window leaves room for that much of it", want, a.Cfg.RepoMapBudget)
	}
}

// warnHeavyPrompt says once when the fixed prompt alone takes more than half
// the usable context. Compaction cannot shrink it, so it will fire often, and
// the user has the knobs that would help.
func (a *Agent) warnHeavyPrompt() {
	if a.heavyPromptNoticed || a.History == nil {
		return
	}
	floor, limit := a.History.Floor(), a.History.Limit()
	if floor*2 <= limit {
		return
	}
	a.heavyPromptNoticed = true
	a.notice("the fixed prompt is %d of %d usable tokens, more than half: compaction cannot shrink it and will fire often. "+
		"A larger context_window, or a smaller repo_map_budget or engine.budget, gives the conversation more room", floor, limit)
}

// workingMemory is the block as the prompt and the compaction request both
// carry it: one render, one budget.
func (a *Agent) workingMemory() string {
	wm := ""
	a.engineDo("render", func(st *engine.Store) {
		wm = st.Render(a.memoryBudget(), a.inRepoMap)
	})
	return wm
}

// flushEngine saves the record. A failure is said once: on a read-only
// project it would otherwise repeat after every request, and what it used to
// say — "continuing without working memory" — was not true, since the record
// keeps working in memory and only fails to reach the disk.
func (a *Agent) flushEngine() {
	a.engineDo("flush", func(st *engine.Store) {
		err := st.Flush()
		a.engineWarnings(st)
		if err != nil && a.flushWarned.CompareAndSwap(false, true) {
			a.notice("engine: the task record could not be saved (%v); working memory still works in this session but will not outlive it", err)
		}
	})
}

// engineWarnings puts what the store needs the human to hear on the notice
// path. The store has no UI of its own, and its stderr is the host's log file
// in a hosted session — where a quarantined document, a second doing mark
// and a document set aside by a conflicting write all used to be announced.
func (a *Agent) engineWarnings(st *engine.Store) {
	for _, w := range st.TakeWarnings() {
		a.notice("engine: %s", w)
	}
}

// TaskLedger is the store as the task tool sees it: every call behind the
// same fence, and a no-op once the engine is detached — the tool stays
// registered and keeps answering, it simply remembers nothing, exactly as it
// does over a store that would not open.
func (a *Agent) TaskLedger() tools.TaskLedger { return fencedLedger{a} }

type fencedLedger struct{ a *Agent }

func (l fencedLedger) Plan(text string, steps []string) (id string) {
	l.a.engineDo("task plan", func(st *engine.Store) { id = st.Plan(text, steps) })
	return id
}

func (l fencedLedger) Add(parent, text string) (id string, err error) {
	l.a.engineDo("task add", func(st *engine.Store) { id, err = st.Add(parent, text) })
	return id, err
}

func (l fencedLedger) SetStatusText(id, status, reason string) (err error) {
	l.a.engineDo("task status", func(st *engine.Store) { err = st.SetStatusText(id, status, reason) })
	return err
}

func (l fencedLedger) Note(id, text, file string, decision, keep bool) (err error) {
	l.a.engineDo("task note", func(st *engine.Store) { err = st.Note(id, text, file, decision, keep) })
	return err
}

func (l fencedLedger) ShowText(id string) (out string) {
	l.a.engineDo("task show", func(st *engine.Store) { out = st.ShowText(id) })
	return out
}

// SetOwner and SetScope are the model's own `task owner` / `task scope`,
// and they take exactly the path the operator's `/task assign` and `/task
// scope` take. Calling the store directly and scheduling afterwards was not
// the same thing: AssignOwner stops a parked run and waits for it before
// rewriting the owner (spec §3.5, which says the model may reassign a
// parked node), and SetScope pushes the widened scope into the running
// sub-agent's registry and resolves its ask (§2.7). Both schedule
// themselves, so nothing is scheduled twice here.
//
// pinned is always false: the pin is the operator's mark, and the model may
// not set it (nor change one that is set — SetOwner refuses that).
func (l fencedLedger) SetOwner(id, owner string, _ bool) error {
	return l.a.AssignOwner(id, owner, false)
}

func (l fencedLedger) SetScope(id string, scope []string) error {
	return l.a.SetScope(id, scope)
}

// Reply answers a sub-agent's open ask_main (the task tool's taskReplier).
func (l fencedLedger) Reply(id, text string) error { return l.a.ReplyAsk(id, text) }

// ActiveRootID is the task tool's optional taskRoots capability.
func (l fencedLedger) ActiveRootID() (id string) {
	l.a.engineDo("task root", func(st *engine.Store) { id = st.ActiveRootID() })
	return id
}

// EngineBaseline is where the current task began, for the changes tool.
func (a *Agent) EngineBaseline() (head, dirty string) {
	a.engineDo("baseline", func(st *engine.Store) {
		b := st.Baseline()
		head, dirty = b.Head, b.Dirty
	})
	return head, dirty
}

// SetEngineCards hands the store the sub-agent cards it validates owner
// tags against. EnableSubAgents calls it with the real cards; the wiring
// calls it with nil when no sub-agent is configured, so an orphaned
// assignment in a task document is still reported once.
func (a *Agent) SetEngineCards(cards map[string]subagent.Card) {
	a.engineDo("sub-agent cards", func(st *engine.Store) { st.SetCards(cards) })
}
