package schedule

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"
	"unicode"
)

// Grant is one thing a fired event may do without asking.
type Grant struct {
	Kind  string `json:"kind"`  // shell | write | browser
	Value string `json:"value"` // a shell glob, a workspace path prefix, a host glob
}

func (g Grant) String() string { return g.Kind + ": " + g.Value }

// ParseGrant reads "kind: value".
func ParseGrant(s string) (Grant, error) {
	if strings.IndexFunc(s, unicode.IsControl) >= 0 {
		// A newline would render as another line of schedules.md.
		return Grant{}, fmt.Errorf("a grant is one line of plain text (got %q)", s)
	}
	k, v, ok := strings.Cut(s, ":")
	k, v = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(v)
	if !ok || v == "" {
		return Grant{}, fmt.Errorf("a grant is written kind: value, e.g. shell: go test ./... (got %q)", s)
	}
	switch k {
	case "shell":
		if strings.Trim(v, "* ") == "" {
			return Grant{}, fmt.Errorf("shell: %s would allow every command; name the command", v)
		}
	case "write":
		c := path.Clean(filepath.ToSlash(v))
		if filepath.IsAbs(v) || strings.HasPrefix(c, "/") || c == ".." || strings.HasPrefix(c, "../") {
			return Grant{}, fmt.Errorf("write: %s is outside the workspace", v)
		}
		v = c
	case "browser":
		v = strings.ToLower(v)
		if strings.Trim(v, "*. ") == "" {
			return Grant{}, fmt.Errorf("browser: %s would allow every site; name the host", v)
		}
	default:
		return Grant{}, fmt.Errorf("unknown grant kind %q (use shell, write or browser)", k)
	}
	return Grant{Kind: k, Value: v}, nil
}

// Allowance is what a fired event may do without asking. Empty is valid:
// the event can read, and anything else asks.
type Allowance []Grant

func ParseAllowance(lines []string) (Allowance, error) {
	var a Allowance
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		g, err := ParseGrant(l)
		if err != nil {
			return nil, err
		}
		a = append(a, g)
	}
	return a, nil
}

// ParseStanding reads a standing allowance (config schedules.allow) one
// entry at a time: an entry ParseGrant refuses is reported, with the entry
// named, and left out, while the rest still apply. A config file must never
// stop a session from starting over one bad line.
func ParseStanding(lines []string) (Allowance, []error) {
	var a Allowance
	var errs []error
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		g, err := ParseGrant(l)
		if err != nil {
			errs = append(errs, fmt.Errorf("%q: %v", l, err))
			continue
		}
		a = append(a, g)
	}
	return a, errs
}

func (a Allowance) Values(kind string) []string {
	var out []string
	for _, g := range a {
		if g.Kind == kind {
			out = append(out, g.Value)
		}
	}
	return out
}

// WriteAllowed reports whether a workspace-relative path is under a write grant.
func (a Allowance) WriteAllowed(rel string) bool {
	rel = path.Clean(filepath.ToSlash(rel))
	for _, p := range a.Values("write") {
		if p == "." || rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// HostAllowed reports whether a host matches a browser grant (path.Match globs).
func (a Allowance) HostAllowed(host string) bool {
	host = strings.ToLower(host)
	if host == "" {
		return false
	}
	for _, h := range a.Values("browser") {
		if ok, _ := path.Match(h, host); ok || h == host {
			return true
		}
	}
	return false
}

// Broad reports a write grant over the whole workspace.
func (a Allowance) Broad() bool {
	for _, p := range a.Values("write") {
		if p == "." {
			return true
		}
	}
	return false
}
