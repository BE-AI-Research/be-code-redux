package tools

import (
	"context"
	"errors"
	"sync"
	"time"

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

func (r *Registry) allowShell(command string) bool {
	p := r.fired.Load()
	if p == nil {
		return false
	}
	globs := p.allow.Values("shell")
	return len(globs) > 0 && classifyCommand(command, globs, nil) == cmdAllowed
}

func (r *Registry) allowWrite(rel string) bool {
	p := r.fired.Load()
	return p != nil && p.allow.WriteAllowed(rel)
}

func (r *Registry) allowHost(host string) bool {
	p := r.fired.Load()
	return p != nil && p.allow.HostAllowed(host)
}

// ask raises a prompt through the seam. nilApproves is what a registry with
// no approver means at the call site: shell and file writes have always run
// unasked without one, the browser and shell_after_web refuse. During a
// fired turn the prompt goes through ApproveCtx under the allowance's
// deadline, so a question nobody is there to answer is withdrawn and
// refused rather than holding the event forever (spec §2.4).
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
	case p.askTimeout > 0 && r.ApproveCtx != nil:
		actx, cancel := context.WithTimeout(ctx, p.askTimeout)
		ok = r.ApproveCtx(actx, action, detail)
		timedOut = !ok && errors.Is(actx.Err(), context.DeadlineExceeded) && ctx.Err() == nil
		cancel()
	case r.Approve != nil:
		ok = r.Approve(action, detail)
	default:
		ok = nilApproves
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
