package subagent

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strings"
)

// CleanScope normalises workspace-relative paths: trimmed, slash-separated,
// cleaned, no absolute paths, nothing that climbs out, no run of two spaces
// (the task document separates trailing fields with two spaces).
func CleanScope(paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		s := strings.TrimSpace(p)
		if s == "" {
			return nil, errors.New("empty scope entry")
		}
		if strings.Contains(s, "  ") {
			return nil, fmt.Errorf("scope %q contains two consecutive spaces", s)
		}
		if filepath.IsAbs(s) || strings.HasPrefix(s, "/") {
			return nil, fmt.Errorf("scope %q is absolute; use a workspace-relative path", s)
		}
		c := path.Clean(filepath.ToSlash(s))
		if c == "." || c == ".." || strings.HasPrefix(c, "../") {
			return nil, fmt.Errorf("scope %q escapes the workspace", s)
		}
		out = append(out, c)
	}
	return out, nil
}

// InScope reports whether rel (slash-separated, workspace-relative) is one
// of the scope entries or under one of them.
func InScope(scope []string, rel string) bool {
	rel = path.Clean(filepath.ToSlash(rel))
	for _, s := range scope {
		if rel == s || strings.HasPrefix(rel, s+"/") {
			return true
		}
	}
	return false
}

// Within reports whether every entry of scope lies inside max. An empty
// max is the whole workspace.
func Within(scope, max []string) bool {
	if len(max) == 0 {
		return true
	}
	for _, s := range scope {
		if !InScope(max, s) {
			return false
		}
	}
	return true
}

// Overlap reports whether any entry of a contains or is contained by an
// entry of b.
func Overlap(a, b []string) bool {
	for _, x := range a {
		if InScope(b, x) {
			return true
		}
	}
	for _, y := range b {
		if InScope(a, y) {
			return true
		}
	}
	return false
}

// LaneKey is the server a base_url names: scheme, host and port, folded to
// lower case. Two provider entries on the same server share a lane.
func LaneKey(baseURL string) string {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Host == "" {
		return baseURL
	}
	return strings.ToLower(u.Scheme + "://" + u.Host)
}
