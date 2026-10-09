package packs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// MarkerName is the provenance file an installed pack carries, committed with
// the pack. It records where the pack came from, and it is the project's
// declaration that it wants the pack (design §7a).
//
// It deliberately holds no checksum of the pack. ContentHash covers every
// non-prose file at the pack root, this file included, so a checksum here
// would be circular — and leaving it out is stronger: editing the marker to
// forge a provenance claim changes the hash and voids the approval.
const MarkerName = ".pack-source.json"

// Marker is MarkerName's contents. Ref is what a person asked for and Commit
// is what they got: `packs update` needs the first, a listing shows the second.
type Marker struct {
	Pack      string `json:"pack"`
	Version   string `json:"version,omitempty"`
	Source    string `json:"source"`
	Ref       string `json:"ref,omitempty"`
	Commit    string `json:"commit,omitempty"`
	Installed string `json:"installed,omitempty"`
}

// Now is the clock the installer stamps a marker with. A test replaces it.
var Now = func() time.Time { return time.Now().UTC() }

// ReadMarker reads a pack's marker. The bool is "there is one and it parsed";
// a missing or corrupt marker is simply not a marker, never an error, because
// every caller's next move is the same either way.
func ReadMarker(packRoot string) (Marker, bool) {
	b, err := os.ReadFile(filepath.Join(packRoot, MarkerName))
	if err != nil {
		return Marker{}, false
	}
	var m Marker
	if err := json.Unmarshal(b, &m); err != nil {
		return Marker{}, false
	}
	return m, true
}

// WriteMarker writes a pack's marker, indented so a person reading the
// committed file can see what it says.
func WriteMarker(packRoot string, m Marker) error {
	if m.Installed == "" {
		m.Installed = Now().Format(time.RFC3339)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(packRoot, MarkerName), append(b, '\n'), 0o644)
}

// DeclaresWanted reports whether this marker is a declaration that the named
// pack is wanted in this project. It must name the pack it sits beside: a
// marker naming something else is somebody else's, copied in by hand.
//
// Wanting is not trust. A pack this returns true for still raises pack_trust
// before any of its rules reach the system prompt, so a repository can move a
// pack from "inactive" to "asks you" and never to "active".
func (m Marker) DeclaresWanted(pack string) bool {
	return pack != "" && m.Pack == pack
}
