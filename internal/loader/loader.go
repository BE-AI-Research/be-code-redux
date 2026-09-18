// Package loader decides one model's runtime parameters and makes them true
// on the backend. It is the only place a model's context window, keep-alive
// and options block are resolved, so there is one place to look when they are
// wrong.
//
// Two facts shape everything here.
//
// The first is that probing costs time. Asking Ollama what window a model
// will run with means /api/ps, then the Modelfile, and — in the code this
// package replaces — loading the model, which on a large local model is a
// multi-minute stall before the session has even started. So an explicitly
// configured window means no probe at all.
//
// The second is that the backend is shared. Sending a num_ctx that differs
// from how a model is currently loaded makes Ollama reload it, evicting
// whatever else on that machine was using it. That is a change to somebody
// else's service, not a setting of ours, so it goes through the same approval
// seam as a shell command or a file write, under the action "model_reload".
// A nil approver — a headless run, a scripted run, a session whose UI has not
// wired one yet — is a refusal. Never a silent yes.
package loader

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// probeTimeout bounds the two cheap reads (/api/ps, /api/show) the loader is
// still allowed to make when nothing is configured. It is deliberately short:
// an unreachable or busy backend must not delay a session's start, because
// the first real request will report the failure anyway.
const probeTimeout = 10 * time.Second

// Params is one model's resolved parameters: the models entry, falling back
// to the provider block, falling back to the top-level keep_alive.
type Params struct {
	Window    int
	KeepAlive time.Duration
	Options   map[string]any
}

// Loader resolves and applies a model's parameters for one provider.
type Loader struct {
	prov    provider.Provider
	cfg     *config.Config
	approve tools.ApproveFunc // nil means non-interactive: a refusal
	notice  func(string)

	// approverFn, when set (SetApprover), supplies the approver at the moment
	// consent is needed rather than at construction. The UI wires
	// Registry.Approve after the agent is built, so a loader constructed
	// during startup would otherwise be stuck with the nil it saw then — and,
	// worse, could not tell "there is nobody to ask yet" from "the user said
	// no", which is the difference between a question deferred and a question
	// answered. Returning nil from it is a refusal, exactly as a nil approve
	// is. Guarded by mu: it is written from a UI goroutine and read from
	// whichever goroutine needs consent.
	approverFn func() tools.ApproveFunc

	mu     sync.Mutex
	agreed map[string]bool // models the user has already answered for
	asked  map[string]bool // models already put to the user this session
	noted  map[string]bool // one notice per model, not one per request
	// inflight names the models with a consent ask in progress, each with a
	// channel closed when it is answered. The decision maps alone are not
	// enough: consent takes as long as a person takes to read it, and two
	// callers that both find "not yet asked" would raise two modals and then
	// race to write two answers, with the wire ending up on whichever landed
	// last. Waiters block on the channel instead, and so read the answer that
	// was actually given.
	inflight map[string]chan struct{}
}

// New builds a loader for one provider. approve may be nil (a refusal);
// notice may be nil (silence).
func New(p provider.Provider, cfg *config.Config, approve tools.ApproveFunc, notice func(string)) *Loader {
	return &Loader{
		prov: p, cfg: cfg, approve: approve, notice: notice,
		agreed: map[string]bool{}, asked: map[string]bool{}, noted: map[string]bool{},
		inflight: map[string]chan struct{}{},
	}
}

// SetApprover replaces the lazy approver source. The UI wires its approver
// from its own goroutine well after the loader was built, so this is a write
// to a field the loader's own goroutine reads: it takes the lock rather than
// leaving a data race for whoever wires it next.
func (l *Loader) SetApprover(f func() tools.ApproveFunc) {
	l.mu.Lock()
	l.approverFn = f
	l.mu.Unlock()
}

// Params resolves one model's parameters. Parameters belong to the model, so
// a models entry wins over the provider block key by key; the passthrough
// options maps are merged rather than replaced, so a model can override one
// key without restating the endpoint's others.
func (l *Loader) Params(model string) Params {
	var p Params
	if l.cfg == nil {
		return p
	}
	if pc, ok := l.cfg.Providers[l.providerName()]; ok {
		p.Window = pc.ContextWindow
		p.KeepAlive = parseDuration(pc.KeepAlive)
		p.Options = copyOptions(pc.Options)
	}
	if mc, ok := l.cfg.Models[model]; ok {
		if mc.ContextWindow > 0 {
			p.Window = mc.ContextWindow
		}
		if d := parseDuration(mc.KeepAlive); d != 0 {
			p.KeepAlive = d
		}
		if len(mc.Options) > 0 {
			if p.Options == nil {
				p.Options = map[string]any{}
			}
			for k, v := range mc.Options {
				p.Options[k] = v
			}
		}
	}
	if p.KeepAlive == 0 {
		p.KeepAlive = parseDuration(l.cfg.KeepAlive)
	}
	return p
}

// Apply resolves the model's parameters and makes them true on the server,
// asking first whenever that would change what another application on the
// box is using. It returns the window the session should budget against; 0
// means unknown (a non-Ollama backend, or one that could not be reached).
func (l *Loader) Apply(ctx context.Context, model string) (int, error) {
	p := l.Params(model)
	o, ok := l.prov.(*provider.Ollama)
	if !ok {
		return 0, nil
	}
	l.applyKeepAlive(o, p)

	// An explicit window means no probe: the user told us the answer, and
	// probing can cost minutes when it has to load the model to find out.
	// Residency is still read, but only from /api/ps — that is a question
	// about who else is using the model, not about what window to pick.
	if p.Window > 0 {
		pctx, cancel := context.WithTimeout(ctx, probeTimeout)
		serverWindow, resident, err := o.Resident(pctx, model)
		cancel()
		switch {
		case err != nil:
			return l.unknownResidency(o, p, model, err)
		case !resident:
			// Nothing is holding the model, so loading it at our window
			// evicts nobody. No consent needed.
			l.setWindow(o, p, p.Window)
			return p.Window, nil
		case serverWindow == p.Window:
			l.setWindow(o, p, p.Window)
			return p.Window, nil
		case serverWindow <= 0:
			return l.residentUnreportedWindow(o, p, model)
		default:
			return l.reconcile(ctx, model, serverWindow, p)
		}
	}

	// No configured window: read what the server has and fit ourselves to it.
	//
	// Residency is asked first, and for the same reason it is asked above.
	// ContextLength falls back to the Modelfile's num_ctx, and a Modelfile
	// describes the load that *would* happen, not the one that already has:
	// putting 4096 on the wire for a model somebody else loaded at 32768
	// reloads and evicts it. With no configured window there is not even a
	// preference to put to the user, so the only correct move is to leave
	// the running model alone.
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	switch serverWindow, resident, err := o.Resident(pctx, model); {
	case err != nil:
		return l.unknownResidency(o, p, model, err)
	case resident && serverWindow <= 0:
		return l.residentUnreportedWindow(o, p, model)
	case resident:
		l.setWindow(o, p, serverWindow)
		return serverWindow, nil
	}

	// Not resident: nothing is holding the model, so the Modelfile describes
	// the load that is about to happen and is safe to adopt.
	n, err := o.ContextLength(pctx, model)
	if err != nil || n == 0 {
		// The window stays the server's business, but the passthrough
		// options are ours either way and must not be lost with it.
		o.SetOptions(provider.Options{Extra: p.Options})
		return 0, err
	}
	l.setWindow(o, p, n)
	return n, nil
}

// unknownResidency handles a backend that would not say what it has loaded:
// unreachable, behind a proxy answering 502, or simply slower than the probe
// timeout. That is an unknown, and an unknown is not a licence. Putting a
// num_ctx on the wire here would reload — and evict — a model another
// application may be holding, with nobody asked, which is the one thing this
// package exists to prevent. So: no window, a notice, and nothing latched,
// because nothing was decided.
func (l *Loader) unknownResidency(o *provider.Ollama, p Params, model string, err error) (int, error) {
	o.SetOptions(provider.Options{Extra: p.Options})
	l.noticeOnce(model, fmt.Sprintf(
		"could not read the backend's loaded models (%s), so it is unknown whether %s is held at another window; "+
			"sending no context window this session rather than risking a reload of somebody else's model",
		compactErr(err), model))
	return 0, nil
}

// residentUnreportedWindow handles a model that is loaded while /api/ps
// declines to say at what size (an older server). We cannot tell whether our
// num_ctx would reload it, and guessing wrong evicts somebody, so we send no
// window at all and run inside whatever it has.
func (l *Loader) residentUnreportedWindow(o *provider.Ollama, p Params, model string) (int, error) {
	o.SetOptions(provider.Options{Extra: p.Options})
	l.noticeOnce(model, fmt.Sprintf(
		"model %s is loaded but the server does not report the window it was loaded with; "+
			"running inside it rather than risking a reload", model))
	return 0, nil
}

// reconcile is where consent lives: the model is resident with a window that
// is not the configured one, and changing it means reloading somebody else's
// model out from under them.
func (l *Loader) reconcile(ctx context.Context, model string, serverWindow int, p Params) (int, error) {
	o := l.prov.(*provider.Ollama)
	// Keeping the server's window means sending it, not sending nothing:
	// our own requests must name the window the model already has, or the
	// very next one reloads it behind the user's back.
	keep := func() (int, error) {
		l.setWindow(o, p, serverWindow)
		return serverWindow, nil
	}
	take := func() (int, error) {
		l.setWindow(o, p, p.Window)
		return p.Window, nil
	}

	switch l.mode() {
	case "never":
		l.noticeOnce(model, fmt.Sprintf(
			"model %s is loaded with a %d-token window; config asks for %d, and reload_on_mismatch is \"never\", so this session runs inside %d",
			model, serverWindow, p.Window, serverWindow))
		return keep()
	case "always":
		return take()
	}

	// "ask", the default. One question per model per session, whichever way
	// it was answered: a person who has said no is not asked again every
	// time the loader runs.
	//
	// One caller claims the ask and the rest wait for its answer, so
	// concurrent callers — a startup Apply, a model switch, a recovery after
	// the backend-status check trips — raise one modal between them and every
	// one of them returns the answer that actually reached the wire.
	//
	// The wait is a channel rather than a mutex, and it is selected against
	// ctx, deliberately. An ask takes as long as a person takes to read it,
	// so a caller parked behind one must remain cancellable: a wait that
	// could not be abandoned would turn "somebody is being asked" into a hang
	// with no way out, which is worse than running inside the server's
	// window. A caller whose context ends while waiting simply keeps what the
	// server has and latches nothing.
	//
	// The one shape this cannot rescue is an approver that calls Apply for
	// the *same* model on its own goroutine, which would wait on an answer
	// only it can give. Nothing does: an approver renders a prompt and
	// returns a bool, and in the TUI the goroutine that answers is Bubble
	// Tea's, never the asking one (`Session.Ask` blocks the asker precisely
	// so). Treat it as forbidden — an approver must not call back into the
	// loader — and note that even then a cancellable context turns it into a
	// bounded wait rather than a dead session.
	for {
		l.mu.Lock()
		if ch, busy := l.inflight[model]; busy {
			l.mu.Unlock()
			select {
			case <-ch:
				continue // answered by whoever claimed it; re-read the answer
			case <-ctx.Done():
				return keep()
			}
		}
		answered, yes := l.asked[model], l.agreed[model]
		if answered {
			l.mu.Unlock()
			if yes {
				return take()
			}
			return keep()
		}
		// Claim the ask. Every path out of here below must release it, so
		// the release is deferred to the end of reconcile rather than
		// written at each return.
		done := make(chan struct{})
		l.inflight[model] = done
		l.mu.Unlock()
		defer func() {
			l.mu.Lock()
			delete(l.inflight, model)
			l.mu.Unlock()
			close(done)
		}()
		break
	}

	approve := l.approver()
	if approve == nil {
		// Nobody to ask: a headless run, a scripted run, or a session whose
		// UI has not wired an approver yet. That is a refusal.
		l.noticeOnce(model, fmt.Sprintf(
			"model %s is loaded with a %d-token window; config asks for %d, but there is nobody to ask "+
				"(a headless run, or a session that has not opened its UI yet), so this session runs inside %d. "+
				"Set reload_on_mismatch: \"always\" if this server is yours to reshape.",
			model, serverWindow, p.Window, serverWindow))
		return keep()
	}

	detail := fmt.Sprintf(
		"model %s is loaded with a %d-token window; config asks for %d.\n"+
			"Reloading evicts anything else on this server using that model.",
		model, serverWindow, p.Window)
	ok := approve("model_reload", detail)
	l.mu.Lock()
	l.asked[model] = true
	l.agreed[model] = ok
	l.mu.Unlock()
	if !ok {
		l.noticeOnce(model, fmt.Sprintf(
			"keeping model %s at its loaded %d-token window; this session's budget is clamped to it",
			model, serverWindow))
		return keep()
	}
	return take()
}

// OnEvicted is the recovery path for a model that is no longer resident
// (another model pushed it out, or its keep-alive expired). The next request
// reloads it whatever we do, so the reload may as well carry the parameters
// we resolved. Nothing is holding the model, so no other application loses
// anything and nothing is asked.
func (l *Loader) OnEvicted(ctx context.Context, model string) {
	o, ok := l.prov.(*provider.Ollama)
	if !ok {
		return
	}
	p := l.Params(model)
	l.applyKeepAlive(o, p)
	if p.Window > 0 {
		l.setWindow(o, p, p.Window)
	}
}

// OnWindowChanged is the other half of the backend-status check: another
// client reloaded the model at a different size. The harness adapts — our
// own requests now name their window, so we neither truncate nor reload it
// back. A reload war between two clients on a shared server is the worst
// outcome available, so this never asks and never insists; the one exception
// is standing consent (reload_on_mismatch: always), where the user has
// already said this server is theirs to reshape.
func (l *Loader) OnWindowChanged(model string, window int) {
	o, ok := l.prov.(*provider.Ollama)
	if !ok || window <= 0 {
		return
	}
	p := l.Params(model)
	if p.Window > 0 && p.Window != window && l.mode() == "always" {
		l.setWindow(o, p, p.Window)
		return
	}
	l.setWindow(o, p, window)
}

// ---- helpers ---------------------------------------------------------------

// mode normalizes reload_on_mismatch. An unrecognized value is treated as
// "ask" — the safe reading of a typo — with one warning.
func (l *Loader) mode() string {
	if l.cfg == nil {
		return "ask"
	}
	switch m := strings.ToLower(strings.TrimSpace(l.cfg.ReloadOnMismatch)); m {
	case "always", "never", "ask":
		return m
	case "":
		return "ask"
	default:
		l.noticeOnce("\x00mode", fmt.Sprintf(
			"reload_on_mismatch %q is not one of ask|always|never; using ask", l.cfg.ReloadOnMismatch))
		return "ask"
	}
}

// approver resolves who to ask right now. Nil means nobody, which is a
// refusal, never a silent yes.
func (l *Loader) approver() tools.ApproveFunc {
	l.mu.Lock()
	fn, fixed := l.approverFn, l.approve
	l.mu.Unlock()
	if fn != nil {
		return fn()
	}
	return fixed
}

func (l *Loader) providerName() string {
	if l.prov == nil {
		return ""
	}
	return l.prov.Name()
}

// setWindow puts one resolved window and the passthrough options on the
// provider. Every path that touches the wire goes through here, so there is
// exactly one place num_ctx is set.
func (l *Loader) setWindow(o *provider.Ollama, p Params, window int) {
	o.SetOptions(provider.Options{NumCtx: window, Extra: p.Options})
}

func (l *Loader) applyKeepAlive(o *provider.Ollama, p Params) {
	if p.KeepAlive > 0 {
		o.SetKeepAlive(p.KeepAlive.String())
	}
}

func (l *Loader) noticeOnce(key, msg string) {
	if l.notice == nil {
		return
	}
	l.mu.Lock()
	seen := l.noted[key]
	l.noted[key] = true
	l.mu.Unlock()
	if !seen {
		l.notice(msg)
	}
}

// parseDuration reads a Go duration string, returning 0 for anything it
// cannot make sense of (including "0", which means "do not keep it resident"
// and is left to the caller's own default rather than forced here).
func parseDuration(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// compactErr keeps a backend failure to one readable line inside a notice.
func compactErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 120 {
		s = s[:120] + "..."
	}
	return s
}

func copyOptions(m map[string]any) map[string]any {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
