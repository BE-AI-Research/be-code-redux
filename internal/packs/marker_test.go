package packs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMarkerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := Marker{
		Pack: "assist", Version: "0.1.0",
		Source:    "https://github.com/BE-AI-Research/be-skill-packs",
		Ref:       "main",
		Commit:    "45f1be8c9e2a1d4f6b8c0a3e5d7f9b1c2e4a6d80",
		Installed: "2026-10-09T16:12:04Z",
	}
	if err := WriteMarker(dir, want); err != nil {
		t.Fatal(err)
	}
	got, ok := ReadMarker(dir)
	if !ok {
		t.Fatal("ReadMarker said there was no marker")
	}
	if got != want {
		t.Errorf("round trip changed it:\n got %+v\nwant %+v", got, want)
	}
}

func TestNoMarkerIsNotAnError(t *testing.T) {
	if _, ok := ReadMarker(t.TempDir()); ok {
		t.Error("an empty directory reported a marker")
	}
}

func TestUnparsableMarkerIsNotAMarker(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, MarkerName), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadMarker(dir); ok {
		t.Error("a corrupt marker was accepted; a pack must not activate on one")
	}
}

// The marker is a declaration of intent, so it must say which pack it is
// about. One naming a different pack is somebody else's marker, copied in.
func TestDeclaresWantedRequiresTheSameName(t *testing.T) {
	m := Marker{Pack: "assist"}
	if !m.DeclaresWanted("assist") {
		t.Error("a marker naming this pack should declare it wanted")
	}
	if m.DeclaresWanted("ue5") {
		t.Error("a marker naming another pack must not declare this one wanted")
	}
	if (Marker{}).DeclaresWanted("assist") {
		t.Error("a marker with no pack name declares nothing")
	}
}

func TestUntrustForgetsOnlyThatPack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "packs.json")
	if err := Trust(path, "assist", "hash-a"); err != nil {
		t.Fatal(err)
	}
	if err := Trust(path, "ue5", "hash-b"); err != nil {
		t.Fatal(err)
	}
	if err := Untrust(path, "assist"); err != nil {
		t.Fatal(err)
	}
	if got := TrustedHash(path, "assist"); got != "" {
		t.Errorf("assist is still trusted: %q", got)
	}
	if got := TrustedHash(path, "ue5"); got != "hash-b" {
		t.Errorf("ue5's approval was collateral damage: %q", got)
	}
}

func TestUntrustOnAMissingFileIsNotAnError(t *testing.T) {
	if err := Untrust(filepath.Join(t.TempDir(), "none.json"), "assist"); err != nil {
		t.Errorf("forgetting what was never recorded should succeed: %v", err)
	}
}

// ContentHash must cover the marker: that is what makes forging a provenance
// claim void the approval instead of inheriting it.
func TestContentHashCoversTheMarker(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "core.md"), []byte("rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteMarker(dir, Marker{Pack: "p", Source: "a"}); err != nil {
		t.Fatal(err)
	}
	before, err := ContentHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteMarker(dir, Marker{Pack: "p", Source: "b"}); err != nil {
		t.Fatal(err)
	}
	after, err := ContentHash(dir)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Error("editing the marker did not change the content hash, so a forged provenance claim would keep its approval")
	}
}
