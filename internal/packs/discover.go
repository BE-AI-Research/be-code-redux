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

	"github.com/brown-enterprises/be-code/internal/verify"
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
	matched, _ := p.MatchesDetail(workspace)
	return matched
}

// MatchesDetail is Matches, and also says whether the walk gave up before it
// had seen the whole tree.
//
// That second answer matters because the limits are reachable: a monorepo, or
// an Unreal project whose Content/ holds thirty thousand .uasset files (not in
// skipDir), exhausts walkLimit before a "**/*.tsx" glob has been anywhere
// interesting. Nothing recorded it, so /packs said "does not match this
// workspace" — which may simply be untrue, and a person could not tell a pack
// that does not apply from one that would have.
//
// It also stops as soon as the verdict is decided rather than walking the
// rest of the tree for nothing, which is what made the limit reachable in the
// first place and cost up to walkTime per pack at startup.
func (p *Pack) MatchesDetail(workspace string) (matched, truncated bool) {
	if len(p.detect.Any) == 0 && len(p.detect.All) == 0 {
		return false, false
	}
	// One walk answers every glob. Compiling first means a glob that cannot
	// be compiled simply never matches, rather than aborting detection.
	anyM := compileGlobs(p.detect.Any)
	allM := compileGlobs(p.detect.All)

	deadline := time.Now().Add(walkTime)
	seen := 0
	anyHit := make([]bool, len(anyM))
	allHit := make([]bool, len(allM))

	// decided reports whether the remaining files can still change the
	// answer: one "any" hit settles it, and so does every "all" glob hit.
	decided := func() bool {
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
			truncated = true
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
		if decided() {
			return fs.SkipAll
		}
		return nil
	})

	return decided(), truncated
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

// MatchesAnyGlob reports whether any of the given workspace-relative paths
// matches any of the globs. It is the matcher a check's when.changed field is
// read with, exported because the decision belongs where the changed-file
// list is known — the agent — and this package cannot import that one.
//
// Glob semantics are the same as detection's: "*" does not cross a separator
// and "**/" also matches no directory at all, so "**/*.tsx" finds a file at
// the root as well as a nested one. An empty glob list matches nothing, which
// is why the caller treats "no globs" as unconditional before calling.
func MatchesAnyGlob(globs, paths []string) bool {
	res := compileGlobs(globs)
	for _, p := range paths {
		slash := filepath.ToSlash(p)
		for _, re := range res {
			if re != nil && re.MatchString(slash) {
				return true
			}
		}
	}
	return false
}

// Checks is the verify commands of every installed pack that matches this
// workspace, for a caller with no agent: `be-code verify`.
//
// A global pack is the person's own, so its checks run. A project pack's need
// the trust decision — and since a pack can now be installed into a project
// deliberately (design §11), a recorded approval for exactly this content is
// that decision, readable here without an agent. One that has no approval is
// named in the notes rather than run unapproved or silently dropped.
//
// trustPath is a parameter rather than computed here so this package does not
// import internal/engine and stays a leaf; its caller knows the workspace and
// can build it.
func Checks(packsDir, workspace, trustPath string) (checks []verify.Check, notes []string) {
	found, warns := Discover(packsDir, workspace)
	notes = append(notes, warns...)
	for _, p := range found {
		// The same precedence the session uses (§7a), not detection alone: a
		// pack installed into this project is wanted even with no detect
		// block, and refusing it here made `be-code verify` disagree with
		// what the session runs.
		if !Wanted(p, workspace, trustPath) {
			continue
		}
		if p.Source != SourceGlobal && !approvedHere(p, trustPath) {
			notes = append(notes, fmt.Sprintf("pack %q came with this project and has not been approved here, "+
				"so its checks are not run; run be-code interactively once to approve it", p.Name))
			continue
		}
		if over, est, budget := CoreOverBudget(p); over {
			notes = append(notes, fmt.Sprintf("pack %q is not active: its core is about %d tokens against a budget of %d", p.Name, est, budget))
			continue
		}
		checks = append(checks, p.Checks(workspace)...)
	}
	return checks, notes
}

// approvedHere reports whether this exact pack content is approved for this
// workspace. An unreadable pack, or nowhere to read approvals from, is not an
// approval: this decides whether to run somebody else's commands.
func approvedHere(p *Pack, trustPath string) bool {
	if trustPath == "" {
		return false
	}
	hash, err := ContentHash(p.Root)
	if err != nil {
		return false
	}
	return hash != "" && TrustedHash(trustPath, p.Name) == hash
}
