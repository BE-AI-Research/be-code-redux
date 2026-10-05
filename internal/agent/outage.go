package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/brown-enterprises/be-code/internal/provider"
)

// outageOffered reports whether err is an outage worth offering the local
// helper for: a transient failure that survived the retries. A rejected key,
// any 4xx other than 408/429, a context overflow and the person's own
// cancellation never are.
func outageOffered(ctx context.Context, err error) bool {
	if err == nil || ctx.Err() != nil || errors.Is(err, context.Canceled) {
		return false
	}
	var he *provider.HTTPError
	if errors.As(err, &he) {
		switch {
		case he.Code == http.StatusRequestTimeout || he.Code == http.StatusTooManyRequests:
		case he.Code >= 500:
		default:
			return false
		}
	}
	if _, _, over := contextOverflow(err); over {
		return false
	}
	return isRetryableBackendError(err) || (he != nil)
}

// offerOutageSwitch runs when a primary call has failed for good. With an
// online main model, a reachable local helper and a transient error it asks
// switch_to_local (no "always", never answered by -y); yes makes the helper
// the main model and returns true so the caller retries through the normal
// path. A fired turn never asks. Once per outage: a "no" is remembered until
// a primary call succeeds.
func (a *Agent) offerOutageSwitch(ctx context.Context, err error) bool {
	name, online := a.Online()
	if !online || !a.helperConfigured() || !outageOffered(ctx, err) {
		return false
	}
	if a.firedTurn() {
		a.notice("%s unreachable; the scheduled event stopped", name)
		return false
	}
	a.onlineMu.Lock()
	declined := a.outageDeclined
	a.onlineMu.Unlock()
	if declined {
		return false
	}
	// Reachable means the helper builds and is not marked down; no probe.
	h := a.helperSt()
	h.mu.Lock()
	down := timeNow().Before(h.downUntil)
	h.mu.Unlock()
	if down {
		return false
	}
	if _, _, _, berr := buildHelper(ctx, a.Cfg); berr != nil {
		return false
	}
	q := fmt.Sprintf("%s is not responding. Switch this session to your local model (%s)?", name, a.Cfg.LocalHelper.Model)
	yes := a.Tools.AskPerson(ctx, "switch_to_local", q)
	if ctx.Err() != nil {
		return false
	}
	if !yes {
		a.onlineMu.Lock()
		a.outageDeclined = true
		a.onlineMu.Unlock()
		return false
	}
	if !a.switchToHelper(ctx, name+" is not responding") {
		return false
	}
	a.notice("this session is on the local model now; /provider %s switches back", name)
	return true
}

// noteOutageOver clears the once-per-outage latch after a primary call
// succeeds.
func (a *Agent) noteOutageOver() {
	a.onlineMu.Lock()
	a.outageDeclined = false
	a.onlineMu.Unlock()
}
