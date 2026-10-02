package tools

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/brown-enterprises/be-code/internal/config"
	"github.com/brown-enterprises/be-code/internal/schedule"
)

// firedPolicy is a scheduled event's allowance for the turn it runs in
// (schedules spec §2.3). It lives on the primary registry only: Subset and
// Scoped build fresh registries, and a scoped registry's Approve is a
// closure over the parent's seam, so neither ever sees it. That is why the
// allowance is consulted inside the tools rather than by wrapping Approve.
type firedPolicy struct {
	allow      schedule.Allowance
	askTimeout time.Duration

	mu       sync.Mutex
	refused  string
	timedOut bool
}

// SetAllowance starts a fired turn's allowance; askTimeout bounds every
// prompt raised during it (0: no bound).
func (r *Registry) SetAllowance(al schedule.Allowance, askTimeout time.Duration) {
	r.fired.Store(&firedPolicy{allow: al, askTimeout: askTimeout})
}

// ClearAllowance ends it and reports the first action refused during the
// turn, and whether that refusal was a prompt nobody answered in time.
func (r *Registry) ClearAllowance() (refused string, timedOut bool) {
	p := r.fired.Swap(nil)
	if p == nil {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.refused, p.timedOut
}

// Fired reports whether a scheduled event's turn is running on this
// registry (SetAllowance until ClearAllowance). While it is, nothing but
// the event's own allowance and the standing config (shell_allow, the
// browser's allow tier) lets an action through unasked: the session-level
// shortcuts a person gave while watching — "a", -y, accept-all — do not
// apply to a turn nobody is watching (final review C1).
func (r *Registry) Fired() bool { return r.fired.Load() != nil }

type firedAskKey struct{}

// FiredAsk reports whether ctx belongs to a question a fired turn raised.
// The UIs' approvers consult it to skip their session-level shortcuts
// (AutoApproveShell, !ApproveFileWrites, AutoApproveBrowser) for exactly
// those questions: a sub-agent's or a person's own question asked at the
// same time is not marked and keeps them.
func FiredAsk(ctx context.Context) bool {
	v, _ := ctx.Value(firedAskKey{}).(bool)
	return v
}

// AskOutcome lets an asker that must tell "answered no" from "nobody
// answered" (a prompt withdrawn because the session quit, or its context
// ended) learn which it was: a bool approval cannot say. The UIs call
// MarkWithdrawn on the question's context when their prompt ended without
// an answer.
type AskOutcome struct{ withdrawn atomic.Bool }

type askOutcomeKey struct{}

// WithAskOutcome returns ctx carrying a fresh outcome for one question.
func WithAskOutcome(ctx context.Context) (context.Context, *AskOutcome) {
	o := &AskOutcome{}
	return context.WithValue(ctx, askOutcomeKey{}, o), o
}

// MarkWithdrawn records, when ctx carries an outcome, that the question
// ended without anybody answering it.
func MarkWithdrawn(ctx context.Context) {
	if o, ok := ctx.Value(askOutcomeKey{}).(*AskOutcome); ok {
		o.withdrawn.Store(true)
	}
}

// Withdrawn reports whether the question ended unanswered.
func (o *AskOutcome) Withdrawn() bool { return o != nil && o.withdrawn.Load() }

func (r *Registry) allowShell(command string) bool {
	p := r.fired.Load()
	if p == nil {
		return false
	}
	globs := p.allow.Values("shell")
	return len(globs) > 0 && classifyCommand(command, globs, nil) == cmdAllowed
}

// allowWrite reports whether absPath is covered by the fired allowance.
// resolve() only checks the path textually, so a symlink already inside a
// granted directory (planted before the event fired, or by an earlier
// covered write in the same turn) could otherwise point anywhere and land an
// unattended, never-diffed write outside the grant; realExistingPath (also
// used by checkScope) resolves it, and both the resolved path's containment
// in Root and its Root-relative path's WriteAllowed must hold, or this falls
// through to the normal prompt.
func (r *Registry) allowWrite(absPath string) bool {
	p := r.fired.Load()
	if p == nil {
		return false
	}
	real, err := realExistingPath(absPath)
	if err != nil {
		return false
	}
	root := r.Root
	if rr, err := filepath.EvalSymlinks(r.Root); err == nil {
		root = rr
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	if protectedFromGrants(root, real) {
		return false
	}
	return p.allow.WriteAllowed(filepath.ToSlash(rel))
}

// protectedFromGrants is what no write grant ever covers, whatever it says
// (final review I3c): the project's schedules.md — a covered write there
// could re-activate a paused schedule or widen an allowance with nobody
// asked — and anything under the BE-Code dotdir (config, approvals,
// sessions). Such a write still happens if a person approves it.
func protectedFromGrants(root, real string) bool {
	if samePath(real, filepath.Join(root, ".be-code", "schedules.md")) {
		return true
	}
	dir, err := config.Dir()
	if err != nil {
		return true // cannot tell where it is: fail closed
	}
	if rd, err := filepath.EvalSymlinks(dir); err == nil {
		dir = rd
	}
	return samePath(real, dir) || within(dir, real)
}

// foldCase is true where the file system usually ignores case.
var foldCase = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if foldCase {
		return strings.EqualFold(a, b)
	}
	return a == b
}

func within(dir, p string) bool {
	dir, p = filepath.Clean(dir), filepath.Clean(p)
	if foldCase {
		dir, p = strings.ToLower(dir), strings.ToLower(p)
	}
	return strings.HasPrefix(p, dir+string(filepath.Separator))
}

func (r *Registry) allowHost(host string) bool {
	p := r.fired.Load()
	return p != nil && p.allow.HostAllowed(host)
}

// ask raises a prompt through the seam. nilApproves is what a registry with
// no approver means at the call site: shell and file writes have always run
// unasked without one, the browser and shell_after_web refuse. During a
// fired turn the prompt goes through ApproveCtx, marked (FiredAsk) so the
// UI's session-level shortcuts do not answer it, and under the allowance's
// deadline, so a question nobody is there to answer is withdrawn and
// refused rather than holding the event forever (spec §2.4). With nobody
// to ask at all, a fired turn's uncovered action is refused.
func (r *Registry) ask(ctx context.Context, action, detail string, nilApproves bool) bool {
	p := r.fired.Load()
	if p == nil {
		if r.Approve == nil {
			return nilApproves
		}
		return r.Approve(action, detail)
	}
	var ok, timedOut bool
	switch {
	case r.ApproveCtx != nil:
		actx := context.WithValue(ctx, firedAskKey{}, true)
		cancel := func() {}
		if p.askTimeout > 0 {
			actx, cancel = context.WithTimeout(actx, p.askTimeout)
		}
		ok = r.ApproveCtx(actx, action, detail)
		timedOut = !ok && errors.Is(actx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		cancel()
	case r.Approve != nil:
		ok = r.Approve(action, detail)
	}
	if !ok {
		p.mu.Lock()
		if p.refused == "" {
			p.refused, p.timedOut = action, timedOut
		}
		p.mu.Unlock()
	}
	return ok
}
