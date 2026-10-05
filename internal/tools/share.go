package tools

import (
	"context"
	"fmt"
	"sync"

	"github.com/brown-enterprises/be-code/internal/browser"
)

// ShareGate is per-site consent for page text while the main model is
// online (online spec §2.2): before what the agent reads on a site — page
// text, page notes, tab titles, a fetched page, search results — reaches
// Provider, the person is asked "Send what the agent reads on <host> to
// <Provider>?". A yes covers the host for the session, except in the
// person's own Chrome, where every page asks. cmd sets it whenever the main
// provider is online and clears it (nil) whenever it is not; with no gate
// nothing asks and nothing changes (a local main model).
type ShareGate struct {
	// Provider is the name the person recognises the online provider by.
	Provider string
	// MyChrome, when set, reports that the browser is in the person's own
	// Chrome, where every page asks. The browser tool also knows this of
	// its own session; either one makes a browser page ask every time.
	MyChrome func() bool
}

// webSearchShareKey is the one session grant web_search results share.
const webSearchShareKey = "(web search results)"

// shareState is the gate and its session grants. It lives on the primary
// registry only: a Subset or Scoped registry reads its parent's (shareRoot).
type shareState struct {
	mu      sync.Mutex
	gate    *ShareGate
	granted map[string]bool
}

// shareRoot is the registry the gate is read from: the primary one, whose
// gate and grants a Subset or Scoped registry shares through parent.
func (r *Registry) shareRoot() *Registry {
	for r.parent != nil {
		r = r.parent
	}
	return r
}

// SetShareGate sets (or, with nil, clears) the gate. Grants were given for
// one provider: a gate naming another one, or none, starts afresh.
func (r *Registry) SetShareGate(g *ShareGate) {
	root := r.shareRoot()
	root.share.mu.Lock()
	defer root.share.mu.Unlock()
	if g == nil || root.share.gate == nil || root.share.gate.Provider != g.Provider {
		root.share.granted = nil
	}
	if g != nil {
		cp := *g
		g = &cp
	}
	root.share.gate = g
}

// shareGate is the gate in force, or nil for a local main model.
func (r *Registry) shareGate() *ShareGate {
	root := r.shareRoot()
	root.share.mu.Lock()
	defer root.share.mu.Unlock()
	return root.share.gate
}

// shareGranted reports whether what the agent reads on host may reach the
// model without asking: always with no gate; never during a fired turn (a
// session grant was given by someone watching); otherwise when the host was
// shared earlier this session. It never asks.
func (r *Registry) shareGranted(host string) bool {
	root := r.shareRoot()
	root.share.mu.Lock()
	gate, ok := root.share.gate, root.share.granted[browser.NormalizeHost(host)]
	root.share.mu.Unlock()
	if gate == nil {
		return true
	}
	return ok && host != "" && !root.Fired()
}

// shareOK says whether what the agent reads on host may be sent to the
// online main model. No gate → yes (a local main model). The caller has
// already let the browser's allow tier through. A host shared earlier this
// session → yes, unless everyTime (the person's own Chrome, a page with no
// address) or a fired turn, which takes no session shortcut. Otherwise it
// asks share_page; a yes is kept for the session only outside those two
// cases. An ask withdrawn or cancelled is a no.
func (r *Registry) shareOK(ctx context.Context, host string, everyTime bool) bool {
	disp := host
	if host == "" {
		disp = "a page with no address"
		everyTime = true
	}
	return r.shareAsk(ctx, browser.NormalizeHost(host), everyTime, func(provider string) string {
		return fmt.Sprintf("Send what the agent reads on %s to %s?", disp, provider)
	})
}

// shareAsk is shareOK over any grant key and question.
func (r *Registry) shareAsk(ctx context.Context, key string, everyTime bool, question func(provider string) string) bool {
	root := r.shareRoot()
	root.share.mu.Lock()
	gate := root.share.gate
	granted := root.share.granted[key]
	root.share.mu.Unlock()
	if gate == nil {
		return true
	}
	fired := root.Fired()
	if granted && !everyTime && !fired {
		return true
	}
	ok := root.AskPerson(ctx, "share_page", question(gate.Provider)) && ctx.Err() == nil
	if ok && !everyTime && !fired {
		root.share.mu.Lock()
		// Kept only if the gate is still the one asked about: a switch away
		// from this provider while the question was open grants nothing.
		if root.share.gate != nil && root.share.gate.Provider == gate.Provider {
			if root.share.granted == nil {
				root.share.granted = map[string]bool{}
			}
			root.share.granted[key] = true
		}
		root.share.mu.Unlock()
	}
	return ok
}

// ShareProvider names the provider the share gate asks about ("" with no
// gate: a local main model).
func (r *Registry) ShareProvider() string {
	if g := r.shareGate(); g != nil {
		return g.Provider
	}
	return ""
}
