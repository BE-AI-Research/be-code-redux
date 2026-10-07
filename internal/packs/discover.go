package packs

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Source says where a pack came from, which decides how much it is trusted.
type Source string

const (
	// SourceGlobal is ~/.be-code/packs: the person put it there themselves.
	SourceGlobal Source = "global"
	// SourceProject is <workspace>/.becode/packs: it arrived with a
	// repository, and a repository is not the person.
	SourceProject Source = "project"
)

// Discover loads every pack on the search path: global first, then the
// project's own, so a project pack shadows a global one of the same name —
// and says so, since the two may give very different advice and which is in
// force should not be a surprise.
//
// A pack that cannot be loaded is named in warnings and skipped. One broken
// pack never stops the others, and none of this ever stops the session.
func Discover(globalDir, workspace string) ([]*Pack, []string) {
	var warns []string
	byName := map[string]*Pack{}

	for _, src := range []struct {
		dir    string
		source Source
	}{
		{globalDir, SourceGlobal},
		{filepath.Join(workspace, ".becode", "packs"), SourceProject},
	} {
		entries, err := os.ReadDir(src.dir)
		if err != nil {
			continue // no packs of this kind; not a problem
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(src.dir, e.Name())
			if _, err := os.Stat(filepath.Join(dir, "pack.json")); err != nil {
				continue // a directory without a manifest is not a pack
			}
			p, w, err := Load(dir)
			warns = append(warns, w...)
			if err != nil {
				warns = append(warns, fmt.Sprintf("pack in %s was not loaded: %v", dir, err))
				continue
			}
			p.Source = src.source
			if prev, ok := byName[p.Name]; ok {
				warns = append(warns, fmt.Sprintf("pack %q in this project shadows the %s one (%s)", p.Name, prev.Source, prev.Root))
			}
			byName[p.Name] = p
		}
	}

	out := make([]*Pack, 0, len(byName))
	for _, p := range byName {
		out = append(out, p)
	}
	// Sorted, so the order a pack's rules reach the system prompt does not
	// depend on directory listing order: the prompt cache pays for churn.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, warns
}

// walkLimit and walkTime bound detection on a large or pathological tree, the
// same way /init's scan is bounded. A detect glob that needs more of the tree
// than this is too broad to be useful anyway.
const (
	walkLimit = 20000
	walkTime  = 2 * time.Second
)

// Matches reports whether this pack's detect globs fit the workspace.
//
// Globs are workspace-relative, with "*" matching within one path element and
// "**" across separators, so "*.uproject" is a file at the root and
// "**/*.Build.cs" is one anywhere. "any" needs one hit, "all" needs every
// glob. A pack with no detect block never matches: it is a general-purpose
// pack somebody turns on deliberately rather than one that recognises a
// project.
func (p *Pack) Matches(workspace string) bool {
	if len(p.detect.Any) == 0 && len(p.detect.All) == 0 {
		return false
	}
	// One walk answers every glob. Compiling first means a glob that cannot
	// be compiled simply never matches, rather than aborting detection.
	anyM := compileGlobs(p.detect.Any)
	allM := compileGlobs(p.detect.All)

	deadline := time.Now().Add(walkTime)
	seen := 0
	anyHit := make([]bool, len(anyM))
	allHit := make([]bool, len(allM))

	filepath.WalkDir(workspace, func(full string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if skipDir(d.Name()) && full != workspace {
				return fs.SkipDir
			}
			return nil
		}
		if seen++; seen > walkLimit || time.Now().After(deadline) {
			return fs.SkipAll
		}
		rel, rerr := filepath.Rel(workspace, full)
		if rerr != nil {
			return nil
		}
		slash := filepath.ToSlash(rel)
		for i, re := range anyM {
			if re != nil && !anyHit[i] && re.MatchString(slash) {
				anyHit[i] = true
			}
		}
		for i, re := range allM {
			if re != nil && !allHit[i] && re.MatchString(slash) {
				allHit[i] = true
			}
		}
		return nil
	})

	for _, h := range anyHit {
		if h {
			return true
		}
	}
	if len(allM) == 0 {
		return false
	}
	for _, h := range allHit {
		if !h {
			return false
		}
	}
	return true
}

// skipDir keeps detection out of the places a project's own files are not,
// the same set /init's scan leaves out.
func skipDir(name string) bool {
	switch name {
	case ".git", ".becode", "node_modules", "vendor", "Intermediate", "Binaries",
		"Saved", "DerivedDataCache", "dist", "build", "target", ".venv", "__pycache__":
		return true
	}
	return false
}

func compileGlobs(globs []string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(globs))
	for i, g := range globs {
		out[i] = compileGlob(g)
	}
	return out
}

// compileGlob turns a detect glob into an anchored regexp. path.Match would
// do for "*.uproject" but has no "**", and a pack needs to say "a Build.cs
// anywhere" without knowing how deep the modules are.
func compileGlob(glob string) *regexp.Regexp {
	glob = path.Clean(filepath.ToSlash(strings.TrimSpace(glob)))
	if glob == "" || glob == "." {
		return nil
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		switch c := glob[i]; c {
		case '*':
			if i+1 < len(glob) && glob[i+1] == '*' {
				i++
				// "**/" also matches no directory at all, so "**/*.cs" finds
				// a file at the root as well as a nested one.
				if i+1 < len(glob) && glob[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
					continue
				}
				b.WriteString(".*")
				continue
			}
			b.WriteString("[^/]*")
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}
