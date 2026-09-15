package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/gitctx"
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
)

// Coworkers is the usable co-worker list, in configured order.
func (a *Agent) Coworkers() []config.CoworkerConfig { return a.coworkers }

// ConsultsThisRun is how many consultations the current RunFull has used
// (tool-initiated and automatic together; /consult never counts).
func (a *Agent) ConsultsThisRun() int { return a.consults }

// ConsultCount is how many times a co-worker has been consulted this session.
func (a *Agent) ConsultCount(name string) int { return a.consultCount[name] }

// AllowCoworker records session-wide consent for an online co-worker: the
// approval modal's "a".
func (a *Agent) AllowCoworker(name string) { a.coworkAllowed[name] = true }

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
// co-workers never ask; a person's own /consult never asks; -y allows;
// otherwise the approval seam is asked once, and "a" (AllowCoworker) or a
// previous session-wide yes skips it.
func (a *Agent) consent(cw config.CoworkerConfig, req ConsultRequest) bool {
	if !cw.Online || strings.HasPrefix(req.Origin, "user:") || a.coworkAllowed[cw.Name] {
		return true
	}
	if a.Cfg.AutoApproveShell { // what -y sets
		a.coworkAllowed[cw.Name] = true
		return true
	}
	if a.Tools.Approve == nil {
		return false
	}
	files := "none"
	if len(req.Files) > 0 {
		files = strings.Join(req.Files, ", ")
	}
	detail := fmt.Sprintf("coworker: %s (%s/%s)\norigin: %s\nquestion: %s\nfiles: %s\nit may read other files in this workspace; nothing is edited",
		cw.Name, cw.Provider, cw.Model, req.Origin, req.Question, files)
	return a.Tools.Approve("consult", detail)
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
	counts := !strings.HasPrefix(req.Origin, "user:")
	if counts && a.consults >= a.Cfg.Cowork.MaxConsultsPerRun {
		return res, fmt.Errorf("consultation limit reached for this run (%d)", a.Cfg.Cowork.MaxConsultsPerRun)
	}
	if !a.consent(cw, req) {
		return res, errors.New("consultation declined")
	}
	// A consultation that was attempted has been paid for, however it
	// ends: a failing co-worker must not be retried without limit.
	if counts {
		a.consults++
	}
	a.consultCount[cw.Name]++
	if CoworkerFactory == nil {
		return res, errors.New("co-working is not wired in this build")
	}
	cp, err := CoworkerFactory(a.Cfg, cw)
	if err != nil {
		return res, fmt.Errorf("co-worker %s unavailable: %w", cw.Name, err)
	}

	if a.Events.OnConsultStart != nil {
		a.Events.OnConsultStart(cw.Name, req.Question, req.Origin)
	}
	start := time.Now()
	defer func() {
		res.Elapsed = time.Since(start)
		if a.Events.OnConsultEnd != nil {
			a.Events.OnConsultEnd(res, err)
		}
	}()

	scratch := a.consultAgent(cp, cw, &res)
	seed := a.buildConsultSeed(ctx, scratch.Tools, req)
	answer, rerr := scratch.Run(ctx, seed)
	a.Stats.PromptTokens += scratch.Stats.PromptTokens
	a.Stats.CompletionTokens += scratch.Stats.CompletionTokens
	a.Stats.Requests += scratch.Stats.Requests
	if rerr != nil {
		// Half an answer from a co-worker that died mid-reply still beats
		// nothing; the caller marks it as partial.
		if partial := scratch.lastAssistantText(); partial != "" {
			res.Answer, res.Partial = partial, true
			return res, nil
		}
		return res, rerr
	}
	res.Answer = strings.TrimSpace(answer)
	return res, nil
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
		repoMap: a.repoMap, Window: 0,
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
	scratch.History = NewHistory(sys, a.History.Budget)
	scratch.History.Reserve = a.History.Reserve
	scratch.History.CharsPerToken = a.History.CharsPerToken
	return scratch
}

// buildConsultSeed is the first user message: the question, the named
// files (read through the confined registry, capped), the caller's recent
// context, and the git summary.
func (a *Agent) buildConsultSeed(ctx context.Context, readOnly *tools.Registry, req ConsultRequest) string {
	var b strings.Builder
	b.WriteString(req.Question)
	b.WriteString("\n")
	if len(req.Files) > 0 {
		b.WriteString("\n## Files\n")
		for _, f := range req.Files {
			r := readOnly.Dispatch(ctx, provider.ToolCall{ID: "seed", Name: "read_file", Arguments: fmt.Sprintf(`{"path":%q}`, f)})
			body := r.Content
			if len(body) > consultFileCap {
				body = body[:consultFileCap] + "\n… (truncated; read_file for the rest)"
			}
			fmt.Fprintf(&b, "\n### %s\n%s\n", f, body)
		}
	}
	if req.Recent != "" {
		recent := req.Recent
		if len(recent) > consultRecentCap {
			recent = recent[len(recent)-consultRecentCap:]
		}
		b.WriteString("\n## Recent context\n" + recent + "\n")
	}
	if g := gitctx.Summary(ctx, a.Tools.Root); g != "" {
		b.WriteString("\n## Git\n" + g + "\n")
	}
	return b.String()
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
