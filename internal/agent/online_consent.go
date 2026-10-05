package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/engine"
	"github.com/brown-enterprises/be-code/internal/provider"
	"github.com/brown-enterprises/be-code/internal/tools"
)

// Per-project consent for an online main model (spec §2.1): before the
// first request a session would send to an online provider, a person says
// whether this project may go there. The answer is kept per workspace and
// per provider name, as configured.

// OnlineResolver re-derives the online state (window, prices, key name,
// SetOnline) for the main provider and model after a switch: an online
// provider marks the session online, a local one clears it. Injected from
// cmd like ReviewerFactory — it needs the provider registry and presets.
// nil leaves the online state as it was (tests, scratch agents).
var OnlineResolver func(ctx context.Context, a *Agent, providerName, model string)

// mainModel is a provider and model the main model has run on.
type mainModel struct {
	p     provider.Provider
	model string
}

// onlineStoreMu serialises this process's read-merge-write of the store;
// another session on the same workspace is covered by the re-read.
var onlineStoreMu sync.Mutex

var onlineTmpSeq atomic.Uint64

// onlineStorePath is ~/.be-code/engine/<key>/online.json for this workspace.
func (a *Agent) onlineStorePath() (string, error) {
	base, err := config.Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "engine", engine.Key(a.Tools.Root), "online.json"), nil
}

// readOnlineStore reads the store; missing or corrupt reads as empty — a
// project nobody can show was approved is not approved, and never fatal.
func readOnlineStore(p string) map[string]bool {
	m := map[string]bool{}
	b, err := os.ReadFile(p)
	if err != nil {
		return m
	}
	if json.Unmarshal(b, &m) != nil || m == nil {
		return map[string]bool{}
	}
	return m
}

// updateOnlineStore re-reads the store, applies fn and writes it back
// atomically, 0600: sessions on one workspace share the file.
func updateOnlineStore(p string, fn func(map[string]bool)) error {
	onlineStoreMu.Lock()
	defer onlineStoreMu.Unlock()
	m := readOnlineStore(p)
	fn(m)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.tmp.%d.%d", p, os.Getpid(), onlineTmpSeq.Add(1))
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, p); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// storedApproval reports whether online.json approves name for this project.
func (a *Agent) storedApproval(name string) bool {
	p, err := a.onlineStorePath()
	if err != nil {
		return false
	}
	return readOnlineStore(p)[name]
}

// OnlineApproved reports whether the main model may send this project to
// its online provider: always true for a local main model (there is
// nothing to approve), else a yes this session, the -y run's approval or a
// remembered yes in online.json.
func (a *Agent) OnlineApproved() bool {
	name, online := a.Online()
	if !online {
		return true
	}
	a.onlineMu.Lock()
	ok := a.onlineRunOK || a.onlineOK[name]
	a.onlineMu.Unlock()
	if ok {
		return true
	}
	if a.storedApproval(name) {
		a.markApproved(name)
		return true
	}
	return false
}

func (a *Agent) markApproved(name string) {
	a.onlineMu.Lock()
	if a.onlineOK == nil {
		a.onlineOK = map[string]bool{}
	}
	a.onlineOK[name] = true
	a.onlineMu.Unlock()
}

// approveOnline records a person's yes: for the session, and in online.json
// for every later session on this workspace. A store that cannot be
// written is said once; the session's yes stands.
func (a *Agent) approveOnline(name string) {
	a.markApproved(name)
	p, err := a.onlineStorePath()
	if err == nil {
		err = updateOnlineStore(p, func(m map[string]bool) { m[name] = true })
	}
	if err != nil {
		a.notice("could not remember the approval for %s (%v); the next session asks again", name, err)
	}
}

// ApproveOnlineForRun is headless -y: this run may use the online provider,
// and nothing is remembered.
func (a *Agent) ApproveOnlineForRun() {
	a.onlineMu.Lock()
	a.onlineRunOK = true
	a.onlineMu.Unlock()
}

// ForgetOnline removes the current online provider's remembered approval
// for this project: the next session asks again. This session keeps the
// yes it already has — taking it back mid-session would strand the
// conversation behind a question nobody is asked.
func (a *Agent) ForgetOnline() error {
	name, online := a.Online()
	if !online {
		return errors.New("the main model is not online; there is no approval to forget")
	}
	p, err := a.onlineStorePath()
	if err != nil {
		return err
	}
	return updateOnlineStore(p, func(m map[string]bool) { delete(m, name) })
}

// onlineQuestion is the online_project prompt, verbatim from the spec.
func (a *Agent) onlineQuestion(name string) string {
	a.modelMu.Lock()
	model := a.Model
	a.modelMu.Unlock()
	return fmt.Sprintf("This project's files, command output and conversation will be sent to %s (%s). Allow for this project?", name, model)
}

func gateWaitingError(name string) error {
	return fmt.Errorf("waiting for your answer about sending this project to %s", name)
}

func notApprovedReason(name string) string {
	return "this project is not approved for " + name
}

// OnlineGatePending reports whether the startup question is on screen.
func (a *Agent) OnlineGatePending() bool {
	a.onlineMu.Lock()
	defer a.onlineMu.Unlock()
	return a.gatePending
}

// OnlineRefusal is the notice the gate ended the session with ("" when it
// did not), so a UI that quits on it can repeat it after the screen goes.
func (a *Agent) OnlineRefusal() string {
	a.onlineMu.Lock()
	defer a.onlineMu.Unlock()
	return a.gateRefusal
}

// StartOnlineGate is the startup question (spec §2.1). It returns true at
// once when the main model is local or already approved. Otherwise it asks
// online_project: yes is remembered; no moves the session's main model to
// local_helper (true) or, with no helper, refuses with a notice (false);
// a question withdrawn — the session quit, the input ended — stores
// nothing and returns false. UIs call it once their approvals are wired
// and before sub-agents and schedules start.
func (a *Agent) StartOnlineGate() bool {
	name, online := a.Online()
	if !online || a.OnlineApproved() {
		a.noteGood()
		return true
	}
	a.onlineMu.Lock()
	a.gatePending = true
	a.onlineMu.Unlock()
	defer func() {
		a.onlineMu.Lock()
		a.gatePending = false
		a.onlineMu.Unlock()
	}()
	if a.Tools.Approve == nil && a.Tools.ApproveCtx == nil {
		// Nobody to ask is not a "no": nothing stored, no switch to the
		// helper, as for a question withdrawn.
		return false
	}
	ctx, out := tools.WithAskOutcome(context.Background())
	yes := a.Tools.AskPerson(ctx, "online_project", a.onlineQuestion(name))
	if yes {
		a.approveOnline(name)
		a.noteGood()
		return true
	}
	if out.Withdrawn() {
		return false
	}
	if a.switchToHelper(context.Background(), "you declined sending this project to "+name) {
		return true
	}
	msg := fmt.Sprintf("this project is not approved for %s and no local_helper is configured; nothing was sent", name)
	a.onlineMu.Lock()
	a.gateRefusal = msg
	a.onlineMu.Unlock()
	a.notice("%s", msg)
	return false
}

// StartOnlineGateAsync runs StartOnlineGate on a goroutine of its own for a
// UI that must keep rendering while the question waits, then calls done.
// A session that needs no question calls done(true) on the caller's own
// goroutine, so a local session starts exactly as it always did.
func (a *Agent) StartOnlineGateAsync(done func(ok bool)) {
	if _, online := a.Online(); !online || a.OnlineApproved() {
		a.noteGood()
		if done != nil {
			done(true)
		}
		return
	}
	a.onlineMu.Lock()
	a.gatePending = true // before the goroutine runs: nothing may slip past
	a.onlineMu.Unlock()
	go func() {
		ok := false
		defer func() {
			if r := recover(); r != nil {
				a.notice("the online consent question failed (%v); nothing was sent", r)
			}
			a.onlineMu.Lock()
			a.gatePending = false
			a.onlineMu.Unlock()
			if done != nil {
				done(ok)
			}
		}()
		ok = a.StartOnlineGate()
	}()
}

// noteGood remembers the provider and model a call may go out on now: the
// place a declined switch goes back to.
func (a *Agent) noteGood() {
	a.modelMu.Lock()
	g := mainModel{p: a.Provider, model: a.Model}
	a.modelMu.Unlock()
	a.onlineMu.Lock()
	a.lastGood = g
	a.onlineMu.Unlock()
}

// checkOnlineGate runs ahead of every primary model call, next to the spend
// cap: nothing reaches an online model before this project is approved for
// it. Local sessions pass untouched. ask is true for the tool loop, where
// the person who switched to an unapproved provider is asked inline;
// chores (compaction, the handoff) never ask — one may run at exit, with
// nobody left to answer — and get the backstop error.
func (a *Agent) checkOnlineGate(ctx context.Context, ask bool) error {
	if a.spendParent != nil {
		// Plan mode's scratch agent runs on the primary's model.
		return a.spendParent.checkOnlineGate(ctx, ask)
	}
	a.onlineMu.Lock()
	unresolved := a.providerUnresolved
	a.onlineMu.Unlock()
	if unresolved {
		// SetProvider without its paired model switch yet: whether the new
		// provider is online is not known, so nothing goes out.
		return errors.New("the provider switch has not finished; nothing was sent")
	}
	name, online := a.Online()
	if !online || a.OnlineApproved() {
		a.noteGood()
		return nil
	}
	if a.Tools != nil && a.Tools.Fired() {
		// A scheduled event never raises this question.
		return errors.New(notApprovedReason(name))
	}
	a.onlineMu.Lock()
	inline, pending := a.gateInline, a.gatePending
	a.onlineMu.Unlock()
	if !ask || !inline || pending {
		return gateWaitingError(name)
	}
	actx, out := tools.WithAskOutcome(ctx)
	if a.Tools.AskPerson(actx, "online_project", a.onlineQuestion(name)) {
		a.approveOnline(name)
		a.onlineMu.Lock()
		a.gateInline = false
		a.onlineMu.Unlock()
		a.noteGood()
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if out.Withdrawn() {
		return gateWaitingError(name)
	}
	a.declineSwitch(ctx, name)
	return fmt.Errorf("not sent: %s", notApprovedReason(name))
}

// declineSwitch takes the main model back from a provider the person has
// just declined: to the last one a call went out on, else the helper.
func (a *Agent) declineSwitch(ctx context.Context, name string) {
	a.onlineMu.Lock()
	back := a.lastGood
	a.gateInline = false
	a.onlineMu.Unlock()
	reason := "you declined sending this project to " + name
	if back.p != nil && back.model != "" {
		a.restoreMain(ctx, back.p, back.model)
		if _, online := a.Online(); !online || a.OnlineApproved() {
			a.notice("%s; back to %s (%s)", reason, back.model, back.p.Name())
			return
		}
	}
	if a.switchToHelper(ctx, reason) {
		return
	}
	a.notice("this project is not approved for %s and no local_helper is configured; nothing was sent", name)
}

// restoreMain makes p and model the main model again, through the same
// switch path a person's /provider and /model take.
func (a *Agent) restoreMain(ctx context.Context, p provider.Provider, model string) {
	if a.CurrentProviderClient() != p {
		a.SetProvider(p)
	}
	a.SetModelNow(ctx, model)
}

// switchToHelper makes local_helper the session's main model (spec §2.1:
// "no" at the gate), with a notice. false when there is no helper to
// switch to.
func (a *Agent) switchToHelper(ctx context.Context, reason string) bool {
	if a.Cfg == nil || a.Cfg.LocalHelper.Model == "" || HelperFactory == nil {
		return false
	}
	p, model, _, err := buildHelper(ctx, a.Cfg)
	if err != nil {
		a.notice("local helper unavailable: %v", err)
		return false
	}
	if model == "" {
		model = a.Cfg.LocalHelper.Model
	}
	a.restoreMain(ctx, p, model)
	if OnlineResolver == nil {
		a.SetOnline("", "", Pricing{})
		a.SetKeyEnv("")
	}
	a.noteGood()
	a.notice("%s; the main model is now the local helper %s", reason, model)
	return true
}

// resolveOnline runs after every switch of the main provider or model: the
// online state follows the provider, and a switch onto a provider this
// project is not approved for is asked about on the next call.
func (a *Agent) resolveOnline(ctx context.Context, model string) {
	if OnlineResolver == nil {
		return
	}
	OnlineResolver(ctx, a, a.CurrentProvider(), model)
	a.onlineMu.Lock()
	a.providerUnresolved = false
	a.onlineMu.Unlock()
	inline := false
	if _, online := a.Online(); online && !a.OnlineApproved() {
		inline = true
	}
	a.onlineMu.Lock()
	a.gateInline = inline
	a.onlineMu.Unlock()
}

// OnlineReport is /online in both UIs: provider, model, approval, spend.
func (a *Agent) OnlineReport() string {
	a.modelMu.Lock()
	model := a.Model
	a.modelMu.Unlock()
	name, online := a.Online()
	if !online {
		return fmt.Sprintf("the main model is local (%s); nothing is sent online", model)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "online: %s · %s\n", name, model)
	a.onlineMu.Lock()
	runOK, sessOK := a.onlineRunOK, a.onlineOK[name]
	a.onlineMu.Unlock()
	switch {
	case a.storedApproval(name):
		b.WriteString("approved for this project (remembered; /online forget asks again next session)\n")
	case runOK:
		b.WriteString("approved for this run only\n")
	case sessOK:
		b.WriteString("approved for this session (not remembered)\n")
	default:
		b.WriteString("not approved for this project\n")
	}
	spent := a.Usage().SpendUSD
	switch p := a.Pricing(); {
	case !p.Known:
		b.WriteString("spend: " + strings.TrimPrefix(UnpricedNote(a.Cfg.MaxSpendUSD), "spend is "))
	case a.SpendCap() > 0:
		fmt.Fprintf(&b, "spend: $%.2f (est.) of a $%.2f cap", spent, a.SpendCap())
	default:
		fmt.Fprintf(&b, "spend: $%.2f (est.)", spent)
	}
	return b.String()
}

// CurrentModel is the main model's name, read under the lock a switch
// writes it under (for goroutines other than the agent's own).
func (a *Agent) CurrentModel() string {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	return a.Model
}

// CurrentProvider is the configured name of the provider the main model is
// on now ("" when none). UIs read it instead of their own copy: a declined
// switch or the helper fallback changes it from the agent's side.
func (a *Agent) CurrentProvider() string {
	if p := a.CurrentProviderClient(); p != nil {
		return p.Name()
	}
	return ""
}

// CurrentProviderClient is the main provider itself, for a UI's listing or
// ping, read under the lock SetProvider writes it under.
func (a *Agent) CurrentProviderClient() provider.Provider {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	return a.Provider
}

// CurrentFamily is the main model's profile family, read under its lock.
func (a *Agent) CurrentFamily() string {
	a.modelMu.Lock()
	defer a.modelMu.Unlock()
	return a.Profile.Family
}
