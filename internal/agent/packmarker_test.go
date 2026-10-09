package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/brown-enterprises/be-code/internal/packs"
	"github.com/brown-enterprises/be-code/internal/provider"
)

// okProvider is this package's scripted provider with one canned reply. It is
// never called here: LoadPacks does not talk to a model. The helpers are
// newTestAgent (internal/agent/loop_test.go:46) and scriptedProvider
// (internal/agent/agent_test.go:89), both already in this package's tests.
func okProvider() provider.Provider {
	return &scriptedProvider{responses: []provider.ChatResponse{{Content: "ok"}}}
}

// projectPack writes a valid pack with no detect block into the workspace,
// and returns its directory.
func projectPack(t *testing.T, ws, name string) string {
	t.Helper()
	dir := filepath.Join(ws, ".becode", "packs", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pack.json", `{"schema":1,"name":"`+name+`","version":"0.1.0",
  "description":"a pack for tests","core":{"file":"core.md","budget_tokens":1500}}`)
	write("core.md", "Rules for "+name+".\n")
	return dir
}

func rowFor(ag *Agent, name string) (why string, found bool) {
	for _, r := range ag.PackRows() {
		if r.Name == name {
			return r.Why, true
		}
	}
	return "", false
}

// A pack with no detect block and no marker is inactive: today's behaviour,
// and the baseline the next test is measured against.
func TestPackWithoutMarkerIsNotWanted(t *testing.T) {
	ag, ws := newTestAgent(t, okProvider(), nil)
	projectPack(t, ws, "tp")
	ag.LoadPacks()
	why, ok := rowFor(ag, "tp")
	if !ok {
		t.Fatal("the pack was not discovered at all")
	}
	if why != "does not match this workspace" {
		t.Errorf("why = %q, want \"does not match this workspace\"", why)
	}
}

// The marker is the project's declaration that it wants the pack, so it is
// wanted even though nothing detects it.
func TestMarkerDeclaresThePackWanted(t *testing.T) {
	ag, ws := newTestAgent(t, okProvider(), nil)
	dir := projectPack(t, ws, "tp")
	if err := packs.WriteMarker(dir, packs.Marker{Pack: "tp", Source: "x"}); err != nil {
		t.Fatal(err)
	}
	ag.LoadPacks()
	why, _ := rowFor(ag, "tp")
	if why == "does not match this workspace" {
		t.Error("a pack whose marker declares it wanted was treated as unwanted")
	}
	if why != "installed in this project" {
		t.Errorf("why = %q, want \"installed in this project\"", why)
	}
}

// A marker naming another pack is somebody else's, copied in: it declares
// nothing.
func TestMarkerForAnotherPackDeclaresNothing(t *testing.T) {
	ag, ws := newTestAgent(t, okProvider(), nil)
	dir := projectPack(t, ws, "tp")
	if err := packs.WriteMarker(dir, packs.Marker{Pack: "somethingelse", Source: "x"}); err != nil {
		t.Fatal(err)
	}
	ag.LoadPacks()
	why, _ := rowFor(ag, "tp")
	if why != "does not match this workspace" {
		t.Errorf("why = %q: a marker naming another pack must not activate this one", why)
	}
}

// An explicit choice beats the marker, in both directions.
func TestForcedBeatsTheMarker(t *testing.T) {
	ag, ws := newTestAgent(t, okProvider(), nil)
	dir := projectPack(t, ws, "tp")
	if err := packs.WriteMarker(dir, packs.Marker{Pack: "tp", Source: "x"}); err != nil {
		t.Fatal(err)
	}
	path, err := ag.packTrustPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := packs.SetForced(path, "tp", false); err != nil {
		t.Fatal(err)
	}
	ag.LoadPacks()
	if why, _ := rowFor(ag, "tp"); why != "switched off" {
		t.Errorf("why = %q, want \"switched off\": /packs off must beat a marker", why)
	}
}
