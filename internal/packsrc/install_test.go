package packsrc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/brown-enterprises/be-code/internal/packs"
)

func opts(t *testing.T, yes bool) (Options, string) {
	t.Helper()
	ws := t.TempDir()
	return Options{
		Workspace: ws,
		TrustPath: filepath.Join(t.TempDir(), "packs.json"),
		Approve:   func(string, string) bool { return yes },
	}, ws
}

func TestInstallLandsInTheProjectAndRecordsTrust(t *testing.T) {
	src := packsRepo(t, "assist")
	s, _ := Resolve(src, "")
	o, ws := opts(t, true)

	res, err := Install(context.Background(), s, "assist", o)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(ws, ".becode", "packs", "assist")
	if res.Dir != want {
		t.Errorf("installed at %q, want %q", res.Dir, want)
	}
	if _, err := os.Stat(filepath.Join(want, "core.md")); err != nil {
		t.Errorf("the pack is not there: %v", err)
	}
	// Trust is recorded, so no prompt follows the person's own install.
	hash, err := packs.ContentHash(want)
	if err != nil {
		t.Fatal(err)
	}
	if got := packs.TrustedHash(o.TrustPath, "assist"); got != hash {
		t.Errorf("trusted hash = %q, want the installed content's %q", got, hash)
	}
	// Enablement comes from the marker, so nothing per-workspace is written.
	if _, set := packs.Forced(o.TrustPath, "assist"); set {
		t.Error("install wrote a Forced entry; the marker is what declares the pack wanted")
	}
}

func TestInstallWritesTheMarker(t *testing.T) {
	src, head := gitPacksRepo(t, "assist")
	s, _ := Resolve("file://"+src, "main")
	o, ws := opts(t, true)

	if _, err := Install(context.Background(), s, "assist", o); err != nil {
		t.Fatal(err)
	}
	m, ok := packs.ReadMarker(filepath.Join(ws, ".becode", "packs", "assist"))
	if !ok {
		t.Fatal("no marker was written")
	}
	if m.Pack != "assist" || m.Ref != "main" || m.Commit != head {
		t.Errorf("marker = %+v, want pack=assist ref=main commit=%s", m, head)
	}
	if m.Source != "file://"+src {
		t.Errorf("marker source = %q, want the source as given", m.Source)
	}
}

func TestInstallRefusedWritesNothing(t *testing.T) {
	src := packsRepo(t, "assist")
	s, _ := Resolve(src, "")
	o, ws := opts(t, false)

	if _, err := Install(context.Background(), s, "assist", o); err == nil {
		t.Fatal("a refused install succeeded")
	}
	if _, err := os.Stat(filepath.Join(ws, ".becode", "packs", "assist")); !os.IsNotExist(err) {
		t.Error("a refused install left something behind")
	}
	if got := packs.TrustedHash(o.TrustPath, "assist"); got != "" {
		t.Error("a refused install recorded a trust")
	}
}

// Review Focus 5: nothing half-installed, ever.
func TestAnInvalidPackIsNotInstalled(t *testing.T) {
	src := packsRepo(t, "assist")
	// A core far over its declared budget: the loader refuses it.
	big := strings.Repeat("rule after rule, and then some more of them. ", 500)
	mustWrite(t, filepath.Join(src, "packs", "assist", "core.md"), big, 0o644)
	s, _ := Resolve(src, "")
	o, ws := opts(t, true)

	_, err := Install(context.Background(), s, "assist", o)
	if err == nil {
		t.Fatal("a pack the loader would refuse was installed")
	}
	if _, serr := os.Stat(filepath.Join(ws, ".becode", "packs", "assist")); !os.IsNotExist(serr) {
		t.Error("a refused pack left a directory behind; the loader would read it")
	}
}

func TestInstallRefusesToClobberWithoutForce(t *testing.T) {
	src := packsRepo(t, "assist")
	s, _ := Resolve(src, "")
	o, _ := opts(t, true)
	if _, err := Install(context.Background(), s, "assist", o); err != nil {
		t.Fatal(err)
	}
	_, err := Install(context.Background(), s, "assist", o)
	if err == nil {
		t.Fatal("a second install overwrote the first silently")
	}
	if !strings.Contains(err.Error(), "update") {
		t.Errorf("the error should point at packs update: %v", err)
	}
	o.Force = true
	if _, err := Install(context.Background(), s, "assist", o); err != nil {
		t.Errorf("--force should replace it: %v", err)
	}
}

func TestUpdateMovesToTheNewCommitAndRetrusts(t *testing.T) {
	src, first := gitPacksRepo(t, "assist")
	s, _ := Resolve("file://"+src, "main")
	o, ws := opts(t, true)
	if _, err := Install(context.Background(), s, "assist", o); err != nil {
		t.Fatal(err)
	}

	mustWrite(t, filepath.Join(src, "packs", "assist", "core.md"), "Revised rules.\n", 0o644)
	git(t, src, "add", "-A")
	git(t, src, "commit", "--quiet", "-m", "revise")
	second := trim(git(t, src, "rev-parse", "HEAD"))

	res, err := Update(context.Background(), "assist", o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Previous != first || res.Commit != second {
		t.Errorf("moved %q→%q, want %q→%q", res.Previous, res.Commit, first, second)
	}
	body, _ := os.ReadFile(filepath.Join(ws, ".becode", "packs", "assist", "core.md"))
	if !strings.Contains(string(body), "Revised rules") {
		t.Error("the pack was not replaced")
	}
	hash, _ := packs.ContentHash(filepath.Join(ws, ".becode", "packs", "assist"))
	if packs.TrustedHash(o.TrustPath, "assist") != hash {
		t.Error("update did not re-record trust for the new content")
	}
}

func TestUpdateRefusesAPackWithNoMarker(t *testing.T) {
	o, ws := opts(t, true)
	dir := filepath.Join(ws, ".becode", "packs", "arrived")
	packFiles(t, dir, "arrived", "0.1.0")

	_, err := Update(context.Background(), "arrived", o)
	if err == nil {
		t.Fatal("updating a pack with no provenance succeeded")
	}
	if !strings.Contains(err.Error(), packs.MarkerName) {
		t.Errorf("the error should say the marker is missing: %v", err)
	}
}

func TestRemoveDeletesThePackAndItsState(t *testing.T) {
	src := packsRepo(t, "assist")
	s, _ := Resolve(src, "")
	o, ws := opts(t, true)
	if _, err := Install(context.Background(), s, "assist", o); err != nil {
		t.Fatal(err)
	}
	if err := packs.SetForced(o.TrustPath, "assist", false); err != nil {
		t.Fatal(err)
	}

	if err := Remove("assist", o); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".becode", "packs", "assist")); !os.IsNotExist(err) {
		t.Error("the pack directory is still there")
	}
	if got := packs.TrustedHash(o.TrustPath, "assist"); got != "" {
		t.Errorf("a trust for content that is gone was left behind: %q", got)
	}
	if _, set := packs.Forced(o.TrustPath, "assist"); set {
		t.Error("a Forced entry for a pack that is gone was left behind")
	}
}

func TestRemoveRefusesANameThatEscapes(t *testing.T) {
	o, _ := opts(t, true)
	if err := Remove("../../etc", o); err == nil {
		t.Fatal("a name that escapes the packs directory was removed")
	}
}

func TestRemoveOfSomethingAbsentSaysSo(t *testing.T) {
	o, _ := opts(t, true)
	if err := Remove("assist", o); err == nil {
		t.Fatal("removing a pack that is not installed succeeded")
	}
}

// §12: "There is no state in which .becode/packs/<name>/ holds half a pack."
// The --force path removed the destination before moving the new pack in, and
// RemoveAll is not atomic, so a failure part-way left the pack gutted —
// demonstrated: pack.json, core.md and the marker all vanished. Update sets
// Force, so this was the ordinary `packs update` path, and on Windows a file
// an editor or a scanner holds open makes RemoveAll fail routinely.
//
// The invariant is about the destination, not about who wins: whether the
// replace succeeds or fails, .becode/packs/<name> must hold a complete pack.
func TestForceNeverLeavesHalfAPack(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	src := packsRepo(t, "p")
	s, _ := Resolve(src, "")
	o, ws := opts(t, true)
	if _, err := Install(context.Background(), s, "p", o); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(ws, ".becode", "packs", "p")

	// A subdirectory whose contents cannot be unlinked: RemoveAll fails here.
	sub := filepath.Join(dest, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "x"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	// Whatever path that directory ends up at, make it removable again so
	// t.TempDir's own cleanup can finish.
	t.Cleanup(func() {
		filepath.WalkDir(filepath.Join(ws, ".becode", "packs"), func(p string, d os.DirEntry, err error) error {
			if err == nil && d.IsDir() {
				os.Chmod(p, 0o755)
			}
			return nil
		})
	})

	o.Force = true
	_, ierr := Install(context.Background(), s, "p", o)

	// Either outcome is acceptable; a half-pack at dest is not.
	for _, must := range []string{"pack.json", "core.md"} {
		if _, err := os.Stat(filepath.Join(dest, must)); err != nil {
			t.Errorf("%s is missing after a force replace (install err: %v): the destination holds half a pack", must, ierr)
		}
	}
	if _, ok := packs.ReadMarker(dest); !ok {
		t.Errorf("the marker is missing after a force replace (install err: %v)", ierr)
	}
	if _, _, err := packs.Load(dest); err != nil {
		t.Errorf("what is at dest is not a loadable pack (install err: %v): %v", ierr, err)
	}
}

// `packs add -C /typo` must not create /typo/.becode/packs and install there:
// a workspace that does not exist is a mistyped path, not a project.
func TestInstallRefusesAWorkspaceThatDoesNotExist(t *testing.T) {
	src := packsRepo(t, "assist")
	ws := filepath.Join(t.TempDir(), "typo")
	_, err := Install(context.Background(), Source{Spec: src, Kind: KindPath}, "assist",
		Options{Workspace: ws, Approve: func(string, string) bool { return true }})
	if err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("want a refusal naming the missing workspace, got %v", err)
	}
	if _, serr := os.Stat(ws); !os.IsNotExist(serr) {
		t.Error("the missing workspace was created")
	}
}
