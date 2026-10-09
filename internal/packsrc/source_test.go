package packsrc

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveClassifies(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name, spec string
		wantKind   Kind
		wantSpec   string
	}{
		{"empty takes the default", "", KindGit, DefaultSource},
		{"an existing directory is a path", dir, KindPath, dir},
		{"an https url is git", "https://github.com/o/r", KindGit, "https://github.com/o/r"},
		{"an ssh url is git", "ssh://git@host/o/r.git", KindGit, "ssh://git@host/o/r.git"},
		{"scp-style is git", "git@github.com:o/r.git", KindGit, "git@github.com:o/r.git"},
		{"a file url is git", "file:///srv/mirror.git", KindGit, "file:///srv/mirror.git"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := Resolve(c.spec, "")
			if err != nil {
				t.Fatalf("Resolve(%q): %v", c.spec, err)
			}
			if got.Kind != c.wantKind {
				t.Errorf("kind = %v, want %v", got.Kind, c.wantKind)
			}
			if got.Spec != c.wantSpec {
				t.Errorf("spec = %q, want %q", got.Spec, c.wantSpec)
			}
		})
	}
}

// A bare word is a mistake, not a repository. Guessing it into a clone of
// something would contact a host the person never named.
func TestResolveRefusesABareWord(t *testing.T) {
	_, err := Resolve("be-skill-packs", "")
	if err == nil {
		t.Fatal("a bare word was accepted as a source")
	}
	if !strings.Contains(err.Error(), "be-skill-packs") {
		t.Errorf("the error should quote what was given: %v", err)
	}
}

func TestResolveKeepsTheRef(t *testing.T) {
	got, err := Resolve("https://github.com/o/r", "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	if got.Ref != "v1.2.3" {
		t.Errorf("ref = %q, want v1.2.3", got.Ref)
	}
}

// Review Focus 2: the name is joined onto a directory, so it must never be
// able to leave it.
func TestValidPackNameRefusesEscapes(t *testing.T) {
	for _, bad := range []string{
		"", "..", "../etc", "a/b", `a\b`, "/abs", "C:/abs", ".", ".hidden",
		"a..b/../..", strings.Repeat("x", 200),
	} {
		if err := ValidPackName(bad); err == nil {
			t.Errorf("ValidPackName(%q) was accepted", bad)
		}
	}
	for _, ok := range []string{"assist", "ui-web", "ue5", "my_pack", "a1"} {
		if err := ValidPackName(ok); err != nil {
			t.Errorf("ValidPackName(%q) was refused: %v", ok, err)
		}
	}
}

// Belt and braces: whatever ValidPackName allows must still be a single
// element when joined.
func TestValidNamesStayOneElement(t *testing.T) {
	for _, n := range []string{"assist", "ui-web", "ue5"} {
		joined := filepath.Join("/base", n)
		if filepath.Dir(joined) != "/base" {
			t.Errorf("%q escaped its directory: %s", n, joined)
		}
	}
}
