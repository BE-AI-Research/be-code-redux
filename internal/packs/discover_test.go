package packs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeMinimalPack(t *testing.T, dir, name, detect string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"schema":1,"name":"` + name + `","version":"1.0.0","core":{"file":"core.md"}` + detect + `}`
	if err := os.WriteFile(filepath.Join(dir, "pack.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "core.md"), []byte("rules for "+name), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverReadsTheSearchPath(t *testing.T) {
	home, ws := t.TempDir(), t.TempDir()
	writeMinimalPack(t, filepath.Join(home, "packs", "ue5"), "ue5", `,"detect":{"any":["*.uproject"]}`)
	writeMinimalPack(t, filepath.Join(home, "packs", "godot"), "godot", `,"detect":{"any":["project.godot"]}`)

	found, warns := Discover(filepath.Join(home, "packs"), ws)
	if len(warns) != 0 {
		t.Fatalf("warnings: %v", warns)
	}
	if len(found) != 2 || found[0].Name != "godot" || found[1].Name != "ue5" {
		t.Fatalf("want both packs, sorted: %v", names(found))
	}
	for _, p := range found {
		if p.Source != SourceGlobal {
			t.Errorf("%s: source = %q", p.Name, p.Source)
		}
	}
}

// A pack in the project shadows a global one of the same name, and the fact
// is reported: the two may say very different things, and which one is in
// force is not something to discover by surprise.
func TestProjectPackShadowsGlobalAndSaysSo(t *testing.T) {
	home, ws := t.TempDir(), t.TempDir()
	writeMinimalPack(t, filepath.Join(home, "packs", "ue5"), "ue5", "")
	writeMinimalPack(t, filepath.Join(ws, ".becode", "packs", "ue5"), "ue5", "")

	found, warns := Discover(filepath.Join(home, "packs"), ws)
	if len(found) != 1 {
		t.Fatalf("want one pack after shadowing, got %v", names(found))
	}
	if found[0].Source != SourceProject {
		t.Errorf("the project pack must win, got %q", found[0].Source)
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "shadow") {
		t.Fatalf("shadowing must be announced: %v", warns)
	}
}

// One unusable pack must not stop the others from loading.
func TestDiscoverSkipsABrokenPack(t *testing.T) {
	home, ws := t.TempDir(), t.TempDir()
	writeMinimalPack(t, filepath.Join(home, "packs", "good"), "good", "")
	bad := filepath.Join(home, "packs", "bad")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "pack.json"), []byte(`{"schema":99,"name":"bad"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A directory with no manifest at all is not a pack and is not an error.
	if err := os.MkdirAll(filepath.Join(home, "packs", "notapack"), 0o755); err != nil {
		t.Fatal(err)
	}

	found, warns := Discover(filepath.Join(home, "packs"), ws)
	if len(found) != 1 || found[0].Name != "good" {
		t.Fatalf("got %v", names(found))
	}
	if len(warns) != 1 || !strings.Contains(warns[0], "bad") {
		t.Fatalf("warnings = %v", warns)
	}
}

func TestDiscoverIsQuietWithNoPacksAtAll(t *testing.T) {
	found, warns := Discover(filepath.Join(t.TempDir(), "nothing"), t.TempDir())
	if len(found) != 0 || len(warns) != 0 {
		t.Fatalf("found=%v warns=%v", names(found), warns)
	}
}

func TestMatchesWorkspace(t *testing.T) {
	ws := t.TempDir()
	for _, f := range []string{"MyGame.uproject", "Source/MyGame/MyGame.Build.cs", "README.md"} {
		full := filepath.Join(ws, filepath.FromSlash(f))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cases := []struct {
		name   string
		detect string
		want   bool
	}{
		{"any matches a root file", `{"any":["*.uproject"]}`, true},
		{"any with one hit of several", `{"any":["project.godot","*.uproject"]}`, true},
		{"any matches nothing", `{"any":["project.godot","*.sln"]}`, false},
		{"a nested path needs **", `{"any":["**/*.Build.cs"]}`, true},
		{"* does not cross a separator", `{"any":["*.Build.cs"]}`, false},
		{"all requires every glob", `{"all":["*.uproject","**/*.Build.cs"]}`, true},
		{"all fails on one miss", `{"all":["*.uproject","project.godot"]}`, false},
		// A pack with no detect block never activates on its own: it is a
		// general-purpose pack somebody turns on deliberately.
		{"no detect never matches", `{}`, false},
	}
	for _, tc := range cases {
		dir := filepath.Join(t.TempDir(), "p")
		writeMinimalPack(t, dir, "p", `,"detect":`+tc.detect)
		p, _, err := Load(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := p.Matches(ws); got != tc.want {
			t.Errorf("%s: Matches() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func names(ps []*Pack) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name+"("+string(p.Source)+")")
	}
	return out
}
