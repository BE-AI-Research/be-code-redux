// Package packsrc fetches a skill pack from somewhere and installs it into a
// project. It is the only code in BE-Code that reaches a pack registry, and
// the only one that runs git for a reason other than reading the workspace.
//
// Direction of dependency: packsrc imports internal/packs, never the reverse.
// The agent needs the marker and the trust file, not the fetcher, and keeping
// it that way is what stops os/exec reaching the agent's import graph.
package packsrc

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// DefaultSource is where a pack comes from when nobody names a source. It is
// a var so a test can point it at a local repository, exactly as
// update.APIBase is.
var DefaultSource = "https://github.com/BE-AI-Research/be-skill-packs"

// Kind is how a source has to be fetched.
type Kind int

const (
	// KindPath is a directory on this machine: copied, no git, no network.
	KindPath Kind = iota
	// KindGit is anything git can clone.
	KindGit
)

func (k Kind) String() string {
	if k == KindPath {
		return "path"
	}
	return "git"
}

// Source is a resolved place to fetch from.
type Source struct {
	Spec string // the directory or the clone URL
	Kind Kind
	Ref  string // branch, tag or commit; "" means the remote's default
}

// scpLike matches git's scp-style remote, user@host:path, which has no scheme
// and so would otherwise look like a bare word.
var scpLike = regexp.MustCompile(`^[^/@:]+@[^/@:]+:.+$`)

// Resolve classifies a source by inspection. It never guesses a bare word
// into a URL: that would contact a host the person did not name.
func Resolve(spec, ref string) (Source, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return Source{Spec: DefaultSource, Kind: KindGit, Ref: ref}, nil
	}
	if strings.HasPrefix(spec, "-") {
		// A URL does not begin with a dash; an option does. Refused here so
		// nothing option-shaped is ever put in git's argument list, whatever
		// clone's own "--" would make of it.
		return Source{}, fmt.Errorf("source %q looks like an option, not a path or a URL", spec)
	}
	if st, err := os.Stat(spec); err == nil && st.IsDir() {
		abs, err := filepath.Abs(spec)
		if err != nil {
			abs = spec
		}
		return Source{Spec: abs, Kind: KindPath, Ref: ref}, nil
	}
	if strings.Contains(spec, "://") || scpLike.MatchString(spec) {
		return Source{Spec: spec, Kind: KindGit, Ref: ref}, nil
	}
	return Source{}, fmt.Errorf("source %q is neither a directory on this machine nor a URL git can clone; "+
		"pass a path, a git URL, or nothing at all to use %s", spec, DefaultSource)
}

// packName is what a pack directory may be called: one path element of
// ordinary characters. The name is joined onto a directory, so anything that
// could leave that directory is refused here, before any path is built.
var packName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidPackName refuses a name that is not a single safe path element.
func ValidPackName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("no pack name given")
	case !packName.MatchString(name):
		// The first character class excludes a dot, so "." and ".." and every
		// hidden name are refused here too.
		return fmt.Errorf("pack name %q is not usable: a pack name is 1-64 characters of letters, "+
			"digits, dot, dash or underscore, starting with a letter or digit", name)
	}
	return nil
}
