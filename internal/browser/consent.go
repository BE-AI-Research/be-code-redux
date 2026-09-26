package browser

import (
	"fmt"
	"net"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
)

// Tier is how interactions on a host are consented to (spec §3.1).
type Tier int

const (
	// TierAsk is the default: ask once per host, then allow it for the
	// rest of the session.
	TierAsk Tier = iota
	TierAllow
	TierWatch
	TierDeny
)

func (t Tier) String() string {
	switch t {
	case TierAllow:
		return "allow"
	case TierWatch:
		return "watch"
	case TierDeny:
		return "deny"
	}
	return "ask"
}

// strictness ranks tiers from strictest to most permissive, for deterministic
// tie-breaking when equal patterns are configured with different tiers.
func (t Tier) strictness() int {
	switch t {
	case TierDeny:
		return 4
	case TierWatch:
		return 3
	case TierAsk:
		return 2
	case TierAllow:
		return 1
	}
	return 0
}

// ParseTier reads a browser.sites value.
func ParseTier(s string) (Tier, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "allow":
		return TierAllow, true
	case "watch":
		return TierWatch, true
	case "deny":
		return TierDeny, true
	}
	return TierAsk, false
}

type siteRule struct {
	pattern string
	tier    Tier
	wild    int
}

// Consent holds the configured tiers (most specific rule first) and the
// hosts granted this session.
type Consent struct {
	rules []siteRule

	mu      sync.Mutex
	granted map[string]bool
}

// NewConsent builds the tiers from browser.sites. An unknown tier value is
// dropped with a warning (the host falls back to the default tier).
func NewConsent(sites map[string]string) (*Consent, []string) {
	c := &Consent{granted: map[string]bool{}}
	var warns []string
	for pat, v := range sites {
		tier, ok := ParseTier(v)
		if !ok {
			warns = append(warns, fmt.Sprintf("browser.sites[%q]: %q is not one of allow|watch|deny; ignored", pat, v))
			continue
		}
		p := NormalizeHost(pat)
		if p == "" {
			continue
		}
		c.rules = append(c.rules, siteRule{pattern: p, tier: tier, wild: strings.Count(p, "*") + strings.Count(p, "?")})
	}
	// Most specific first: an exact host before any glob, fewer wildcards
	// before more, a longer pattern before a shorter, stricter tiers before
	// permissive ones — so the JSON's own (unordered) key order never decides.
	sort.Slice(c.rules, func(i, j int) bool {
		a, b := c.rules[i], c.rules[j]
		if a.wild != b.wild {
			return a.wild < b.wild
		}
		if len(a.pattern) != len(b.pattern) {
			return len(a.pattern) > len(b.pattern)
		}
		if a.pattern != b.pattern {
			return a.pattern < b.pattern
		}
		// Equal patterns: stricter tier comes first
		return a.tier.strictness() > b.tier.strictness()
	})
	sort.Strings(warns)
	return c, warns
}

// Tier returns the tier for a host. A page with no host (about:blank, data:,
// anything unparsable) is asked about like any unknown site, because a page
// can open one itself and write into it. With no matching rule, loopback is
// allowed and everything else asks.
func (c *Consent) Tier(host string) Tier {
	h := NormalizeHost(host)
	for _, r := range c.rules {
		if ok, _ := path.Match(r.pattern, h); ok {
			return r.tier
		}
	}
	if IsLoopbackHost(h) {
		return TierAllow
	}
	return TierAsk
}

// Granted reports whether the host was allowed earlier this session.
func (c *Consent) Granted(host string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.granted[NormalizeHost(host)]
}

// Grant allows the host for the rest of the session.
func (c *Consent) Grant(host string) {
	c.mu.Lock()
	c.granted[NormalizeHost(host)] = true
	c.mu.Unlock()
}

// Forget revokes a session grant, reporting whether there was one.
func (c *Consent) Forget(host string) bool {
	h := NormalizeHost(host)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.granted[h] {
		return false
	}
	delete(c.granted, h)
	return true
}

// Grants lists the hosts allowed this session, sorted.
func (c *Consent) Grants() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.granted))
	for h := range c.granted {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// NormalizeHost lowercases a host and strips a port, brackets and a
// trailing dot, so "GitHub.com:443" and "github.com." are one site.
func NormalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if host, _, err := net.SplitHostPort(h); err == nil {
		h = host
	}
	h = strings.Trim(h, "[]")
	return strings.TrimSuffix(h, ".")
}

// HostOf is the normalised host of a page URL; "" for about:blank and
// anything unparsable.
func HostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return NormalizeHost(u.Host)
}

// IsLoopbackHost reports localhost, *.localhost and loopback addresses.
func IsLoopbackHost(host string) bool {
	h := NormalizeHost(host)
	if h == "localhost" || strings.HasSuffix(h, ".localhost") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
