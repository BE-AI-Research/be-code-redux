package packsrc

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/brown-enterprises/be-code/internal/packs"
)

// Options is everything an install needs from its caller. Approve is the
// person's seam: a nil Approve is a refusal, never a silent yes, because an
// install reaches the network and then writes rules into the model's own
// system prompt.
type Options struct {
	Workspace string // the project root
	TrustPath string // ~/.be-code/engine/<key>/packs.json
	Approve   func(action, detail string) bool
	Force     bool // replace a pack that is already installed
}

// Result is what an install or update did.
type Result struct {
	Pack     string
	Version  string
	Commit   string
	Dir      string
	Previous string // the commit replaced, for an update
}

// PacksDir is where a project's packs live. Note the dot-directory: the task
// engine uses .be-code, packs and custom commands use .becode, and a pack
// written to the wrong one is never found.
func PacksDir(workspace string) string {
	return filepath.Join(workspace, ".becode", "packs")
}

func packPath(workspace, pack string) (string, error) {
	if err := ValidPackName(pack); err != nil {
		return "", err
	}
	return filepath.Join(PacksDir(workspace), pack), nil
}

// Install fetches a pack and puts it in the project.
//
// The order is fixed and load-bearing: approve, fetch to a temp directory,
// validate it there, move it into place, write the marker, hash the result,
// record the trust. Validating before the move is what keeps a pack the
// loader would refuse from ever existing at its final path; hashing after the
// marker is what puts the marker inside the hash.
func Install(ctx context.Context, s Source, pack string, o Options) (Result, error) {
	if st, err := os.Stat(o.Workspace); err != nil || !st.IsDir() {
		// A mistyped -C is a path, not a project: creating /typo/.becode/packs
		// and installing there, silently, keyed the trust file on the typo.
		return Result{}, fmt.Errorf("workspace %s does not exist; a pack is installed into an existing project", o.Workspace)
	}
	detail := installDetail(s, pack, o.Workspace)
	if o.Approve == nil || !o.Approve("pack_install", detail) {
		return Result{}, fmt.Errorf("install declined")
	}
	got, err := Fetch(ctx, s, pack)
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(filepath.Dir(got.Dir))

	if err := validate(got.Dir); err != nil {
		return Result{}, err
	}
	dest, err := packPath(o.Workspace, got.Pack)
	if err != nil {
		return Result{}, err
	}
	previous := ""
	aside := ""
	if _, err := os.Stat(dest); err == nil {
		if !o.Force {
			return Result{}, fmt.Errorf("%s is already installed at %s; use packs update to move it, or --force to replace it",
				got.Pack, dest)
		}
		if m, ok := packs.ReadMarker(dest); ok {
			previous = m.Commit
		}
		// Move the old pack aside rather than deleting it: RemoveAll is not
		// atomic, so removing first left the pack gutted whenever it failed
		// part-way — an undeletable file, or on Windows one an editor or a
		// scanner is holding open. Update sets Force, so this is the ordinary
		// `packs update` path and §12's "never half a pack" has to hold here.
		aside = dest + ".replacing-" + packs.Now().Format("20060102-150405")
		if err := os.Rename(dest, aside); err != nil {
			return Result{}, fmt.Errorf("could not move the installed %s aside to replace it: %w", got.Pack, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return Result{}, restore(aside, dest, err)
	}
	if err := move(got.Dir, dest); err != nil {
		return Result{}, restore(aside, dest, err)
	}
	if aside != "" {
		// The new pack is in place; the old one is now just litter. A failure
		// here is not worth failing the install over, but it must be visible
		// rather than leaving a mystery directory behind.
		if rerr := os.RemoveAll(aside); rerr != nil {
			fmt.Fprintf(os.Stderr, "warn: the previous %s is left at %s (%v)\n", got.Pack, aside, rerr)
		}
	}
	m := packs.Marker{
		Pack: got.Pack, Version: got.Version,
		Source: s.Spec, Ref: s.Ref, Commit: got.Commit,
		Installed: packs.Now().Format(time.RFC3339),
	}
	// Past this point the pack is at its final path. A failure below leaves it
	// there without a marker or an approval — it fails safe, since neither
	// is inferred — but the error has to say a directory now exists.
	if err := packs.WriteMarker(dest, m); err != nil {
		return Result{}, fmt.Errorf("%s is at %s but its marker could not be written (%w); "+
			"it is not approved and packs update cannot see where it came from: remove it, or re-run with --force", got.Pack, dest, err)
	}
	if err := record(dest, got.Pack, o.TrustPath); err != nil {
		return Result{}, fmt.Errorf("%s is at %s but its approval could not be recorded (%w); "+
			"a session will ask before using it, or re-run with --force", got.Pack, dest, err)
	}
	return Result{Pack: got.Pack, Version: got.Version, Commit: got.Commit, Dir: dest, Previous: previous}, nil
}

// Update re-fetches an installed pack at the ref its marker records.
func Update(ctx context.Context, pack string, o Options) (Result, error) {
	dest, err := packPath(o.Workspace, pack)
	if err != nil {
		return Result{}, err
	}
	if _, serr := os.Stat(dest); serr != nil {
		return Result{}, fmt.Errorf("%s is not installed in this project", pack)
	}
	m, ok := packs.ReadMarker(dest)
	if !ok || m.Source == "" {
		return Result{}, fmt.Errorf("%s has no %s, so there is nothing to say where it came from; "+
			"a pack that arrived with the project is updated by whoever maintains it", pack, packs.MarkerName)
	}
	s, err := Resolve(m.Source, m.Ref)
	if err != nil {
		return Result{}, err
	}
	o.Force = true
	return Install(ctx, s, pack, o)
}

// Remove deletes a pack from the project and forgets its per-workspace state.
// os.RemoveAll is rm -rf's semantics without a shell: Windows has no rm, and
// putting a path through a shell adds an injection surface for no gain.
func Remove(pack string, o Options) error {
	dest, err := packPath(o.Workspace, pack)
	if err != nil {
		return err
	}
	if _, serr := os.Stat(dest); serr != nil {
		return fmt.Errorf("%s is not installed in this project", pack)
	}
	if err := os.RemoveAll(dest); err != nil {
		return err
	}
	if o.TrustPath == "" {
		return nil
	}
	// A trust for content that is gone, or a switch for a pack that is gone,
	// would be inherited by a later reinstall or by a different pack that
	// happens to share the name.
	if err := packs.Untrust(o.TrustPath, pack); err != nil {
		return err
	}
	return packs.ClearForced(o.TrustPath, pack)
}

// validate is what `be-code packs validate` runs: a pack the loader would
// refuse must never reach its final path.
func validate(dir string) error {
	p, _, err := packs.Load(dir)
	if err != nil {
		return fmt.Errorf("not a usable pack: %w", err)
	}
	if over, est, budget := packs.CoreOverBudget(p); over {
		return fmt.Errorf("not installed: %s's core is about %d tokens against a budget of %d", p.Name, est, budget)
	}
	return nil
}

// record stamps the install as consent: the person asked for this pack, so
// the trust gate must not ask again for this exact content.
func record(dest, pack, trustPath string) error {
	if trustPath == "" {
		return nil
	}
	hash, err := packs.ContentHash(dest)
	if err != nil {
		return err
	}
	return packs.Trust(trustPath, pack, hash)
}

// move puts the validated pack at its final path in one step where it can,
// and falls back to copy-then-remove across filesystems, which a temp
// directory on another mount makes ordinary rather than exotic.
func move(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := copyTree(src, dst); err != nil {
		os.RemoveAll(dst)
		return err
	}
	return os.RemoveAll(src)
}

// restore puts the pack that was moved aside back, so a failed replace leaves
// the project exactly as it was. The returned error is the original cause,
// with the restore's own failure appended if it could not be undone — that
// second case is the one a person has to know about, because the pack is then
// only at the aside path.
func restore(aside, dest string, cause error) error {
	if aside == "" {
		return cause
	}
	os.RemoveAll(dest)
	if err := os.Rename(aside, dest); err != nil {
		return fmt.Errorf("%w; and the pack that was installed could not be put back, it is at %s (%v)", cause, aside, err)
	}
	return cause
}

func installDetail(s Source, pack, workspace string) string {
	name := pack
	if name == "" {
		name = "(the pack this source offers)"
	}
	ref := s.Ref
	if ref == "" {
		ref = "the default branch"
	}
	what := "copy from"
	if s.Kind == KindGit {
		what = "fetch over the network from"
	}
	return fmt.Sprintf("install the skill pack %q\n  %s %s\n  at %s\n  into %s\n\n"+
		"A pack's rules go into the model's system prompt and its checks into the verification loop.",
		name, what, s.Spec, ref, filepath.Join(PacksDir(workspace), name))
}
