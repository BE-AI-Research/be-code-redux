package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/profiles"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// A co-working model is a second, usually stronger, model the primary can
// consult mid-task. It inspects the repository with read-only tools and
// answers; the primary keeps every write and shell capability. Consult is
// the one entry point: the consult tool, the harness's own triggers in
// RunFull/run, and the /consult command all come through here.

// ConsultRequest is one question for a co-worker.
type ConsultRequest struct {
	Who      string // co-worker name; "" = the first configured
	Question string
	Files    []string // workspace-relative paths to read first
	Origin   string   // "tool" | "auto:verify" | "auto:tool" | "user:<label>"
	Recent   string   // the caller's recent context, already condensed
}

// ConsultResult is what a consultation produced. Partial marks an answer
// the co-worker had begun before its backend failed: worth showing, but
// not a finished reply.
type ConsultResult struct {
	Coworker string
	Answer   string
	Partial  bool
	Read     int
	Elapsed  time.Duration
	// Started is true once the co-worker was actually run: OnConsultStart
	// has fired and OnConsultEnd will. A refusal before that point (unknown
	// name, the cap, a decline, no factory) returns only an error, and a
	// caller that prints its own error line uses this to avoid saying it
	// twice.
	Started bool
}

// CoworkerFactory builds a co-worker's provider. Injected by cmd, like
// ReviewerFactory, so this package never imports the provider registry.
var CoworkerFactory func(cfg *config.Config, cw config.CoworkerConfig) (provider.Provider, error)

// ConsultFrame is the co-worker's system prompt. The project notes are
// appended after it.
const ConsultFrame = `You are a co-working model advising a smaller local model that is doing the work and keeps the pen. It has asked for help because it is stuck or out of its depth.

Rules:
- Inspect what you need with the read-only tools (read_file, list_dir, search) before answering. You cannot edit files or run commands, and you must not present running a command as the answer.
- Answer with concrete, minimal guidance: what is wrong, and the exact change to make, with file:line references. Prefer the smallest change that fixes the problem.
- If the question cannot be answered from the code, say so plainly and say what would be needed.
- Be brief. The reader is a small model: short numbered steps beat prose.`

const (
	consultFileCap   = 8 * 1024
	consultRecentCap = 6 * 1024
	// The seed is bounded twice over: per file, and across all of them. A
	// model that asks for forty files would otherwise fill the co-worker's
	// window with the repository and leave no room for the answer.
	consultMaxFiles    = 8
	consultAllFilesCap = 32 * 1024
)

// errConsultCapped and errConsultDeclined mark the two ordinary refusals:
// the per-run budget is spent, or the user said no. Both are outcomes the
// harness's automatic triggers must accept in silence, so they are
// sentinels rather than bare strings; the messages they produce are
// unchanged, and a caller that only prints them sees no difference.
var (
	errConsultCapped   = errors.New("consultation limit reached for this run")
	errConsultDeclined = errors.New("consultation declined")
)

// Coworkers is the usable co-worker list, in configured order. The slice
// is copied: a UI listing co-workers must not be able to reorder or
// overwrite the agent's own.
func (a *Agent) Coworkers() []config.CoworkerConfig {
	out := make([]config.CoworkerConfig, len(a.coworkers))
	copy(out, a.coworkers)
	return out
}

// ConsultsThisRun is how many consultations the current RunFull has used
// (tool-initiated and automatic together; /consult never counts).
func (a *Agent) ConsultsThisRun() int {
	a.coworkMu.Lock()
	defer a.coworkMu.Unlock()
	return a.consults
}

// ConsultCount is how many times a co-worker has been consulted this session.
func (a *Agent) ConsultCount(name string) int {
	a.coworkMu.Lock()
	defer a.coworkMu.Unlock()
	return a.consultCount[name]
}

// AllowCoworker records session-wide consent for an online co-worker: the
// approval modal's "a".
func (a *Agent) AllowCoworker(name string) { a.allow(name) }

// ---- counter access -------------------------------------------------------
//
// The three counters are shared between the agent goroutine (inside
// Consult) and whatever UI goroutine renders /coworkers or answers the
// approval modal. Every touch goes through these helpers, which hold
// coworkMu for the duration of a map or int operation and nothing longer:
// coworkMu is never held while blocked in Tools.Approve or the co-worker's
// own run.

// bumpConsult records one attempted consultation and returns the run total.
func (a *Agent) bumpConsult(name string, counts bool) int {
	a.coworkMu.Lock()
	defer a.coworkMu.Unlock()
	if counts {
		a.consults++
	}
	a.consultCount[name]++
	return a.consults
}

// resetConsults clears the per-run budget (RunFull, once per request).
func (a *Agent) resetConsults() {
	a.coworkMu.Lock()
	defer a.coworkMu.Unlock()
	a.consults = 0
}

// allowedFor reports session-wide consent for an online co-worker.
func (a *Agent) allowedFor(name string) bool {
	a.coworkMu.Lock()
	defer a.coworkMu.Unlock()
	return a.coworkAllowed[name]
}

// allow records session-wide consent for an online co-worker.
func (a *Agent) allow(name string) {
	a.coworkMu.Lock()
	defer a.coworkMu.Unlock()
	a.coworkAllowed[name] = true
}

func (a *Agent) coworkerByName(who string) (config.CoworkerConfig, error) {
	if len(a.coworkers) == 0 {
		return config.CoworkerConfig{}, errors.New("no co-working models are configured")
	}
	if who == "" {
		return a.coworkers[0], nil
	}
	names := make([]string, 0, len(a.coworkers))
	for _, cw := range a.coworkers {
		if cw.Name == who {
			return cw, nil
		}
		names = append(names, cw.Name)
	}
	return config.CoworkerConfig{}, fmt.Errorf("unknown co-worker %q (configured: %s)", who, strings.Join(names, ", "))
}

// consent decides whether code may be sent to cw for this request. Local
// co-workers never ask; a person's own /consult never asks; -y allows
// (through AutoApproveConsult, which only -y sets — never the shell
// approval's "a" and never the config file); otherwise the approval seam is
// asked once, and "a" (AllowCoworker) or a previous session-wide yes skips it.
func (a *Agent) consent(cw config.CoworkerConfig, req ConsultRequest) bool {
	if !cw.Online || strings.HasPrefix(req.Origin, "user:") || a.allowedFor(cw.Name) {
		return true
	}
	if a.Cfg.AutoApproveConsult { // what -y sets; never the config file
		a.allow(cw.Name)
		return true
	}
	if a.Tools.Approve == nil {
		return false
	}
	files := "none"
	if len(req.Files) > 0 {
		files = strings.Join(req.Files, ", ")
	}
	detail := fmt.Sprintf("coworker: %s (%s/%s)\norigin: %s\nquestion: %s\nfiles: %s\nit may read other files in this workspace; your recent request, reply and tool output, the project notes and the git summary go with the question; nothing is edited",
		cw.Name, cw.Provider, cw.Model, req.Origin, req.Question, files)
	return a.Tools.Approve("consult", detail)
}

// ConsentCoworker pulls the co-worker's name out of a consult approval's
// detail, whose first line is `coworker: <name> (<provider>/<model>)` (see
// consent, just above). "" for anything else, so a changed detail format
// degrades to a one-off approval rather than allowing the wrong name for the
// session. Both UIs use it for the approval's "always": the TUI modal's "a"
// and the REPL's "a", which is why it lives here rather than in either.
func ConsentCoworker(detail string) string {
	line := strings.SplitN(detail, "\n", 2)[0]
	rest, ok := strings.CutPrefix(line, "coworker: ")
	if !ok {
		return ""
	}
	// The name itself cannot contain " (": the provider/model suffix is the
	// last one, so trim from there.
	if i := strings.LastIndex(rest, " ("); i >= 0 {
		rest = rest[:i]
	}
	return strings.TrimSpace(rest)
}

// Consult runs one consultation. It never affects the primary's own
// history; its outcome is returned to the caller, who decides what to do
// with it. Every failure is an error (or a partial result), never a panic
// or a hang: the primary's run must go on without the co-worker.
func (a *Agent) Consult(ctx context.Context, req ConsultRequest) (res ConsultResult, err error) {
	// One consultation at a time: a second automatic trigger while one is
	// in flight is dropped rather than queued behind it.
	if !a.consultMu.TryLock() {
		return res, errors.New("a consultation is already running")
	}
	defer a.consultMu.Unlock()

	cw, err := a.coworkerByName(req.Who)
	if err != nil {
		return res, err
	}
	res.Coworker = cw.Name
	// Nothing to ask the user about, and nothing to charge them for, if
	// there is no way to build the co-worker in the first place.
	if CoworkerFactory == nil {
		return res, errors.New("co-working is not wired in this build")
	}
	counts := !strings.HasPrefix(req.Origin, "user:")
	if counts && a.ConsultsThisRun() >= a.Cfg.Cowork.MaxConsultsPerRun {
		return res, fmt.Errorf("%w (%d)", errConsultCapped, a.Cfg.Cowork.MaxConsultsPerRun)
	}
	if !a.consent(cw, req) {
		return res, errConsultDeclined
	}
	// A consultation that was attempted has been paid for, however it
	// ends: a failing co-worker must not be retried without limit.
	a.bumpConsult(cw.Name, counts)
	cp, err := CoworkerFactory(a.Cfg, cw)
	if err != nil {
		return res, fmt.Errorf("co-worker %s unavailable: %w", cw.Name, err)
	}

	if a.Events.OnConsultStart != nil {
		a.Events.OnConsultStart(cw.Name, req.Question, req.Origin)
	}
	start := time.Now()
	res.Started = true
	defer func() {
		res.Elapsed = time.Since(start)
		if a.Events.OnConsultEnd != nil {
			a.Events.OnConsultEnd(res, err)
		}
	}()

	// A co-worker whose backend has stopped answering must not park the
	// primary's run for the rest of the day. The deadline is the derived
	// context's alone: parent stays in hand so a cancellation by the user
	// is still reported as the user's, not as a timeout.
	parent := ctx
	if secs := a.Cfg.Cowork.ConsultTimeout; secs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(secs)*time.Second)
		defer cancel()
	}

	scratch := a.consultAgent(cp, cw, &res)
	seed := a.buildConsultSeed(ctx, scratch.Tools, req)
	answer, rerr := scratch.Run(ctx, seed)
	a.addStats(scratch.usageTokens())
	if rerr != nil {
		// Whatever it had said before failing is worth returning either
		// way, so a UI can show it.
		res.Answer = sanitizeAdvice(scratch.lastAssistantText())
		// A cancelled consultation is not a partial answer: the user
		// walked away from it, and no caller may feed it back to the
		// primary as advice.
		if parent.Err() != nil {
			return res, parent.Err()
		}
		// The parent is alive and only the derived deadline fired: the
		// co-worker hung. Said plainly, so autoConsult's notice names the
		// wait rather than blaming an unreachable backend.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return res, fmt.Errorf("co-worker %s timed out after %ds", cw.Name, a.Cfg.Cowork.ConsultTimeout)
		}
		// Half an answer from a co-worker whose backend died mid-reply
		// still beats nothing; the caller marks it as partial.
		if res.Answer != "" {
			res.Partial = true
			return res, nil
		}
		return res, rerr
	}
	res.Answer = sanitizeAdvice(strings.TrimSpace(answer))
	return res, nil
}

// sanitizeAdvice strips tool-call and tool-result markup from a co-worker's
// answer before it is fed back to the primary. A compat-mode co-worker
// emits <tool_call>…</tool_call> in plain text, and its answer can quote
// the <tool_result> wrappers it was given; pasted into the primary's
// history that text is indistinguishable from a real call, and
// ParseEmbeddedCalls would dispatch it — advice that runs itself. Stray
// half-tags (a reply cut off mid-block) go too, so nothing is left to pair
// with a later tag.
func sanitizeAdvice(s string) string {
	for _, name := range []string{"tool_call", "tool_result"} {
		openTag, closeTag := "<"+name+">", "</"+name+">"
		for {
			i := strings.Index(s, openTag)
			if i < 0 {
				break
			}
			j := strings.Index(s[i:], closeTag)
			if j < 0 {
				break
			}
			s = s[:i] + s[i+j+len(closeTag):]
		}
		s = strings.ReplaceAll(s, openTag, "")
		s = strings.ReplaceAll(s, closeTag, "")
	}
	return strings.TrimSpace(s)
}

// autoConsult is the harness's own consultation: the two automatic
// triggers — verification exhausted (RunFull) and the same tool failing
// three times running (dispatch) — both come through here so they share
// one shape. It names the co-worker before asking, because the notice a
// user sees ("consulting big…") has to arrive before the wait it explains,
// and it hands over the primary's recent context so the co-worker sees
// what went wrong rather than only the question.
//
// A consultation that fails never fails the run: the caller gets ok=false
// and carries on exactly as it would have with no co-workers configured.
// The per-run cap and a declined consultation are ordinary outcomes and
// stay silent; anything else earns one line.
func (a *Agent) autoConsult(ctx context.Context, origin, question string, files []string) (string, string, bool) {
	// Consult resolves "" to the first configured co-worker, but only
	// after the notice below has to be written — so resolve it the same
	// way, here, and let Consult repeat the work harmlessly.
	cw, err := a.coworkerByName("")
	if err != nil {
		return "", "", false
	}
	// A run that has spent its consultation budget must not announce a
	// consultation it will never make: the announcement outlives the
	// run in plain mode, where transient notes are ordinary lines.
	if a.ConsultsThisRun() >= a.Cfg.Cowork.MaxConsultsPerRun {
		return "", "", false
	}
	name := cw.Name
	a.transient("consulting %s…", name)
	res, err := a.Consult(ctx, ConsultRequest{
		Question: question,
		Files:    files,
		Origin:   origin,
		Recent:   a.RecentContext(),
	})
	if err != nil {
		// The cap and a decline are the run's own decisions, and a
		// cancelled run was the user's; none of them is the co-worker
		// being unavailable.
		if !errors.Is(err, errConsultCapped) && !errors.Is(err, errConsultDeclined) && !errors.Is(err, context.Canceled) {
			a.notice("co-worker unavailable: %v", err)
		}
		return "", "", false
	}
	// A co-worker that answered with nothing has nothing to add; feeding
	// an empty advice note back would spend a repair round on silence.
	if strings.TrimSpace(res.Answer) == "" {
		return "", "", false
	}
	return res.Coworker, res.Answer, true
}

// changedFiles is Checkpoints.ChangedLast with a nil checkpointer
// tolerated: an agent built without one (tests, and any embedder that
// does not want undo) still gets the automatic triggers, just without the
// file list to hand the co-worker.
func (a *Agent) changedFiles() []string {
	if a.Checkpoints == nil {
		return nil
	}
	return a.Checkpoints.ChangedLast()
}

// RecentContext condenses what the primary was doing for a co-worker: the
// current request, the primary's newest reply, and the newest failing tool
// output (recorded by dispatch as it happens, so it is exact rather than
// guessed at from history). Capped at consultRecentCap.
func (a *Agent) RecentContext() string {
	var b strings.Builder
	if a.lastUserInput != "" {
		b.WriteString("User request:\n" + a.lastUserInput + "\n\n")
	}
	if reply := a.lastAssistantText(); reply != "" {
		b.WriteString("Your last reply:\n" + reply + "\n\n")
	}
	if a.lastFailingTool != "" {
		b.WriteString("Failing tool output:\n" + a.lastFailingTool + "\n")
	}
	return cutTail(b.String(), consultRecentCap)
}

// consultAgent is the read-only scratch agent for one consultation: the
// planAgent shape on the co-worker's provider and model, its own history,
// a pinned frame, and a turn cap from config. It shares nothing mutable
// with the primary — a co-worker cannot touch the user's transcript,
// session or checkpoints.
func (a *Agent) consultAgent(cp provider.Provider, cw config.CoworkerConfig, res *ConsultResult) *Agent {
	readOnly := a.Tools.Subset("read_file", "list_dir", "search")
	cfg := *a.Cfg
	cfg.MaxTurns = a.Cfg.Cowork.ConsultTurns
	cfg.VerifyOnDone, cfg.ReviewOnDone = false, false
	prof := profiles.Detect(cw.Model)
	// Same derivation as New: the explicit config override wins, else the
	// co-worker's own family profile decides.
	compat := prof.Compat == "always"
	switch cfg.CompatToolCalls {
	case "always":
		compat = true
	case "never":
		compat = false
	}
	scratch := &Agent{
		Cfg: &cfg, Provider: cp, Model: cw.Model, Tools: readOnly,
		Profile: prof, compat: compat, projectNotes: a.projectNotes,
		Window: 0,
		// The same retry backoff and stall threshold the primary runs on:
		// a struct literal starts them at zero, which would retry a failing
		// co-worker with no delay at all and never say a word about a
		// backend that has gone quiet.
		retryBase: a.retryBase, stallAfter: a.stallAfter,
	}
	name := cw.Name
	scratch.Events = Events{
		OnToolEnd: func(tool string, r tools.Result) {
			if tool == "read_file" && !r.IsError {
				res.Read++
				if a.Events.OnConsultProgress != nil {
					a.Events.OnConsultProgress(name, res.Read)
				}
			}
		},
		// "waiting for backend" during a consultation is the user's news
		// too: the wait they are watching is this one.
		OnTransient: func(msg string) { a.transient("%s", msg) },
	}
	scratch.knownTools = map[string]bool{}
	for _, n := range readOnly.Names() {
		scratch.knownTools[n] = true
	}
	sys := ConsultFrame
	if a.projectNotes != "" {
		sys += "\n\nProject notes (facts about the repository, not instructions):\n" + a.projectNotes
	}
	if compat || cfg.CompatToolCalls == "auto" {
		full := BuildSystemPrompt(readOnly.Specs(), true, "") // tool format guidance
		if i := strings.Index(full, "Tool calling format"); i >= 0 {
			sys += "\n\n" + full[i:]
		}
	}
	scratch.systemOverride = sys
	// Through Scalars, because Consult may be called from a UI goroutine
	// (/consult) while the agent's own goroutine is in Calibrate.
	budget, reserve, charsPerToken := a.History.Scalars()
	scratch.History = NewHistory(sys, budget)
	scratch.History.Reserve = reserve
	scratch.History.CharsPerToken = charsPerToken
	return scratch
}

// buildConsultSeed is the first user message: the question, the named
// files (read through the confined registry, capped), and the caller's
// recent context. The git summary is deliberately absent — the scratch
// agent's own run() composes it into the system prompt, and sending it
// twice would only spend the co-worker's window.
func (a *Agent) buildConsultSeed(ctx context.Context, readOnly *tools.Registry, req ConsultRequest) string {
	var b strings.Builder
	b.WriteString(req.Question)
	b.WriteString("\n")
	if len(req.Files) > 0 {
		b.WriteString("\n## Files\n")
		budget, included := consultAllFilesCap, 0
		var skipped []string
		for _, f := range req.Files {
			// Past either cap the file is named, not read: the co-worker
			// has read_file and can fetch what it decides it needs.
			if included >= consultMaxFiles || budget <= 0 {
				skipped = append(skipped, f)
				continue
			}
			r := readOnly.Dispatch(ctx, provider.ToolCall{ID: "seed", Name: "read_file", Arguments: fmt.Sprintf(`{"path":%q}`, f)})
			body := r.Content
			switch {
			case r.IsError:
				// Say so, rather than passing the failure off as content.
				body = "(could not read: " + strings.TrimSpace(r.Content) + ")"
			default:
				limit := consultFileCap
				if budget < limit {
					limit = budget
				}
				if len(body) > limit {
					body = cutHead(body, limit) + "\n… (truncated; read_file for the rest)"
				}
			}
			included++
			budget -= len(body)
			fmt.Fprintf(&b, "\n### %s\n%s\n", f, body)
		}
		if len(skipped) > 0 {
			fmt.Fprintf(&b, "\n(not included: %s)\n", strings.Join(skipped, ", "))
		}
	}
	if req.Recent != "" {
		b.WriteString("\n## Recent context\n" + cutTail(req.Recent, consultRecentCap) + "\n")
	}
	return b.String()
}

// cutHead keeps at most n bytes from the front of s, backing off to a rune
// boundary so a cap never lands inside a multi-byte character (the model
// would read U+FFFD) — the convention TrimProjectNotes follows.
func cutHead(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// cutTail keeps at most n bytes from the end of s, moving forward to a
// rune boundary.
func cutTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := len(s) - n
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return s[i:]
}

// lastAssistantText is the newest assistant message content, for a
// consultation cut short after it had started answering.
func (a *Agent) lastAssistantText() string {
	for i := len(a.History.Messages) - 1; i >= 0; i-- {
		m := a.History.Messages[i]
		if m.Role == provider.RoleAssistant && strings.TrimSpace(m.Content) != "" {
			return strings.TrimSpace(m.Content)
		}
	}
	return ""
}
